// Copyright 2020 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachinerytypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/pointer"

	mccommon "antrea.io/antrea/multicluster/controllers/multicluster/common"
	agentconfig "antrea.io/antrea/pkg/agent/config"
	"antrea.io/antrea/pkg/agent/openflow"
	ofmock "antrea.io/antrea/pkg/agent/openflow/testing"
	"antrea.io/antrea/pkg/agent/proxy/metrics"
	"antrea.io/antrea/pkg/agent/proxy/types"
	"antrea.io/antrea/pkg/agent/route"
	routemock "antrea.io/antrea/pkg/agent/route/testing"
	binding "antrea.io/antrea/pkg/ovs/openflow"
	k8sproxy "antrea.io/antrea/third_party/proxy"
)

var (
	svc1IPv4              = net.ParseIP("10.20.30.41")
	svc2IPv4              = net.ParseIP("10.20.30.42")
	svc1IPv6              = net.ParseIP("2001::10:20:30:41")
	svc2IPv6              = net.ParseIP("2001::10:20:30:42")
	ep1IPv4               = net.ParseIP("10.180.0.1")
	ep1IPv6               = net.ParseIP("2001::10:180:0:1")
	ep2IPv4               = net.ParseIP("10.180.0.2")
	ep2IPv6               = net.ParseIP("2001::10:180:0:2")
	loadBalancerIPv4      = net.ParseIP("169.254.169.1")
	loadBalancerIPv6      = net.ParseIP("fec0::169:254:169:1")
	svcNodePortIPv4       = net.ParseIP("192.168.77.100")
	svcNodePortIPv6       = net.ParseIP("2001::192:168:77:100")
	externalIPv4          = net.ParseIP("192.168.77.101")
	externalIPv6          = net.ParseIP("2001::192:168:77:101")
	nodePortAddressesIPv4 = []net.IP{svcNodePortIPv4}
	nodePortAddressesIPv6 = []net.IP{svcNodePortIPv6}

	svcPort     = 80
	svcNodePort = 30008
	svcPortName = makeSvcPortName("ns", "svc", strconv.Itoa(svcPort), corev1.ProtocolTCP)

	hostname = "localhostName"

	skippedServiceNN = "kube-system/kube-dns"
	skippedClusterIP = "192.168.1.2"
)

const testServiceProxyName = "antrea"

func makeSvcPortName(namespace, name, port string, protocol corev1.Protocol) k8sproxy.ServicePortName {
	return k8sproxy.ServicePortName{
		NamespacedName: apimachinerytypes.NamespacedName{Namespace: namespace, Name: name},
		Port:           port,
		Protocol:       protocol,
	}
}

func makeServiceMap(proxier *proxier, allServices ...*corev1.Service) {
	for i := range allServices {
		proxier.serviceChanges.OnServiceUpdate(nil, allServices[i])
	}
	proxier.serviceChanges.OnServiceSynced()
}

func makeTestService(namespace, name string, svcFunc func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: map[string]string{},
		},
		Spec:   corev1.ServiceSpec{},
		Status: corev1.ServiceStatus{},
	}
	svcFunc(svc)
	return svc
}

func makeEndpointsMap(proxier *proxier, allEndpoints ...*corev1.Endpoints) {
	for i := range allEndpoints {
		proxier.endpointsChanges.OnEndpointUpdate(nil, allEndpoints[i])
	}
	proxier.endpointsChanges.OnEndpointsSynced()
}

func makeEndpointSliceMap(proxier *proxier, allEndpoints ...*discovery.EndpointSlice) {
	for i := range allEndpoints {
		proxier.endpointsChanges.OnEndpointSliceUpdate(allEndpoints[i], false)
	}
	proxier.endpointsChanges.OnEndpointsSynced()
}

func makeTestEndpointSlice(namespace, svcName string, eps []discovery.Endpoint, ports []discovery.EndpointPort, isIPv6 bool) *discovery.EndpointSlice {
	addrType := discovery.AddressTypeIPv4
	if isIPv6 {
		addrType = discovery.AddressTypeIPv6
	}
	endpointSlice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", svcName, rand.String(5)),
			Namespace: namespace,
			Labels: map[string]string{
				discovery.LabelServiceName: svcName,
			},
		},
	}
	endpointSlice.Endpoints = eps
	endpointSlice.Ports = ports
	endpointSlice.AddressType = addrType
	return endpointSlice
}

func makeTestClusterIPService(svcPortName *k8sproxy.ServicePortName,
	clusterIP net.IP,
	externalIPs []net.IP,
	svcPort int32,
	protocol corev1.Protocol,
	affinitySeconds *int32,
	internalTrafficPolicy *corev1.ServiceInternalTrafficPolicyType,
	nested bool,
	labels map[string]string) *corev1.Service {
	return makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *corev1.Service) {
		svc.Spec.ClusterIP = clusterIP.String()
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:     svcPortName.Port,
			Port:     svcPort,
			Protocol: protocol,
		}}
		for _, ip := range externalIPs {
			if ip != nil {
				svc.Spec.ExternalIPs = append(svc.Spec.ExternalIPs, ip.String())
			}
		}
		if internalTrafficPolicy != nil {
			svc.Spec.InternalTrafficPolicy = internalTrafficPolicy
		}
		if affinitySeconds != nil {
			svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			svc.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{
					TimeoutSeconds: affinitySeconds,
				},
			}
		}
		if nested {
			svc.Annotations = map[string]string{mccommon.AntreaMCServiceAnnotation: "true"}
		}
		if labels != nil {
			svc.Labels = labels
		}
	})
}

func makeTestNodePortService(svcPortName *k8sproxy.ServicePortName,
	clusterIP net.IP,
	externalIPs []net.IP,
	svcPort,
	svcNodePort int32,
	protocol corev1.Protocol,
	affinitySeconds *int32,
	internalTrafficPolicy corev1.ServiceInternalTrafficPolicyType,
	externalTrafficPolicy corev1.ServiceExternalTrafficPolicyType) *corev1.Service {
	return makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *corev1.Service) {
		svc.Spec.ClusterIP = clusterIP.String()
		svc.Spec.Type = corev1.ServiceTypeNodePort
		svc.Spec.Ports = []corev1.ServicePort{{
			NodePort: svcNodePort,
			Name:     svcPortName.Port,
			Port:     svcPort,
			Protocol: protocol,
		}}
		for _, ip := range externalIPs {
			if ip != nil {
				svc.Spec.ExternalIPs = append(svc.Spec.ExternalIPs, ip.String())
			}
		}
		svc.Spec.ExternalTrafficPolicy = externalTrafficPolicy
		svc.Spec.InternalTrafficPolicy = &internalTrafficPolicy
		if affinitySeconds != nil {
			svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			svc.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{
					TimeoutSeconds: affinitySeconds,
				},
			}
		}
	})
}

func makeTestLoadBalancerService(svcPortName *k8sproxy.ServicePortName,
	clusterIP net.IP,
	externalIPs,
	loadBalancerIPs []net.IP,
	svcPort,
	svcNodePort int32,
	protocol corev1.Protocol,
	affinitySeconds *int32,
	internalTrafficPolicy *corev1.ServiceInternalTrafficPolicyType,
	externalTrafficPolicy corev1.ServiceExternalTrafficPolicyType) *corev1.Service {
	return makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *corev1.Service) {
		svc.Spec.ClusterIP = clusterIP.String()
		svc.Spec.Type = corev1.ServiceTypeLoadBalancer
		var ingress []corev1.LoadBalancerIngress
		for _, ip := range loadBalancerIPs {
			if ip != nil {
				ingress = append(ingress, corev1.LoadBalancerIngress{IP: ip.String()})
			}
		}
		svc.Status.LoadBalancer.Ingress = ingress
		for _, ip := range externalIPs {
			if ip != nil {
				svc.Spec.ExternalIPs = append(svc.Spec.ExternalIPs, ip.String())
			}
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			NodePort: svcNodePort,
			Name:     svcPortName.Port,
			Port:     svcPort,
			Protocol: protocol,
		}}
		svc.Spec.ExternalTrafficPolicy = externalTrafficPolicy
		if internalTrafficPolicy != nil {
			svc.Spec.InternalTrafficPolicy = internalTrafficPolicy
		}
		if affinitySeconds != nil {
			svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
			svc.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
				ClientIP: &corev1.ClientIPConfig{
					TimeoutSeconds: affinitySeconds,
				},
			}
		}
	})
}

func makeTestEndpointSubset(svcPortName *k8sproxy.ServicePortName,
	epIP net.IP,
	port int32,
	protocol corev1.Protocol,
	isLocal bool) *corev1.EndpointSubset {
	var nodeName *string
	if isLocal {
		nodeName = &hostname
	}
	return &corev1.EndpointSubset{
		Addresses: []corev1.EndpointAddress{{
			IP:       epIP.String(),
			NodeName: nodeName,
		}},
		Ports: []corev1.EndpointPort{{
			Name:     svcPortName.Port,
			Port:     port,
			Protocol: protocol,
		}},
	}
}

func makeTestEndpointSliceEndpointAndPort(svcPortName *k8sproxy.ServicePortName,
	epIP net.IP,
	port int32,
	protocol corev1.Protocol,
	isLocal bool) (*discovery.Endpoint, *discovery.EndpointPort) {
	ready := true
	var nodeName *string
	if isLocal {
		nodeName = &hostname
	}
	return &discovery.Endpoint{
			Addresses: []string{
				epIP.String(),
			},
			Conditions: discovery.EndpointConditions{
				Ready: &ready,
			},
			Hostname: nodeName,
			NodeName: nodeName,
		}, &discovery.EndpointPort{
			Name:     &svcPortName.Port,
			Port:     &port,
			Protocol: &protocol,
		}
}

func makeTestEndpoints(svcPortName *k8sproxy.ServicePortName, epSubsets []corev1.EndpointSubset) *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcPortName.Name,
			Namespace: svcPortName.Namespace,
		},
		Subsets: epSubsets,
	}
}

type proxyOptions struct {
	proxyAllEnabled      bool
	proxyLoadBalancerIPs bool
	endpointSliceEnabled bool
	supportNestedService bool
	serviceProxyNameSet  bool
}

type proxyOptionsFn func(*proxyOptions)

func withProxyAll(o *proxyOptions) {
	o.proxyAllEnabled = true
}

func withSupportNestedService(o *proxyOptions) {
	o.supportNestedService = true
}

func withoutProxyLoadBalancerIPs(o *proxyOptions) {
	o.proxyLoadBalancerIPs = false
}

func withoutEndpointSlice(o *proxyOptions) {
	o.endpointSliceEnabled = false
}

func withServiceProxyNameSet(o *proxyOptions) {
	o.serviceProxyNameSet = true
}

func getMockClients(ctrl *gomock.Controller) (*ofmock.MockClient, *routemock.MockInterface) {
	mockOFClient := ofmock.NewMockClient(ctrl)
	mockRouteClient := routemock.NewMockInterface(ctrl)
	return mockOFClient, mockRouteClient
}

func newFakeProxier(routeClient route.Interface, ofClient openflow.Client, nodePortAddresses []net.IP, groupIDAllocator openflow.GroupAllocator, isIPv6 bool, options ...proxyOptionsFn) *proxier {
	o := &proxyOptions{
		proxyAllEnabled:      false,
		proxyLoadBalancerIPs: true,
		endpointSliceEnabled: true,
		supportNestedService: false,
		serviceProxyNameSet:  false,
	}

	for _, fn := range options {
		fn(o)
	}
	var serviceProxyName string
	if o.serviceProxyNameSet {
		serviceProxyName = testServiceProxyName
	}
	fakeClient := fake.NewSimpleClientset()
	p, _ := newProxier(hostname,
		serviceProxyName,
		fakeClient,
		informers.NewSharedInformerFactory(fakeClient, 0),
		ofClient,
		isIPv6,
		routeClient,
		nodePortAddresses,
		o.proxyAllEnabled,
		[]string{skippedServiceNN, skippedClusterIP},
		o.proxyLoadBalancerIPs,
		types.NewGroupCounter(groupIDAllocator, make(chan string, 100)), o.supportNestedService)
	p.runner = k8sproxy.NewBoundedFrequencyRunner(componentName, p.syncProxyRules, time.Second, 30*time.Second, 2)
	p.endpointsChanges = newEndpointsChangesTracker(hostname, o.endpointSliceEnabled, isIPv6)
	return p
}

func testClusterIPAdd(t *testing.T,
	svcIP net.IP,
	externalIP net.IP,
	ep1IP net.IP,
	ep2IP net.IP,
	isIPv6 bool,
	nodeLocalInternal bool,
	extraSvcs []*corev1.Service,
	extraEps []*corev1.Endpoints,
	endpointSliceEnabled bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	options := []proxyOptionsFn{withProxyAll}
	if !endpointSliceEnabled {
		options = append(options, withoutEndpointSlice)
	}
	options = append(options, withSupportNestedService)
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6, options...)

	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	if nodeLocalInternal {
		internalTrafficPolicy = corev1.ServiceInternalTrafficPolicyLocal
	}
	var externalIPs []net.IP
	if externalIP != nil {
		externalIPs = append(externalIPs, externalIP)
	}
	allSvcs := append(extraSvcs, makeTestClusterIPService(&svcPortName, svcIP, externalIPs, int32(svcPort), corev1.ProtocolTCP, nil, &internalTrafficPolicy, true, nil))
	makeServiceMap(fp, allSvcs...)

	if !endpointSliceEnabled {
		remoteEpSubset := makeTestEndpointSubset(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
		localEpSubset := makeTestEndpointSubset(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
		allEps := append(extraEps, makeTestEndpoints(&svcPortName, []corev1.EndpointSubset{*remoteEpSubset, *localEpSubset}))
		makeEndpointsMap(fp, allEps...)
	} else {
		remoteEp, remoteEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
		localEp, localEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
		endpointSlice := makeTestEndpointSlice(svcPortName.Namespace,
			svcPortName.Name,
			[]discovery.Endpoint{*remoteEp, *localEp},
			[]discovery.EndpointPort{*remoteEpPort, *localEpPort},
			isIPv6)
		makeEndpointSliceMap(fp, endpointSlice)
	}

	var nodeName string
	var serving bool
	if endpointSliceEnabled {
		nodeName = hostname
		serving = true
	}
	expectedLocalEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep2IP.String(), nodeName, "", svcPort, true, true, serving, false, nil)}
	expectedAllEps := expectedLocalEps
	if !nodeLocalInternal || externalIP != nil {
		expectedAllEps = append(expectedAllEps, k8sproxy.NewBaseEndpointInfo(ep1IP.String(), "", "", svcPort, false, true, serving, false, nil))
	}

	bindingProtocol := binding.ProtocolTCP
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
	}
	internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalInternal)
	var externalGroupID, clusterGroupID binding.GroupIDType
	if nodeLocalInternal == false {
		mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedAllEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, true).Times(1)
		if externalIP != nil {
			externalGroupID = internalGroupID
			clusterGroupID = internalGroupID
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
	} else {
		mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedAllEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.InAnyOrder(expectedLocalEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, true).Times(1)
		if externalIP != nil {
			externalGroupID = fp.groupCounter.AllocateIfNotExist(svcPortName, false)
			clusterGroupID = externalGroupID
			mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
	}
	if externalIP != nil {
		mockRouteClient.EXPECT().AddExternalIPRoute(externalIP)
	}
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func testLoadBalancerAdd(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	externalIP net.IP,
	ep1IP net.IP,
	ep2IP net.IP,
	loadBalancerIP net.IP,
	isIPv6 bool,
	nodeLocalInternal bool,
	nodeLocalExternal bool,
	proxyLoadBalancerIPs bool,
	endpointSliceEnabled bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	options := []proxyOptionsFn{withProxyAll}
	if !proxyLoadBalancerIPs {
		options = append(options, withoutProxyLoadBalancerIPs)
	}
	if !endpointSliceEnabled {
		options = append(options, withoutEndpointSlice)
	}
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, options...)

	externalTrafficPolicy := corev1.ServiceExternalTrafficPolicyTypeCluster
	if nodeLocalExternal {
		externalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
	}
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	if nodeLocalInternal {
		internalTrafficPolicy = corev1.ServiceInternalTrafficPolicyLocal
	}
	svc := makeTestLoadBalancerService(&svcPortName,
		svcIP,
		[]net.IP{externalIP},
		[]net.IP{loadBalancerIP},
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		&internalTrafficPolicy,
		externalTrafficPolicy)
	makeServiceMap(fp, svc)

	if !endpointSliceEnabled {
		remoteEpSubset := makeTestEndpointSubset(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
		localEpSubset := makeTestEndpointSubset(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
		eps := makeTestEndpoints(&svcPortName, []corev1.EndpointSubset{*remoteEpSubset, *localEpSubset})
		makeEndpointsMap(fp, eps)
	} else {
		remoteEp, remoteEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
		localEp, localEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
		endpointSlice := makeTestEndpointSlice(svcPortName.Namespace,
			svcPortName.Name,
			[]discovery.Endpoint{*remoteEp, *localEp},
			[]discovery.EndpointPort{*remoteEpPort, *localEpPort},
			isIPv6)
		makeEndpointSliceMap(fp, endpointSlice)
	}

	var nodeName string
	var serving bool
	if endpointSliceEnabled {
		nodeName = hostname
		serving = true
	}
	expectedLocalEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep2IP.String(), nodeName, "", svcPort, true, true, serving, false, nil)}
	expectedAllEps := append(expectedLocalEps, k8sproxy.NewBaseEndpointInfo(ep1IP.String(), "", "", svcPort, false, true, serving, false, nil))

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedAllEps)).Times(1)
	if nodeLocalInternal != nodeLocalExternal {
		var clusterIPEps, nodePortEps []k8sproxy.Endpoint
		if nodeLocalInternal {
			clusterIPEps = expectedLocalEps
			nodePortEps = expectedAllEps
		} else {
			clusterIPEps = expectedAllEps
			nodePortEps = expectedLocalEps
		}
		internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalInternal)
		externalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalExternal)
		var clusterGroupID binding.GroupIDType
		if nodeLocalInternal {
			clusterGroupID = externalGroupID
		} else {
			clusterGroupID = internalGroupID
		}
		mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.InAnyOrder(clusterIPEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
		mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.InAnyOrder(nodePortEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		if proxyLoadBalancerIPs {
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
		if externalIP != nil {
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
	} else {
		nodeLocalVal := nodeLocalInternal && nodeLocalExternal
		groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalVal)
		var clusterGroupID binding.GroupIDType
		if nodeLocalVal {
			clusterGroupID = fp.groupCounter.AllocateIfNotExist(svcPortName, false)
			mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedLocalEps)).Times(1)
			mockOFClient.EXPECT().InstallServiceGroup(clusterGroupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
		} else {
			clusterGroupID = groupID
			mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
			mockOFClient.EXPECT().UninstallServiceGroup(fp.groupCounter.AllocateIfNotExist(svcPortName, !nodeLocalVal)).Times(1)
		}
		mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, clusterGroupID, vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		if proxyLoadBalancerIPs {
			mockOFClient.EXPECT().InstallServiceFlows(groupID, clusterGroupID, loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
		if externalIP != nil {
			mockOFClient.EXPECT().InstallServiceFlows(groupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
	}
	if proxyLoadBalancerIPs {
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	if externalIP != nil {
		mockRouteClient.EXPECT().AddExternalIPRoute(externalIP)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func testNodePortAdd(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	externalIP net.IP,
	ep1IP net.IP,
	ep2IP net.IP,
	isIPv6 bool,
	nodeLocalInternal bool,
	nodeLocalExternal bool,
	endpointSliceEnabled bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	options := []proxyOptionsFn{withProxyAll}
	if !endpointSliceEnabled {
		options = append(options, withoutEndpointSlice)
	}
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, options...)

	externalTrafficPolicy := corev1.ServiceExternalTrafficPolicyTypeCluster
	if nodeLocalExternal {
		externalTrafficPolicy = corev1.ServiceExternalTrafficPolicyTypeLocal
	}
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	if nodeLocalInternal {
		internalTrafficPolicy = corev1.ServiceInternalTrafficPolicyLocal
	}
	svc := makeTestNodePortService(&svcPortName,
		svcIP,
		[]net.IP{externalIP},
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		internalTrafficPolicy,
		externalTrafficPolicy)
	makeServiceMap(fp, svc)

	if !endpointSliceEnabled {
		remoteEpSubset := makeTestEndpointSubset(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
		localEpSubset := makeTestEndpointSubset(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
		eps := makeTestEndpoints(&svcPortName, []corev1.EndpointSubset{*remoteEpSubset, *localEpSubset})
		makeEndpointsMap(fp, eps)
	} else {
		remoteEp, remoteEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
		localEp, localEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
		endpointSlice := makeTestEndpointSlice(svcPortName.Namespace,
			svcPortName.Name,
			[]discovery.Endpoint{*remoteEp, *localEp},
			[]discovery.EndpointPort{*remoteEpPort, *localEpPort},
			isIPv6)
		makeEndpointSliceMap(fp, endpointSlice)
	}

	var nodeName string
	var serving bool
	if endpointSliceEnabled {
		nodeName = hostname
		serving = true
	}
	expectedLocalEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep2IP.String(), nodeName, "", svcPort, true, true, serving, false, nil)}
	expectedAllEps := append(expectedLocalEps, k8sproxy.NewBaseEndpointInfo(ep1IP.String(), "", "", svcPort, false, true, serving, false, nil))

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedAllEps)).Times(1)
	if nodeLocalInternal != nodeLocalExternal {
		var clusterIPEps, nodePortEps []k8sproxy.Endpoint
		if nodeLocalInternal {
			clusterIPEps = expectedLocalEps
			nodePortEps = expectedAllEps
		} else {
			clusterIPEps = expectedAllEps
			nodePortEps = expectedLocalEps
		}
		internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalInternal)
		externalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalExternal)
		var clusterGroupID binding.GroupIDType
		if nodeLocalInternal {
			clusterGroupID = externalGroupID
		} else {
			clusterGroupID = internalGroupID
		}

		mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.InAnyOrder(clusterIPEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
		mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.InAnyOrder(nodePortEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		if externalIP != nil {
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
	} else {
		nodeLocalVal := nodeLocalInternal && nodeLocalExternal
		groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalVal)
		var clusterGroupID binding.GroupIDType
		if nodeLocalVal {
			clusterGroupID = fp.groupCounter.AllocateIfNotExist(svcPortName, false)
			mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedLocalEps)).Times(1)
			mockOFClient.EXPECT().InstallServiceGroup(clusterGroupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
		} else {
			clusterGroupID = groupID
			mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
			mockOFClient.EXPECT().UninstallServiceGroup(fp.groupCounter.AllocateIfNotExist(svcPortName, !nodeLocalVal)).Times(1)
		}
		mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, clusterGroupID, vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		if externalIP != nil {
			mockOFClient.EXPECT().InstallServiceFlows(groupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		}
	}
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	if externalIP != nil {
		mockRouteClient.EXPECT().AddExternalIPRoute(externalIP)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestClusterIPAdd(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, false, []*corev1.Service{}, []*corev1.Endpoints{}, false)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, true, []*corev1.Service{}, []*corev1.Endpoints{}, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, false, []*corev1.Service{}, []*corev1.Endpoints{}, true)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, true, []*corev1.Service{}, []*corev1.Endpoints{}, true)
			})
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, false, []*corev1.Service{}, []*corev1.Endpoints{}, false)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, true, []*corev1.Service{}, []*corev1.Endpoints{}, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, false, []*corev1.Service{}, []*corev1.Endpoints{}, true)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPAdd(t, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, true, []*corev1.Service{}, []*corev1.Endpoints{}, true)
			})
		})
	})
}

func TestLoadBalancerAdd(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, false, false, true, false)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, false, true, true, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, true, false, true, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, true, true, true, false)
			})
			t.Run("No External IPs", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, false, false, false, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, false, false, true, true)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, false, true, true, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, true, false, true, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, true, true, true, true)
			})
			t.Run("No External IPs", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, loadBalancerIPv4, false, false, false, false, true)
			})
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, false, false, true, false)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, false, true, true, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, true, false, true, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, true, true, true, false)
			})
			t.Run("No External IPs", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, false, false, false, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, false, false, true, true)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, false, true, true, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, true, false, true, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, true, true, true, true)
			})
			t.Run("No External IPs", func(t *testing.T) {
				testLoadBalancerAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, loadBalancerIPv6, true, false, false, false, true)
			})
		})
	})
}

func TestLoadBalancerServiceWithMultiplePorts(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	nodePortAddresses := []net.IP{net.ParseIP("0.0.0.0")}
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, false, withProxyAll)

	port80Str := "port80"
	port80Int32 := int32(80)
	port443Str := "port443"
	port443Int32 := int32(443)
	port30001Int32 := int32(30001)
	port30002Int32 := int32(30002)
	protocolTCP := corev1.ProtocolTCP
	endpoint1Address := "192.168.0.11"
	endpoint2Address := "192.168.1.11"
	endpoint1NodeName := fp.hostname
	endpoint2NodeName := "node2"

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       port80Str,
					Protocol:   protocolTCP,
					Port:       port80Int32,
					TargetPort: intstr.FromInt(int(port80Int32)),
					NodePort:   port30001Int32,
				},
				{
					Name:       port443Str,
					Protocol:   protocolTCP,
					Port:       port443Int32,
					TargetPort: intstr.FromInt(int(port443Int32)),
					NodePort:   port30002Int32,
				},
			},
			ClusterIP:             svc1IPv4.String(),
			ClusterIPs:            []string{svc1IPv4.String()},
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyTypeLocal,
			HealthCheckNodePort:   40000,
			IPFamilies:            []corev1.IPFamily{corev1.IPv4Protocol},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
				{IP: loadBalancerIPv4.String()},
			}},
		},
	}
	makeServiceMap(fp, svc)

	endpointSlice := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc-x5ks2",
			Namespace: svc.Namespace,
			Labels: map[string]string{
				discovery.LabelServiceName: svc.Name,
			},
		},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{
			{
				Addresses: []string{
					endpoint1Address,
				},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				NodeName: &endpoint1NodeName,
			},
			{
				Addresses: []string{
					endpoint2Address,
				},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				NodeName: &endpoint2NodeName,
			},
		},
		Ports: []discovery.EndpointPort{
			{
				Name:     &port80Str,
				Port:     &port80Int32,
				Protocol: &protocolTCP,
			},
			{
				Name:     &port443Str,
				Port:     &port443Int32,
				Protocol: &protocolTCP,
			},
		},
	}
	makeEndpointSliceMap(fp, endpointSlice)

	localEndpointForPort80 := k8sproxy.NewBaseEndpointInfo(endpoint1Address, endpoint1NodeName, "", int(port80Int32), true, true, true, false, nil)
	localEndpointForPort443 := k8sproxy.NewBaseEndpointInfo(endpoint1Address, endpoint1NodeName, "", int(port443Int32), true, true, true, false, nil)
	remoteEndpointForPort80 := k8sproxy.NewBaseEndpointInfo(endpoint2Address, endpoint2NodeName, "", int(port80Int32), false, true, true, false, nil)
	remoteEndpointForPort443 := k8sproxy.NewBaseEndpointInfo(endpoint2Address, endpoint2NodeName, "", int(port443Int32), false, true, true, false, nil)

	mockOFClient.EXPECT().InstallEndpointFlows(binding.ProtocolTCP, gomock.InAnyOrder([]k8sproxy.Endpoint{localEndpointForPort80, remoteEndpointForPort80})).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(gomock.Any(), false, []k8sproxy.Endpoint{localEndpointForPort80}).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(gomock.Any(), false, gomock.InAnyOrder([]k8sproxy.Endpoint{localEndpointForPort80, remoteEndpointForPort80})).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), svc1IPv4, uint16(port80Int32), binding.ProtocolTCP, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), agentconfig.VirtualNodePortDNATIPv4, uint16(port30001Int32), binding.ProtocolTCP, uint16(0), true, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), loadBalancerIPv4, uint16(port80Int32), binding.ProtocolTCP, uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(port30001Int32), binding.ProtocolTCP).Times(1)
	mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIPv4).Times(1)

	mockOFClient.EXPECT().InstallEndpointFlows(binding.ProtocolTCP, gomock.InAnyOrder([]k8sproxy.Endpoint{localEndpointForPort443, remoteEndpointForPort443})).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(gomock.Any(), false, []k8sproxy.Endpoint{localEndpointForPort443}).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(gomock.Any(), false, gomock.InAnyOrder([]k8sproxy.Endpoint{localEndpointForPort443, remoteEndpointForPort443})).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), svc1IPv4, uint16(port443Int32), binding.ProtocolTCP, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), agentconfig.VirtualNodePortDNATIPv4, uint16(port30002Int32), binding.ProtocolTCP, uint16(0), true, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), loadBalancerIPv4, uint16(port443Int32), binding.ProtocolTCP, uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(port30002Int32), binding.ProtocolTCP).Times(1)

	fp.syncProxyRules()

	// Remove the service.
	fp.serviceChanges.OnServiceUpdate(svc, nil)
	fp.endpointsChanges.OnEndpointSliceUpdate(endpointSlice, true)

	mockOFClient.EXPECT().UninstallEndpointFlows(binding.ProtocolTCP, gomock.InAnyOrder([]k8sproxy.Endpoint{localEndpointForPort80, remoteEndpointForPort80}))
	mockOFClient.EXPECT().UninstallServiceGroup(gomock.Any()).Times(2)
	mockOFClient.EXPECT().UninstallServiceFlows(svc1IPv4, uint16(port80Int32), binding.ProtocolTCP)
	mockOFClient.EXPECT().UninstallServiceFlows(agentconfig.VirtualNodePortDNATIPv4, uint16(port30001Int32), binding.ProtocolTCP)
	mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIPv4, uint16(port80Int32), binding.ProtocolTCP)
	mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(port30001Int32), binding.ProtocolTCP)

	mockOFClient.EXPECT().UninstallEndpointFlows(binding.ProtocolTCP, gomock.InAnyOrder([]k8sproxy.Endpoint{localEndpointForPort443, remoteEndpointForPort443}))
	mockOFClient.EXPECT().UninstallServiceGroup(gomock.Any()).Times(2)
	mockOFClient.EXPECT().UninstallServiceFlows(svc1IPv4, uint16(port443Int32), binding.ProtocolTCP)
	mockOFClient.EXPECT().UninstallServiceFlows(agentconfig.VirtualNodePortDNATIPv4, uint16(port30002Int32), binding.ProtocolTCP)
	mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIPv4, uint16(port443Int32), binding.ProtocolTCP)
	mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(port30002Int32), binding.ProtocolTCP)
	// The route for the ClusterIP and the LoadBalancer IP should only be uninstalled once.
	mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIPv4)

	fp.syncProxyRules()

	assert.Emptyf(t, fp.serviceIPRouteReferences, "serviceIPRouteReferences was not cleaned up after Service was removed")
}

func TestNodePortAdd(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, false, false, false)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, false, true, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, true, false, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, true, true, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, false, false, true)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, false, true, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, true, false, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, ep2IPv4, false, true, true, true)
			})
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, false, false, false)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, false, true, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, true, false, false)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, true, true, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, false, false, true)
			})
			t.Run("InternalTrafficPolicy:Cluster ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, false, true, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Cluster", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, true, false, true)
			})
			t.Run("InternalTrafficPolicy:Local ExternalTrafficPolicy:Local", func(t *testing.T) {
				testNodePortAdd(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, ep2IPv6, true, true, true, true)
			})
		})
	})
}

func TestClusterSkipServices(t *testing.T) {
	svc1Port := 53
	svc2Port := 88
	svc1ClusterIP := net.ParseIP("10.96.10.12")
	svc2ClusterIP := net.ParseIP(skippedClusterIP)
	ep1IP := net.ParseIP("172.16.1.2")
	ep2IP := net.ParseIP("172.16.1.3")

	skippedServiceNamespace := strings.Split(skippedServiceNN, "/")[0]
	skippedServiceName := strings.Split(skippedServiceNN, "/")[1]
	svc1PortName := makeSvcPortName(skippedServiceNamespace, skippedServiceName, strconv.Itoa(svc1Port), corev1.ProtocolTCP)
	svc2PortName := makeSvcPortName("kube-system", "test", strconv.Itoa(svc2Port), corev1.ProtocolTCP)
	svc1 := makeTestClusterIPService(&svc1PortName, svc1ClusterIP, nil, int32(svc1Port), corev1.ProtocolTCP, nil, nil, false, nil)
	svc2 := makeTestClusterIPService(&svc2PortName, svc2ClusterIP, nil, int32(svc2Port), corev1.ProtocolTCP, nil, nil, false, nil)
	svcs := []*corev1.Service{svc1, svc2}

	epSubset := makeTestEndpointSubset(&svc1PortName, ep1IP, int32(svc1Port), corev1.ProtocolTCP, false)
	ep1 := makeTestEndpoints(&svc1PortName, []corev1.EndpointSubset{*epSubset})
	epSubset = makeTestEndpointSubset(&svc1PortName, ep2IP, int32(svc2Port), corev1.ProtocolTCP, false)
	ep2 := makeTestEndpoints(&svc2PortName, []corev1.EndpointSubset{*epSubset})
	eps := []*corev1.Endpoints{ep1, ep2}

	testClusterIPAdd(t, svc1IPv4, nil, ep1IPv4, ep2IPv4, false, false, svcs, eps, false)
}

func TestDualStackService(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	ipv4GroupAllocator := openflow.NewGroupAllocator()
	ipv6GroupAllocator := openflow.NewGroupAllocator()
	fpv4 := newFakeProxier(mockRouteClient, mockOFClient, nil, ipv4GroupAllocator, false)
	fpv6 := newFakeProxier(mockRouteClient, mockOFClient, nil, ipv6GroupAllocator, true)

	svc := makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *corev1.Service) {
		svc.Spec.ClusterIP = svc1IPv4.String()
		svc.Spec.ClusterIPs = []string{svc1IPv4.String(), svc1IPv6.String()}
		svc.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:     svcPortName.Port,
			Port:     int32(svcPort),
			Protocol: corev1.ProtocolTCP,
		}}
	})

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IPv4, int32(svcPort), corev1.ProtocolTCP, false)
	epv4 := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, false)
	ep, epPort = makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IPv6, int32(svcPort), corev1.ProtocolTCP, false)
	epv6 := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, true)

	// In production code, each proxier creates its own serviceConfig and endpointSliceConfig, to which each proxier
	// will register its event handler. So we call each proxier's event handlers directly, instead of meta proxier's
	// ones.
	fpv4.OnServiceUpdate(nil, svc)
	fpv4.OnServiceSynced()
	fpv4.OnEndpointSliceUpdate(nil, epv4)
	fpv4.OnEndpointSliceUpdate(nil, epv6)
	fpv4.OnEndpointsSynced()
	fpv6.OnServiceUpdate(nil, svc)
	fpv6.OnServiceSynced()
	fpv6.OnEndpointSliceUpdate(nil, epv4)
	fpv6.OnEndpointSliceUpdate(nil, epv6)
	fpv6.OnEndpointsSynced()

	groupIDv4 := fpv4.groupCounter.AllocateIfNotExist(svcPortName, false)
	groupIDv6 := fpv6.groupCounter.AllocateIfNotExist(svcPortName, false)

	mockOFClient.EXPECT().InstallServiceGroup(groupIDv4, false, []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep1IPv4.String(), "", "", svcPort, false, true, true, false, nil)}).Times(1)
	mockOFClient.EXPECT().InstallEndpointFlows(binding.ProtocolTCP, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDv4, binding.GroupIDType(0), svc1IPv4, uint16(svcPort), binding.ProtocolTCP, uint16(0), false, false).Times(1)

	mockOFClient.EXPECT().InstallServiceGroup(groupIDv6, false, []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep1IPv6.String(), "", "", svcPort, false, true, true, false, nil)}).Times(1)
	mockOFClient.EXPECT().InstallEndpointFlows(binding.ProtocolTCPv6, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDv6, binding.GroupIDType(0), svc1IPv6, uint16(svcPort), binding.ProtocolTCPv6, uint16(0), false, false).Times(1)

	fpv4.syncProxyRules()
	fpv6.syncProxyRules()
	assert.Contains(t, fpv4.serviceInstalledMap, svcPortName)
	assert.Contains(t, fpv6.serviceInstalledMap, svcPortName)
}

func testClusterIPRemove(t *testing.T, svcIP, externalIP, epIP net.IP, isIPv6 bool, nodeLocalInternal, endpointSliceEnabled bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	options := []proxyOptionsFn{withProxyAll, withSupportNestedService}
	if !endpointSliceEnabled {
		options = append(options, withoutEndpointSlice)
	}
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6, options...)

	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	if nodeLocalInternal {
		internalTrafficPolicy = corev1.ServiceInternalTrafficPolicyLocal
	}
	svc := makeTestClusterIPService(&svcPortName, svcIP, []net.IP{externalIP}, int32(svcPort), corev1.ProtocolTCP, nil, &internalTrafficPolicy, true, nil)
	makeServiceMap(fp, svc)

	var ep *corev1.Endpoints
	var eps *discovery.EndpointSlice
	if !endpointSliceEnabled {
		epSubset := makeTestEndpointSubset(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
		ep = makeTestEndpoints(&svcPortName, []corev1.EndpointSubset{*epSubset})
		makeEndpointsMap(fp, ep)
	} else {
		epSubset, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
		eps = makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*epSubset}, []discovery.EndpointPort{*epPort}, isIPv6)
		makeEndpointSliceMap(fp, eps)
	}

	bindingProtocol := binding.ProtocolTCP
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
	}

	internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, nodeLocalInternal)
	var externalGroupID, clusterGroupID binding.GroupIDType
	if nodeLocalInternal == false {
		mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
		mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.Any()).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, true).Times(1)
		mockOFClient.EXPECT().UninstallServiceGroup(gomock.Any()).Times(1)
		mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
		mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
		if externalIP != nil {
			externalGroupID = internalGroupID
			clusterGroupID = internalGroupID
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
			mockOFClient.EXPECT().UninstallServiceFlows(externalIP, uint16(svcPort), bindingProtocol).Times(1)
		}
	} else {
		mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.Any()).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, true).Times(1)
		mockOFClient.EXPECT().UninstallServiceGroup(internalGroupID).Times(1)
		mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
		if externalIP != nil {
			externalGroupID = fp.groupCounter.AllocateIfNotExist(svcPortName, false)
			clusterGroupID = externalGroupID
			mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.Any()).Times(1)
			mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
			mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)

			mockOFClient.EXPECT().UninstallServiceGroup(externalGroupID).Times(1)
			mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
			mockOFClient.EXPECT().UninstallServiceFlows(externalIP, uint16(svcPort), bindingProtocol).Times(1)
		}
	}
	if externalIP != nil {
		mockRouteClient.EXPECT().AddExternalIPRoute(externalIP)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(externalIP)
	}
	fp.syncProxyRules()

	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)

	fp.serviceChanges.OnServiceUpdate(svc, nil)
	if !endpointSliceEnabled {
		fp.endpointsChanges.OnEndpointUpdate(ep, nil)
	} else {
		fp.endpointsChanges.OnEndpointSliceUpdate(eps, true)
	}
	fp.syncProxyRules()

	assert.NotContains(t, fp.serviceInstalledMap, svcPortName)
	assert.NotContains(t, fp.endpointsInstalledMap, svcPortName)
	_, exists := fp.groupCounter.Get(svcPortName, nodeLocalInternal)
	assert.False(t, exists)
}

func testNodePortRemove(t *testing.T, nodePortAddresses []net.IP, svcIP, externalIP, epIP net.IP, isIPv6 bool, endpointSliceEnabled bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	options := []proxyOptionsFn{withProxyAll}
	if !endpointSliceEnabled {
		options = append(options, withoutEndpointSlice)
	}
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, options...)

	svc := makeTestNodePortService(&svcPortName,
		svcIP,
		[]net.IP{externalIP},
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		corev1.ServiceInternalTrafficPolicyCluster,
		corev1.ServiceExternalTrafficPolicyTypeLocal)
	makeServiceMap(fp, svc)

	var ep *corev1.Endpoints
	var eps *discovery.EndpointSlice
	if !endpointSliceEnabled {
		epSubset := makeTestEndpointSubset(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
		ep = makeTestEndpoints(&svcPortName, []corev1.EndpointSubset{*epSubset})
		makeEndpointsMap(fp, ep)
	} else {
		epSubset, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
		eps = makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*epSubset}, []discovery.EndpointPort{*epPort}, isIPv6)
		makeEndpointSliceMap(fp, eps)
	}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	externalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, true)
	internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	clusterGroupID := internalGroupID
	mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	if externalIP != nil {
		mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(externalIP)
	}

	mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceGroup(gomock.Any()).Times(2)
	mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	if externalIP != nil {
		mockOFClient.EXPECT().UninstallServiceFlows(externalIP, uint16(svcPort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(externalIP)
	}
	fp.syncProxyRules()

	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)

	fp.serviceChanges.OnServiceUpdate(svc, nil)
	if !endpointSliceEnabled {
		fp.endpointsChanges.OnEndpointUpdate(ep, nil)
	} else {
		fp.endpointsChanges.OnEndpointSliceUpdate(eps, true)
	}
	fp.syncProxyRules()

	assert.NotContains(t, fp.serviceInstalledMap, svcPortName)
	assert.NotContains(t, fp.endpointsInstalledMap, svcPortName)
}

func testLoadBalancerRemove(t *testing.T, nodePortAddresses []net.IP, svcIP, externalIP, epIP, loadBalancerIP net.IP, isIPv6 bool, endpointSliceEnabled bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	options := []proxyOptionsFn{withProxyAll}
	if !endpointSliceEnabled {
		options = append(options, withoutEndpointSlice)
	}
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, options...)

	externalTrafficPolicy := corev1.ServiceExternalTrafficPolicyTypeLocal
	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster

	svc := makeTestLoadBalancerService(&svcPortName,
		svcIP,
		[]net.IP{externalIP},
		[]net.IP{loadBalancerIP},
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		&internalTrafficPolicy,
		externalTrafficPolicy)
	makeServiceMap(fp, svc)

	var ep *corev1.Endpoints
	var eps *discovery.EndpointSlice
	if !endpointSliceEnabled {
		epSubset := makeTestEndpointSubset(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, true)
		ep = makeTestEndpoints(&svcPortName, []corev1.EndpointSubset{*epSubset})
		makeEndpointsMap(fp, ep)
	} else {
		epSubset, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, true)
		eps = makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*epSubset}, []discovery.EndpointPort{*epPort}, isIPv6)
		makeEndpointSliceMap(fp, eps)
	}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	externalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, true)
	internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	clusterGroupID := internalGroupID
	mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	if externalIP != nil {
		mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, externalIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(externalIP)
	}

	mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceGroup(gomock.Any()).Times(2)
	mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
	if externalIP != nil {
		mockOFClient.EXPECT().UninstallServiceFlows(externalIP, uint16(svcPort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(externalIP)
	}
	fp.syncProxyRules()

	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)

	fp.serviceChanges.OnServiceUpdate(svc, nil)
	if !endpointSliceEnabled {
		fp.endpointsChanges.OnEndpointUpdate(ep, nil)
	} else {
		fp.endpointsChanges.OnEndpointSliceUpdate(eps, true)
	}
	fp.syncProxyRules()

	assert.NotContains(t, fp.serviceInstalledMap, svcPortName)
	assert.NotContains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestClusterIPRemove(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv4, externalIPv4, ep1IPv4, false, false, false)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv4, externalIPv4, ep1IPv4, false, true, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv4, externalIPv4, ep1IPv4, false, false, true)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv4, externalIPv4, ep1IPv4, false, true, true)
			})
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv6, externalIPv6, ep1IPv6, true, false, false)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv6, externalIPv6, ep1IPv6, true, true, false)
			})
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			t.Run("InternalTrafficPolicy Cluster", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv6, externalIPv6, ep1IPv6, true, false, true)
			})
			t.Run("InternalTrafficPolicy Local", func(t *testing.T) {
				testClusterIPRemove(t, svc1IPv6, externalIPv6, ep1IPv6, true, true, true)
			})
		})
	})
}

func TestNodePortRemove(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			testNodePortRemove(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, false, false)
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			testNodePortRemove(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, false, true)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			testNodePortRemove(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, true, false)
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			testNodePortRemove(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, true, true)
		})
	})
}

func TestLoadBalancerRemove(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			testLoadBalancerRemove(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, loadBalancerIPv4, false, false)
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			testLoadBalancerRemove(t, nodePortAddressesIPv4, svc1IPv4, externalIPv4, ep1IPv4, loadBalancerIPv4, false, true)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("Endpoints", func(t *testing.T) {
			testLoadBalancerRemove(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, loadBalancerIPv6, true, false)
		})
		t.Run("EndpointSlice", func(t *testing.T) {
			testLoadBalancerRemove(t, nodePortAddressesIPv6, svc1IPv6, externalIPv6, ep1IPv6, loadBalancerIPv6, true, true)
		})
	})

}

func testClusterIPNoEndpoint(t *testing.T, svcIP net.IP, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6)

	svc := makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	updatedSvc := makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort+1), corev1.ProtocolTCP, nil, nil, false, nil)
	makeServiceMap(fp, svc)
	makeEndpointSliceMap(fp)

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, []k8sproxy.Endpoint{}).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), gomock.Any(), uint16(0), false, false).Times(1)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)

	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort+1), gomock.Any(), uint16(0), false, false).Times(1)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
}

func TestClusterIPNoEndpoint(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		testClusterIPNoEndpoint(t, svc1IPv4, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testClusterIPNoEndpoint(t, svc1IPv6, true)
	})
}

func testNodePortNoEndpoint(t *testing.T, nodePortAddresses []net.IP, svcIP net.IP, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	svc := makeTestNodePortService(&svcPortName,
		svcIP,
		nil,
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		corev1.ServiceInternalTrafficPolicyCluster,
		corev1.ServiceExternalTrafficPolicyTypeLocal)
	updatedSvc := makeTestNodePortService(&svcPortName,
		svcIP,
		nil,
		int32(svcPort+1),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		corev1.ServiceInternalTrafficPolicyCluster,
		corev1.ServiceExternalTrafficPolicyTypeLocal)
	makeServiceMap(fp, svc)
	makeEndpointSliceMap(fp)

	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupIDCluster := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	groupIDLocal := fp.groupCounter.AllocateIfNotExist(svcPortName, true)
	mockOFClient.EXPECT().InstallServiceGroup(groupIDCluster, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupIDLocal, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDCluster, binding.GroupIDType(0), svcIP, uint16(svcPort), gomock.Any(), uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDLocal, groupIDCluster, vIP, uint16(svcNodePort), gomock.Any(), uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), gomock.Any()).Times(1)
	fp.syncProxyRules()

	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), gomock.Any()).Times(1)
	mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDCluster, binding.GroupIDType(0), svcIP, uint16(svcPort+1), gomock.Any(), uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDLocal, groupIDCluster, vIP, uint16(svcNodePort), gomock.Any(), uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), gomock.Any()).Times(1)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
}

func TestNodePortNoEndpoint(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		testNodePortNoEndpoint(t, nodePortAddressesIPv4, svc1IPv4, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testNodePortNoEndpoint(t, nodePortAddressesIPv6, svc1IPv6, true)
	})
}

func testLoadBalancerNoEndpoint(t *testing.T, nodePortAddresses []net.IP, svcIP net.IP, loadBalancerIP net.IP, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	internalTrafficPolicy := corev1.ServiceInternalTrafficPolicyCluster
	externalTrafficPolicy := corev1.ServiceExternalTrafficPolicyTypeLocal

	svc := makeTestLoadBalancerService(&svcPortName,
		svcIP,
		nil,
		[]net.IP{loadBalancerIP},
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		&internalTrafficPolicy,
		externalTrafficPolicy)
	updatedSvc := makeTestLoadBalancerService(&svcPortName,
		svcIP,
		nil,
		[]net.IP{loadBalancerIP},
		int32(svcPort+1),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		&internalTrafficPolicy,
		externalTrafficPolicy)
	makeServiceMap(fp, svc)
	makeEndpointSliceMap(fp)

	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	internalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	externalGroupID := fp.groupCounter.AllocateIfNotExist(svcPortName, true)
	clusterGroupID := internalGroupID
	mockOFClient.EXPECT().InstallServiceGroup(internalGroupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(externalGroupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort), gomock.Any(), uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, vIP, uint16(svcNodePort), gomock.Any(), uint16(0), true, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, loadBalancerIP, uint16(svcPort), gomock.Any(), uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), gomock.Any()).Times(1)
	mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	fp.syncProxyRules()

	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), gomock.Any()).Times(1)
	mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), gomock.Any()).Times(1)
	mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(internalGroupID, binding.GroupIDType(0), svcIP, uint16(svcPort+1), gomock.Any(), uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, vIP, uint16(svcNodePort), gomock.Any(), uint16(0), true, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(externalGroupID, clusterGroupID, loadBalancerIP, uint16(svcPort+1), gomock.Any(), uint16(0), true, false).Times(1)
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), gomock.Any()).Times(1)
	mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
}

func TestLoadBalancerNoEndpoint(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		testLoadBalancerNoEndpoint(t, nodePortAddressesIPv4, svc1IPv4, loadBalancerIPv4, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testLoadBalancerNoEndpoint(t, nodePortAddressesIPv6, svc1IPv6, loadBalancerIPv6, true)
	})
}

func testClusterIPRemoveSamePortEndpoint(t *testing.T, svcIP net.IP, epIP net.IP, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6)

	svcPortNameTCP := makeSvcPortName("ns", "svc-tcp", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svcPortNameUDP := makeSvcPortName("ns", "svc-udp", strconv.Itoa(svcPort), corev1.ProtocolUDP)

	svcTCP := makeTestClusterIPService(&svcPortNameTCP, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	svcUDP := makeTestClusterIPService(&svcPortNameUDP, svcIP, nil, int32(svcPort), corev1.ProtocolUDP, nil, nil, false, nil)
	makeServiceMap(fp, svcTCP, svcUDP)

	epTCP, epPortTCP := makeTestEndpointSliceEndpointAndPort(&svcPortNameTCP, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	epsTCP := makeTestEndpointSlice(svcPortNameTCP.Namespace, svcPortNameTCP.Name, []discovery.Endpoint{*epTCP}, []discovery.EndpointPort{*epPortTCP}, isIPv6)
	makeEndpointSliceMap(fp, epsTCP)
	epUDP, epPortUDP := makeTestEndpointSliceEndpointAndPort(&svcPortNameUDP, epIP, int32(svcPort), corev1.ProtocolUDP, false)
	epsUDP := makeTestEndpointSlice(svcPortNameUDP.Namespace, svcPortNameUDP.Name, []discovery.Endpoint{*epUDP}, []discovery.EndpointPort{*epPortUDP}, isIPv6)
	makeEndpointSliceMap(fp, epsUDP)

	protocolTCP := binding.ProtocolTCP
	protocolUDP := binding.ProtocolUDP
	if isIPv6 {
		protocolTCP = binding.ProtocolTCPv6
		protocolUDP = binding.ProtocolUDPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortNameTCP, false)
	groupIDUDP := fp.groupCounter.AllocateIfNotExist(svcPortNameUDP, false)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupIDUDP, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallEndpointFlows(protocolTCP, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallEndpointFlows(protocolUDP, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), protocolTCP, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDUDP, binding.GroupIDType(0), svcIP, uint16(svcPort), protocolUDP, uint16(0), false, false).Times(1)
	fp.syncProxyRules()

	mockOFClient.EXPECT().InstallServiceGroup(groupIDUDP, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallEndpointFlows(protocolUDP, gomock.Any()).Times(1)
	fp.endpointsChanges.OnEndpointSliceUpdate(epsUDP, true)
	fp.syncProxyRules()

	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallEndpointFlows(protocolTCP, gomock.Any()).Times(1)
	fp.endpointsChanges.OnEndpointSliceUpdate(epsTCP, true)
	fp.syncProxyRules()
}

func TestClusterIPRemoveSamePortEndpoint(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		testClusterIPRemoveSamePortEndpoint(t, svc1IPv4, ep1IPv4, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testClusterIPRemoveSamePortEndpoint(t, svc1IPv6, ep1IPv6, true)
	})
}

func testClusterIPRemoveEndpoints(t *testing.T, svcIP net.IP, epIP net.IP, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6)

	svc := makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	bindingProtocol := binding.ProtocolTCP
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	fp.syncProxyRules()

	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)

	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	fp.endpointsChanges.OnEndpointSliceUpdate(eps, true)
	fp.syncProxyRules()

	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	endpointsMap, ok := fp.endpointsInstalledMap[svcPortName]
	assert.True(t, ok)
	assert.Equal(t, 0, len(endpointsMap))
	fp.syncProxyRules()
}

func TestClusterIPRemoveEndpoints(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		testClusterIPRemoveEndpoints(t, svc1IPv4, ep1IPv4, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testClusterIPRemoveEndpoints(t, svc1IPv6, ep1IPv6, true)
	})
}

func testSessionAffinity(t *testing.T, svcIP net.IP, epIP net.IP, affinitySeconds int32, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6)

	svc := makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *corev1.Service) {
		svc.Spec.Type = corev1.ServiceTypeNodePort
		svc.Spec.ClusterIP = svcIP.String()
		svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
		svc.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
			ClientIP: &corev1.ClientIPConfig{
				TimeoutSeconds: &affinitySeconds,
			},
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:     svcPortName.Port,
			Port:     int32(svcPort),
			Protocol: corev1.ProtocolTCP,
			NodePort: int32(svcNodePort),
		}}
	})
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	bindingProtocol := binding.ProtocolTCP
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
	}
	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, true, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	var expectedAffinity uint16
	if affinitySeconds > math.MaxUint16 {
		expectedAffinity = math.MaxUint16
	} else {
		expectedAffinity = uint16(affinitySeconds)
	}
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, expectedAffinity, false, false).Times(1)

	fp.syncProxyRules()
}

func TestSessionAffinity(t *testing.T) {
	affinitySeconds := corev1.DefaultClientIPServiceAffinitySeconds
	t.Run("IPv4", func(t *testing.T) {
		testSessionAffinity(t, svc1IPv4, ep1IPv4, affinitySeconds, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testSessionAffinity(t, svc1IPv6, ep1IPv6, affinitySeconds, true)
	})
}

func TestSessionAffinityOverflow(t *testing.T) {
	// Ensure that the SessionAffinity timeout is truncated to the max supported value, instead
	// of wrapping around.
	affinitySeconds := int32(math.MaxUint16 + 10)
	testSessionAffinity(t, svc1IPv4, ep1IPv4, affinitySeconds, false)
}

func testSessionAffinityNoEndpoint(t *testing.T, svcExternalIPs net.IP, svcIP net.IP, isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6)

	timeoutSeconds := corev1.DefaultClientIPServiceAffinitySeconds

	svc := makeTestService(svcPortName.Namespace, svcPortName.Name, func(svc *corev1.Service) {
		svc.Spec.Type = corev1.ServiceTypeNodePort
		svc.Spec.ClusterIP = svcIP.String()
		svc.Spec.ExternalIPs = []string{svcExternalIPs.String()}
		svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
		svc.Spec.SessionAffinityConfig = &corev1.SessionAffinityConfig{
			ClientIP: &corev1.ClientIPConfig{
				TimeoutSeconds: &timeoutSeconds,
			},
		}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:     svcPortName.Port,
			Port:     int32(svcPort),
			Protocol: corev1.ProtocolTCP,
			NodePort: int32(svcNodePort),
		}}
	})
	makeServiceMap(fp, svc)
	makeEndpointsMap(fp)

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, true, []k8sproxy.Endpoint{}).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), gomock.Any(), uint16(10800), false, false).Times(1)
	fp.syncProxyRules()
}

func TestSessionAffinityNoEndpoint(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		testSessionAffinityNoEndpoint(t, net.ParseIP("50.60.70.81"), svc1IPv4, false)
	})
	t.Run("IPv6", func(t *testing.T) {
		testSessionAffinityNoEndpoint(t, net.ParseIP("5060:70::81"), svc1IPv6, true)
	})
}

func testServiceClusterIPUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	updatedSvcIP net.IP,
	loadBalancerIP net.IP,
	epIP net.IP,
	svcType corev1.ServiceType,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var svc, updatedSvc *corev1.Service
	switch svcType {
	case corev1.ServiceTypeClusterIP:
		svc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
		updatedSvc = makeTestClusterIPService(&svcPortName, updatedSvcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	case corev1.ServiceTypeNodePort:
		svc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestNodePortService(&svcPortName, updatedSvcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
	case corev1.ServiceTypeLoadBalancer:
		svc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestLoadBalancerService(&svcPortName, updatedSvcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	}
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(epIP.String(), "", "", svcPort, false, true, true, false, nil)}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)

	s1 := mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	s2 := mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), updatedSvcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	s2.After(s1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)

		mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)

		mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), bindingProtocol).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServiceClusterIPUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceClusterIPUpdate(t, nil, svc1IPv4, svc2IPv4, nil, ep1IPv4, corev1.ServiceTypeClusterIP, false)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServiceClusterIPUpdate(t, nodePortAddressesIPv4, svc1IPv4, svc2IPv4, nil, ep1IPv4, corev1.ServiceTypeNodePort, false)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceClusterIPUpdate(t, nodePortAddressesIPv4, svc1IPv4, svc2IPv4, loadBalancerIPv4, ep1IPv4, corev1.ServiceTypeLoadBalancer, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceClusterIPUpdate(t, nil, svc1IPv6, svc2IPv6, nil, ep1IPv6, corev1.ServiceTypeClusterIP, true)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServiceClusterIPUpdate(t, nodePortAddressesIPv6, svc1IPv6, svc2IPv6, nil, ep1IPv6, corev1.ServiceTypeNodePort, true)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceClusterIPUpdate(t, nodePortAddressesIPv6, svc1IPv6, svc2IPv6, loadBalancerIPv6, ep1IPv6, corev1.ServiceTypeLoadBalancer, true)
		})
	})
}

func testServicePortUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	loadBalancerIP net.IP,
	epIP net.IP,
	svcType corev1.ServiceType,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var svc, updatedSvc *corev1.Service
	switch svcType {
	case corev1.ServiceTypeClusterIP:
		svc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
		updatedSvc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort+1), corev1.ProtocolTCP, nil, nil, false, nil)
	case corev1.ServiceTypeNodePort:
		svc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort+1), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
	case corev1.ServiceTypeLoadBalancer:
		svc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort+1), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	}
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(epIP.String(), "", "", svcPort, false, true, true, false, nil)}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)

	s1 := mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	s2 := mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort+1), bindingProtocol, uint16(0), false, false).Times(1)
	s2.After(s1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)

		mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)

		s1 = mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), bindingProtocol)
		s2 = mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort+1), bindingProtocol, uint16(0), true, false).Times(1)
		s2.After(s1)

		mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServicePortUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServicePortUpdate(t, nil, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeClusterIP, false)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServicePortUpdate(t, nodePortAddressesIPv4, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeNodePort, false)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServicePortUpdate(t, nodePortAddressesIPv4, svc1IPv4, loadBalancerIPv4, ep1IPv4, corev1.ServiceTypeLoadBalancer, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServicePortUpdate(t, nil, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeClusterIP, true)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServicePortUpdate(t, nodePortAddressesIPv6, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeNodePort, true)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServicePortUpdate(t, nodePortAddressesIPv6, svc1IPv6, loadBalancerIPv6, ep1IPv6, corev1.ServiceTypeLoadBalancer, true)
		})
	})
}

func testServiceNodePortUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	loadBalancerIP net.IP,
	epIP net.IP,
	svcType corev1.ServiceType,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var svc, updatedSvc *corev1.Service
	switch svcType {
	case corev1.ServiceTypeNodePort:
		svc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort+1), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
	case corev1.ServiceTypeLoadBalancer:
		svc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort+1), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	}
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(epIP.String(), "", "", svcPort, false, true, true, false, nil)}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)

		s1 := mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol)
		mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		s2 := mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort+1), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort+1), bindingProtocol).Times(1)
		s2.After(s1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServiceNodePortUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("NodePort", func(t *testing.T) {
			testServiceNodePortUpdate(t, nodePortAddressesIPv4, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeNodePort, false)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceNodePortUpdate(t, nodePortAddressesIPv4, svc1IPv4, loadBalancerIPv4, ep1IPv4, corev1.ServiceTypeLoadBalancer, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("NodePort", func(t *testing.T) {
			testServiceNodePortUpdate(t, nodePortAddressesIPv6, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeNodePort, true)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceNodePortUpdate(t, nodePortAddressesIPv6, svc1IPv6, loadBalancerIPv6, ep1IPv6, corev1.ServiceTypeLoadBalancer, true)
		})
	})
}

func testServiceExternalTrafficPolicyUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	loadBalancerIP net.IP,
	ep1IP net.IP,
	ep2IP net.IP,
	svcType corev1.ServiceType,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var svc, updatedSvc *corev1.Service
	switch svcType {
	case corev1.ServiceTypeNodePort:
		svc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeLocal)
	case corev1.ServiceTypeLoadBalancer:
		svc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeLocal)
	}
	makeServiceMap(fp, svc)

	remoteEp, remoteEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
	localEp, localEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
	eps := makeTestEndpointSlice(svcPortName.Namespace,
		svcPortName.Name,
		[]discovery.Endpoint{*remoteEp, *localEp},
		[]discovery.EndpointPort{*remoteEpPort, *localEpPort},
		isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedLocalEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep2IP.String(), hostname, "", svcPort, true, true, true, false, nil)}
	expectedAllEps := append(expectedLocalEps, k8sproxy.NewBaseEndpointInfo(ep1IP.String(), "", "", svcPort, false, true, true, false, nil))

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedAllEps)).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)

	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	groupIDLocal := fp.groupCounter.AllocateIfNotExist(svcPortName, true)

	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
		mockOFClient.EXPECT().InstallServiceGroup(groupIDLocal, false, expectedLocalEps).Times(1)
		s1 := mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
		s2 := mockOFClient.EXPECT().InstallServiceFlows(groupIDLocal, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		s2.After(s1)

		mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		s1 := mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), bindingProtocol).Times(1)
		s2 := mockOFClient.EXPECT().InstallServiceFlows(groupIDLocal, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		s2.After(s1)

		mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServiceExternalTrafficPolicyUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("NodePort", func(t *testing.T) {
			testServiceExternalTrafficPolicyUpdate(t, nodePortAddressesIPv4, svc1IPv4, nil, ep1IPv4, ep2IPv4, corev1.ServiceTypeNodePort, false)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceExternalTrafficPolicyUpdate(t, nodePortAddressesIPv4, svc1IPv4, loadBalancerIPv4, ep1IPv4, ep2IPv4, corev1.ServiceTypeLoadBalancer, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("NodePort", func(t *testing.T) {
			testServiceExternalTrafficPolicyUpdate(t, nodePortAddressesIPv6, svc1IPv6, nil, ep1IPv6, ep2IPv6, corev1.ServiceTypeNodePort, true)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceExternalTrafficPolicyUpdate(t, nodePortAddressesIPv6, svc1IPv6, loadBalancerIPv6, ep1IPv6, ep2IPv6, corev1.ServiceTypeLoadBalancer, true)
		})
	})
}

func testServiceInternalTrafficPolicyUpdate(t *testing.T,
	svcIP net.IP,
	ep1IP net.IP,
	ep2IP net.IP,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, isIPv6, withProxyAll)

	internalTrafficPolicyCluster := corev1.ServiceInternalTrafficPolicyCluster
	internalTrafficPolicyLocal := corev1.ServiceInternalTrafficPolicyLocal

	svc := makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, &internalTrafficPolicyCluster, false, nil)
	updatedSvc := makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, &internalTrafficPolicyLocal, false, nil)
	makeServiceMap(fp, svc)

	remoteEp, remoteEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IP, int32(svcPort), corev1.ProtocolTCP, false)
	localEp, localEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep2IP, int32(svcPort), corev1.ProtocolTCP, true)
	endpointSlice := makeTestEndpointSlice(svcPortName.Namespace,
		svcPortName.Name,
		[]discovery.Endpoint{*remoteEp, *localEp},
		[]discovery.EndpointPort{*remoteEpPort, *localEpPort},
		isIPv6)
	makeEndpointSliceMap(fp, endpointSlice)

	expectedLocalEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep2IP.String(), hostname, "", svcPort, true, true, true, false, nil)}
	expectedRemoteEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(ep1IP.String(), "", "", svcPort, false, true, true, false, nil)}
	expectedAllEps := append(expectedLocalEps, expectedRemoteEps...)

	bindingProtocol := binding.ProtocolTCP
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedAllEps)).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedAllEps)).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)

	assertEndpoints := func(t *testing.T, expectedEndpoints []k8sproxy.Endpoint, gotEndpoints map[string]k8sproxy.Endpoint) {
		var endpoints []k8sproxy.Endpoint
		for _, e := range gotEndpoints {
			endpoints = append(endpoints, e)
		}
		assert.ElementsMatch(t, expectedEndpoints, endpoints)
	}

	svcEndpointsMap, ok := fp.endpointsInstalledMap[svcPortName]
	assert.True(t, ok)
	assertEndpoints(t, expectedAllEps, svcEndpointsMap)

	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	groupIDLocal := fp.groupCounter.AllocateIfNotExist(svcPortName, true)

	mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, expectedRemoteEps).Times(1)
	mockOFClient.EXPECT().UninstallServiceGroup(groupID).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupIDLocal, false, expectedLocalEps).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupIDLocal, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	fp.syncProxyRules()

	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	svcEndpointsMap, ok = fp.endpointsInstalledMap[svcPortName]
	assert.True(t, ok)
	assertEndpoints(t, expectedLocalEps, svcEndpointsMap)
}

func TestServiceInternalTrafficPolicyUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceInternalTrafficPolicyUpdate(t, svc1IPv4, ep1IPv4, ep2IPv4, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceInternalTrafficPolicyUpdate(t, svc1IPv6, ep1IPv6, ep2IPv6, true)
		})
	})
}

func testServiceIngressIPsUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	epIP net.IP,
	loadBalancerIPs []net.IP,
	updatedLoadBalancerIPs []net.IP,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var loadBalancerIPStrs, updatedLoadBalancerIPStrs []string
	for _, ip := range loadBalancerIPs {
		loadBalancerIPStrs = append(loadBalancerIPStrs, ip.String())
	}
	for _, ip := range updatedLoadBalancerIPs {
		updatedLoadBalancerIPStrs = append(updatedLoadBalancerIPStrs, ip.String())
	}

	svc := makeTestLoadBalancerService(&svcPortName, svcIP, nil, loadBalancerIPs, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	updatedSvc := makeTestLoadBalancerService(&svcPortName, svcIP, nil, updatedLoadBalancerIPs, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(epIP.String(), "", "", svcPort, false, true, true, false, nil)}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.InAnyOrder(expectedEps)).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, gomock.InAnyOrder(expectedEps)).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
	for _, ip := range loadBalancerIPs {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), ip, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
	}
	mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	for _, ip := range loadBalancerIPs {
		mockRouteClient.EXPECT().AddExternalIPRoute(ip).Times(1)
	}

	toDeleteLoadBalancerIPs := smallSliceDifference(loadBalancerIPStrs, updatedLoadBalancerIPStrs)
	toAddLoadBalancerIPs := smallSliceDifference(updatedLoadBalancerIPStrs, loadBalancerIPStrs)
	for _, ipStr := range toDeleteLoadBalancerIPs {
		mockOFClient.EXPECT().UninstallServiceFlows(net.ParseIP(ipStr), uint16(svcPort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(net.ParseIP(ipStr)).Times(1)
	}
	for _, ipStr := range toAddLoadBalancerIPs {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), net.ParseIP(ipStr), uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(net.ParseIP(ipStr)).Times(1)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServiceIngressIPsUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("LoadBalancer", func(t *testing.T) {
			loadBalancerIPs := []net.IP{net.ParseIP("169.254.1.1"), net.ParseIP("169.254.1.2")}
			updatedLoadBalancerIPs := []net.IP{net.ParseIP("169.254.1.2"), net.ParseIP("169.254.1.3")}
			testServiceIngressIPsUpdate(t, nodePortAddressesIPv4, svc1IPv4, ep1IPv4, loadBalancerIPs, updatedLoadBalancerIPs, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("LoadBalancer", func(t *testing.T) {
			loadBalancerIPs := []net.IP{net.ParseIP("fec0::169:254:1:1"), net.ParseIP("fec0::169:254:1:2")}
			updatedLoadBalancerIPs := []net.IP{net.ParseIP("fec0::169:254:1:2"), net.ParseIP("fec0::169:254:1:3")}
			testServiceIngressIPsUpdate(t, nodePortAddressesIPv6, svc1IPv6, ep1IPv6, loadBalancerIPs, updatedLoadBalancerIPs, true)
		})
	})
}

func testServiceStickyMaxAgeSecondsUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	loadBalancerIP net.IP,
	epIP net.IP,
	svcType corev1.ServiceType,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var svc, updatedSvc *corev1.Service
	affinitySeconds := int32(10)
	updatedAffinitySeconds := int32(100)
	switch svcType {
	case corev1.ServiceTypeClusterIP:
		svc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, &affinitySeconds, nil, false, nil)
		updatedSvc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, &updatedAffinitySeconds, nil, false, nil)
	case corev1.ServiceTypeNodePort:
		svc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, &affinitySeconds, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, &updatedAffinitySeconds, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
	case corev1.ServiceTypeLoadBalancer:
		svc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, &affinitySeconds, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, &updatedAffinitySeconds, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	}
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(epIP.String(), "", "", svcPort, false, true, true, false, nil)}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, true, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(affinitySeconds), false, false).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(updatedAffinitySeconds), false, false).Times(1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(affinitySeconds), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(updatedAffinitySeconds), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(affinitySeconds), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
		mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(updatedAffinitySeconds), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServiceStickyMaxAgeSecondsUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceStickyMaxAgeSecondsUpdate(t, nil, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeClusterIP, false)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServiceStickyMaxAgeSecondsUpdate(t, nodePortAddressesIPv4, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeNodePort, false)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceStickyMaxAgeSecondsUpdate(t, nodePortAddressesIPv4, svc1IPv4, loadBalancerIPv4, ep1IPv4, corev1.ServiceTypeLoadBalancer, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceStickyMaxAgeSecondsUpdate(t, nil, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeClusterIP, true)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServiceStickyMaxAgeSecondsUpdate(t, nodePortAddressesIPv6, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeNodePort, true)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceStickyMaxAgeSecondsUpdate(t, nodePortAddressesIPv6, svc1IPv6, loadBalancerIPv6, ep1IPv6, corev1.ServiceTypeLoadBalancer, true)
		})
	})
}

func testServiceSessionAffinityTypeUpdate(t *testing.T,
	nodePortAddresses []net.IP,
	svcIP net.IP,
	loadBalancerIP net.IP,
	epIP net.IP,
	svcType corev1.ServiceType,
	isIPv6 bool) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddresses, groupAllocator, isIPv6, withProxyAll)

	var svc, updatedSvc *corev1.Service
	affinitySeconds := int32(100)
	switch svcType {
	case corev1.ServiceTypeClusterIP:
		svc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
		updatedSvc = makeTestClusterIPService(&svcPortName, svcIP, nil, int32(svcPort), corev1.ProtocolTCP, &affinitySeconds, nil, false, nil)
	case corev1.ServiceTypeNodePort:
		svc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestNodePortService(&svcPortName, svcIP, nil, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, &affinitySeconds, corev1.ServiceInternalTrafficPolicyCluster, corev1.ServiceExternalTrafficPolicyTypeCluster)
	case corev1.ServiceTypeLoadBalancer:
		svc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, nil, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
		updatedSvc = makeTestLoadBalancerService(&svcPortName, svcIP, nil, []net.IP{loadBalancerIP}, int32(svcPort), int32(svcNodePort), corev1.ProtocolTCP, &affinitySeconds, nil, corev1.ServiceExternalTrafficPolicyTypeCluster)
	}
	makeServiceMap(fp, svc)

	ep, epPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, epIP, int32(svcPort), corev1.ProtocolTCP, false)
	eps := makeTestEndpointSlice(svcPortName.Namespace, svcPortName.Name, []discovery.Endpoint{*ep}, []discovery.EndpointPort{*epPort}, isIPv6)
	makeEndpointSliceMap(fp, eps)

	expectedEps := []k8sproxy.Endpoint{k8sproxy.NewBaseEndpointInfo(epIP.String(), "", "", svcPort, false, true, true, false, nil)}

	bindingProtocol := binding.ProtocolTCP
	vIP := agentconfig.VirtualNodePortDNATIPv4
	if isIPv6 {
		bindingProtocol = binding.ProtocolTCPv6
		vIP = agentconfig.VirtualNodePortDNATIPv6
	}

	groupID := fp.groupCounter.AllocateIfNotExist(svcPortName, false)
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID, false, expectedEps).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)

	mockOFClient.EXPECT().InstallServiceGroup(groupID, true, expectedEps).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svcIP, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), svcIP, uint16(svcPort), bindingProtocol, uint16(affinitySeconds), false, false).Times(1)

	if svcType == corev1.ServiceTypeNodePort || svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)

		mockOFClient.EXPECT().UninstallServiceFlows(vIP, uint16(svcNodePort), bindingProtocol).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), vIP, uint16(svcNodePort), bindingProtocol, uint16(affinitySeconds), true, false).Times(1)
		mockRouteClient.EXPECT().DeleteNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
		mockRouteClient.EXPECT().AddNodePort(nodePortAddresses, uint16(svcNodePort), bindingProtocol).Times(1)
	}
	if svcType == corev1.ServiceTypeLoadBalancer {
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(0), true, false).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)

		mockOFClient.EXPECT().UninstallServiceFlows(loadBalancerIP, uint16(svcPort), bindingProtocol)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, gomock.Any(), loadBalancerIP, uint16(svcPort), bindingProtocol, uint16(affinitySeconds), true, false).Times(1)
		mockRouteClient.EXPECT().DeleteExternalIPRoute(loadBalancerIP).Times(1)
		mockRouteClient.EXPECT().AddExternalIPRoute(loadBalancerIP).Times(1)
	}

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
	fp.serviceChanges.OnServiceUpdate(svc, updatedSvc)
	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName)
}

func TestServiceSessionAffinityTypeUpdate(t *testing.T) {
	t.Run("IPv4", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceSessionAffinityTypeUpdate(t, nil, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeClusterIP, false)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServiceSessionAffinityTypeUpdate(t, nodePortAddressesIPv4, svc1IPv4, nil, ep1IPv4, corev1.ServiceTypeNodePort, false)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceSessionAffinityTypeUpdate(t, nodePortAddressesIPv4, svc1IPv4, loadBalancerIPv4, ep1IPv4, corev1.ServiceTypeLoadBalancer, false)
		})
	})
	t.Run("IPv6", func(t *testing.T) {
		t.Run("ClusterIP", func(t *testing.T) {
			testServiceSessionAffinityTypeUpdate(t, nil, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeClusterIP, true)
		})
		t.Run("NodePort", func(t *testing.T) {
			testServiceSessionAffinityTypeUpdate(t, nodePortAddressesIPv6, svc1IPv6, nil, ep1IPv6, corev1.ServiceTypeNodePort, true)
		})
		t.Run("LoadBalancer", func(t *testing.T) {
			testServiceSessionAffinityTypeUpdate(t, nodePortAddressesIPv6, svc1IPv6, loadBalancerIPv6, ep1IPv6, corev1.ServiceTypeLoadBalancer, true)
		})
	})
}

func TestServicesWithSameEndpoints(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, false)

	svcPortName1 := makeSvcPortName("ns", "svc1", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svcPortName2 := makeSvcPortName("ns", "svc2", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svc1 := makeTestClusterIPService(&svcPortName1, svc1IPv4, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	svc2 := makeTestClusterIPService(&svcPortName2, svc2IPv4, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	makeServiceMap(fp, svc1, svc2)

	ep1, ep1Port := makeTestEndpointSliceEndpointAndPort(&svcPortName1, ep1IPv4, int32(svcPort), corev1.ProtocolTCP, false)
	eps1 := makeTestEndpointSlice(svcPortName1.Namespace, svcPortName1.Name, []discovery.Endpoint{*ep1}, []discovery.EndpointPort{*ep1Port}, false)
	ep2, ep2Port := makeTestEndpointSliceEndpointAndPort(&svcPortName2, ep1IPv4, int32(svcPort), corev1.ProtocolTCP, false)
	eps2 := makeTestEndpointSlice(svcPortName2.Namespace, svcPortName2.Name, []discovery.Endpoint{*ep2}, []discovery.EndpointPort{*ep2Port}, false)
	makeEndpointSliceMap(fp, eps1, eps2)

	groupID1 := fp.groupCounter.AllocateIfNotExist(svcPortName1, false)
	groupID2 := fp.groupCounter.AllocateIfNotExist(svcPortName2, false)
	mockOFClient.EXPECT().InstallServiceGroup(groupID1, false, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceGroup(groupID2, false, gomock.Any()).Times(1)
	bindingProtocol := binding.ProtocolTCP
	mockOFClient.EXPECT().InstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID1, binding.GroupIDType(0), svc1IPv4, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().InstallServiceFlows(groupID2, binding.GroupIDType(0), svc2IPv4, uint16(svcPort), bindingProtocol, uint16(0), false, false).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svc1IPv4, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceFlows(svc2IPv4, uint16(svcPort), bindingProtocol).Times(1)
	mockOFClient.EXPECT().UninstallServiceGroup(groupID1).Times(1)
	mockOFClient.EXPECT().UninstallServiceGroup(groupID2).Times(1)
	// Since these two Services reference to the same Endpoint, there should only be one operation.
	mockOFClient.EXPECT().UninstallEndpointFlows(bindingProtocol, gomock.Any()).Times(1)

	fp.syncProxyRules()
	assert.Contains(t, fp.serviceInstalledMap, svcPortName1)
	assert.Contains(t, fp.serviceInstalledMap, svcPortName2)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName1)
	assert.Contains(t, fp.endpointsInstalledMap, svcPortName2)

	fp.serviceChanges.OnServiceUpdate(svc1, nil)
	fp.endpointsChanges.OnEndpointSliceUpdate(eps1, true)
	fp.syncProxyRules()
	assert.NotContains(t, fp.serviceInstalledMap, svcPortName1)
	assert.NotContains(t, fp.endpointsInstalledMap, svcPortName1)

	fp.serviceChanges.OnServiceUpdate(svc2, nil)
	fp.endpointsChanges.OnEndpointSliceUpdate(eps2, true)
	fp.syncProxyRules()
	assert.NotContains(t, fp.serviceInstalledMap, svcPortName2)
	assert.NotContains(t, fp.endpointsInstalledMap, svcPortName2)
}

func TestMetrics(t *testing.T) {
	legacyregistry.Reset()
	metrics.Register()

	for _, tc := range []struct {
		name                string
		svcIP, ep1IP, ep2IP net.IP
		isIPv6              bool
	}{
		{"IPv4", svc1IPv4, ep1IPv4, ep2IPv4, false},
		{"IPv6", svc1IPv6, ep1IPv6, ep2IPv6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpointsUpdateTotalMetric := metrics.EndpointsUpdatesTotal.CounterMetric
			servicesUpdateTotalMetric := metrics.ServicesUpdatesTotal.CounterMetric
			endpointsInstallMetric := metrics.EndpointsInstalledTotal.GaugeMetric
			servicesInstallMetric := metrics.ServicesInstalledTotal.GaugeMetric
			if tc.isIPv6 {
				endpointsUpdateTotalMetric = metrics.EndpointsUpdatesTotalV6.CounterMetric
				servicesUpdateTotalMetric = metrics.ServicesUpdatesTotalV6.CounterMetric
				endpointsInstallMetric = metrics.EndpointsInstalledTotalV6.GaugeMetric
				servicesInstallMetric = metrics.ServicesInstalledTotalV6.GaugeMetric
			}

			testClusterIPAdd(t, tc.svcIP, nil, tc.ep1IP, tc.ep2IP, tc.isIPv6, false, []*corev1.Service{}, []*corev1.Endpoints{}, true)
			v, err := testutil.GetCounterMetricValue(endpointsUpdateTotalMetric)
			assert.NoError(t, err)
			assert.Equal(t, 0, int(v))
			v, err = testutil.GetCounterMetricValue(servicesUpdateTotalMetric)
			assert.Equal(t, 0, int(v))
			assert.NoError(t, err)
			v, err = testutil.GetGaugeMetricValue(servicesInstallMetric)
			assert.Equal(t, 1, int(v))
			assert.NoError(t, err)
			v, err = testutil.GetGaugeMetricValue(endpointsInstallMetric)
			assert.Equal(t, 2, int(v))
			assert.NoError(t, err)

			testClusterIPRemove(t, tc.svcIP, nil, tc.ep1IP, tc.isIPv6, false, false)

			v, err = testutil.GetCounterMetricValue(endpointsUpdateTotalMetric)
			assert.NoError(t, err)
			assert.Equal(t, 0, int(v))
			v, err = testutil.GetCounterMetricValue(servicesUpdateTotalMetric)
			assert.Equal(t, 0, int(v))
			assert.NoError(t, err)
			v, err = testutil.GetGaugeMetricValue(servicesInstallMetric)
			assert.Equal(t, 0, int(v))
			assert.NoError(t, err)
			v, err = testutil.GetGaugeMetricValue(endpointsInstallMetric)
			assert.Equal(t, 0, int(v))
			assert.NoError(t, err)
		})
	}
}

func TestGetServiceFlowKeys(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	svc := makeTestNodePortService(&svcPortName,
		svc1IPv4,
		nil,
		int32(svcPort),
		int32(svcNodePort),
		corev1.ProtocolTCP,
		nil,
		corev1.ServiceInternalTrafficPolicyLocal,
		corev1.ServiceExternalTrafficPolicyTypeCluster)

	remoteEp, remoteEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IPv4, int32(svcPort), corev1.ProtocolTCP, false)
	localEp, localEpPort := makeTestEndpointSliceEndpointAndPort(&svcPortName, ep1IPv4, int32(svcPort), corev1.ProtocolTCP, true)
	eps := makeTestEndpointSlice(svcPortName.Namespace,
		svcPortName.Name,
		[]discovery.Endpoint{*remoteEp, *localEp},
		[]discovery.EndpointPort{*remoteEpPort, *localEpPort},
		false)

	testCases := []struct {
		name             string
		svc              *corev1.Service
		eps              *discovery.EndpointSlice
		serviceInstalled bool
		expectedFound    bool
	}{
		{
			name:             "Installed Service with Endpoints",
			svc:              svc,
			eps:              eps,
			serviceInstalled: true,
			expectedFound:    true,
		},
		{
			name:             "Not installed Service without Endpoints",
			svc:              svc,
			serviceInstalled: false,
			expectedFound:    false,
		},
		{
			name:             "Not installed Service with Endpoints",
			svc:              svc,
			eps:              eps,
			serviceInstalled: false,
			expectedFound:    false,
		},
		{
			name:             "Not existing Service",
			serviceInstalled: false,
			expectedFound:    false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFakeProxier(mockRouteClient, mockOFClient, nodePortAddressesIPv4, groupAllocator, false, withProxyAll)
			if tc.svc != nil {
				makeServiceMap(fp, svc)
			}
			if tc.eps != nil {
				makeEndpointSliceMap(fp, eps)
			}
			if tc.svc != nil && tc.eps != nil && tc.serviceInstalled {
				mockRouteClient.EXPECT().AddNodePort(nodePortAddressesIPv4, uint16(svcNodePort), binding.ProtocolTCP).Times(1)
				mockOFClient.EXPECT().InstallServiceGroup(gomock.Any(), gomock.Any(), gomock.Any()).Times(2)
				mockOFClient.EXPECT().InstallEndpointFlows(binding.ProtocolTCP, gomock.Any()).Times(1)
				mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), gomock.Any(), uint16(svcNodePort), binding.ProtocolTCP, uint16(0), true, false).Times(1)
				mockOFClient.EXPECT().InstallServiceFlows(gomock.Any(), gomock.Any(), svc1IPv4, uint16(svcPort), binding.ProtocolTCP, uint16(0), false, false).Times(1)
				fp.syncProxyRules()
			}

			var expectedGroupIDs []binding.GroupIDType
			if tc.serviceInstalled {
				expectedGroupIDs = append(expectedGroupIDs, fp.groupCounter.AllocateIfNotExist(svcPortName, false))
				expectedGroupIDs = append(expectedGroupIDs, fp.groupCounter.AllocateIfNotExist(svcPortName, true))
				mockOFClient.EXPECT().GetServiceFlowKeys(svc1IPv4, uint16(svcPort), binding.ProtocolTCP, gomock.Any()).Times(1)
			}

			_, groupIDs, found := fp.GetServiceFlowKeys("svc", "ns")
			assert.ElementsMatch(t, expectedGroupIDs, groupIDs)
			assert.Equal(t, tc.expectedFound, found)
		})
	}
}

func TestServiceLabelSelector(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOFClient, mockRouteClient := getMockClients(ctrl)
	groupAllocator := openflow.NewGroupAllocator()
	svcPortName1 := makeSvcPortName("ns", "svc1", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svcPortName2 := makeSvcPortName("ns", "svc2", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svcPortName3 := makeSvcPortName("ns", "svc3", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svcPortName4 := makeSvcPortName("ns", "svc4", strconv.Itoa(svcPort), corev1.ProtocolTCP)
	svc1IP := net.ParseIP("1.1.1.1")
	svc2IP := net.ParseIP("1.1.1.2")
	svc3IP := net.ParseIP("1.1.1.3")
	svc4IP := net.ParseIP("1.1.1.4")
	svc1 := makeTestClusterIPService(&svcPortName1, svc1IP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, nil)
	svc2 := makeTestClusterIPService(&svcPortName2, svc2IP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, map[string]string{labelServiceProxyName: testServiceProxyName})
	svc3 := makeTestClusterIPService(&svcPortName3, svc3IP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, map[string]string{labelServiceProxyName: "other"})
	svc4 := makeTestClusterIPService(&svcPortName4, svc4IP, nil, int32(svcPort), corev1.ProtocolTCP, nil, nil, false, map[string]string{corev1.IsHeadlessService: ""})

	// Service with label "service.kubernetes.io/headless" should be always ignored.

	// When ServiceProxyName is set, only the Service with the label "service.kubernetes.io/service-proxy-name=antrea"
	// should be processed. Other Services without the label "service.kubernetes.io/service-proxy-name=antrea" should
	// be ignored.
	t.Run("ServiceProxyName", func(t *testing.T) {
		fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, false, withServiceProxyNameSet)
		makeServiceMap(fp, svc1, svc2, svc3, svc4)
		makeEndpointSliceMap(fp)

		groupID := fp.groupCounter.AllocateIfNotExist(svcPortName2, false)
		mockOFClient.EXPECT().InstallServiceGroup(groupID, false, []k8sproxy.Endpoint{}).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svc2IP, uint16(svcPort), gomock.Any(), uint16(0), false, false).Times(1)
		fp.syncProxyRules()
		assert.Contains(t, fp.serviceInstalledMap, svcPortName2)
	})

	// When ServiceProxyName is not set, only the Services without the label "service.kubernetes.io/service-proxy-name"
	// should be processed. Other Services with the label "service.kubernetes.io/service-proxy-name" (regardless of
	// the value) should be ignored.
	t.Run("empty ServiceProxyName", func(t *testing.T) {
		fp := newFakeProxier(mockRouteClient, mockOFClient, nil, groupAllocator, false)
		makeServiceMap(fp, svc1, svc2, svc3, svc4)
		makeEndpointSliceMap(fp)

		groupID := fp.groupCounter.AllocateIfNotExist(svcPortName1, false)
		mockOFClient.EXPECT().InstallServiceGroup(groupID, false, []k8sproxy.Endpoint{}).Times(1)
		mockOFClient.EXPECT().InstallServiceFlows(groupID, binding.GroupIDType(0), svc1IP, uint16(svcPort), gomock.Any(), uint16(0), false, false).Times(1)
		fp.syncProxyRules()
		assert.Contains(t, fp.serviceInstalledMap, svcPortName1)
	})
}
