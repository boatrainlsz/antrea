// Copyright 2019 Antrea Authors
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

package cniserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/containernetworking/plugins/pkg/ip"
	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent/cniserver/ipam"
	"antrea.io/antrea/pkg/agent/cniserver/types"
	"antrea.io/antrea/pkg/agent/config"
	"antrea.io/antrea/pkg/agent/interfacestore"
	"antrea.io/antrea/pkg/agent/openflow"
	"antrea.io/antrea/pkg/agent/route"
	"antrea.io/antrea/pkg/agent/secondarynetwork/cnipodcache"
	"antrea.io/antrea/pkg/agent/util"
	cnipb "antrea.io/antrea/pkg/apis/cni/v1beta1"
	"antrea.io/antrea/pkg/cni"
	"antrea.io/antrea/pkg/ovs/ovsconfig"
	"antrea.io/antrea/pkg/util/channel"
)

const (
	antreaCNIType = "antrea"

	// networkReadyTimeout is the maximum time the CNI server will wait for network ready when processing CNI Add
	// requests. If timeout occurs, tryAgainLaterResponse will be returned.
	// The default runtime request timeout of kubelet is 2 minutes.
	// https://github.com/kubernetes/kubernetes/blob/v1.19.3/staging/src/k8s.io/kubelet/config/v1beta1/types.go#L451
	// networkReadyTimeout is set to a shorter time so it returns a clear message to the runtime.
	networkReadyTimeout = 30 * time.Second
)

// containerAccessArbitrator is used to ensure that concurrent goroutines cannot perfom operations
// on the same containerID. Other parts of the code make this assumption (in particular the
// InstallPodFlows / UninstallPodFlows methods of the OpenFlow client, which are invoked
// respectively by CmdAdd and CmdDel). The idea is to simply the locking requirements for the rest
// of the code by ensuring that all the requests for a given container are serialized.
type containerAccessArbitrator struct {
	mutex             sync.Mutex
	cond              *sync.Cond
	busyContainerKeys map[string]bool // used as a set of container keys
}

func newContainerAccessArbitrator() *containerAccessArbitrator {
	arbitrator := &containerAccessArbitrator{
		busyContainerKeys: make(map[string]bool),
	}
	arbitrator.cond = sync.NewCond(&arbitrator.mutex)
	return arbitrator
}

// lockContainer prevents other goroutines from accessing containerKey. If containerKey is already
// locked by another goroutine, this function will block until the container is available. Every
// call to lockContainer must be followed by a call to unlockContainer on the same containerKey.
func (arbitrator *containerAccessArbitrator) lockContainer(containerKey string) {
	arbitrator.cond.L.Lock()
	defer arbitrator.cond.L.Unlock()
	for {
		_, ok := arbitrator.busyContainerKeys[containerKey]
		if !ok {
			break
		}
		arbitrator.cond.Wait()
	}
	arbitrator.busyContainerKeys[containerKey] = true
}

// unlockContainer releases access to containerKey.
func (arbitrator *containerAccessArbitrator) unlockContainer(containerKey string) {
	arbitrator.cond.L.Lock()
	defer arbitrator.cond.L.Unlock()
	delete(arbitrator.busyContainerKeys, containerKey)
	arbitrator.cond.Broadcast()
}

type CNIServer struct {
	cniSocket            string
	supportedCNIVersions map[string]bool
	serverVersion        string
	nodeConfig           *config.NodeConfig
	hostProcPathPrefix   string
	kubeClient           clientset.Interface
	containerAccess      *containerAccessArbitrator
	podConfigurator      *podConfigurator
	routeClient          route.Interface
	isChaining           bool
	enableBridgingMode   bool
	// Enable AntreaIPAM for secondary networks implementd by other CNIs.
	enableSecondaryNetworkIPAM bool
	disableTXChecksumOffload   bool
	secondaryNetworkEnabled    bool
	networkConfig              *config.NetworkConfig
	// networkReadyCh notifies that the network is ready so new Pods can be created. Therefore, CmdAdd waits for it.
	networkReadyCh <-chan struct{}
}

var supportedCNIVersionSet map[string]bool

type CNIConfig struct {
	*types.NetworkConfig
	// AntreaIPAM for an interface not managed by Antrea CNI.
	secondaryNetworkIPAM bool
	// CniCmdArgs received from the CNI plugin. IPAM data in CniCmdArgs can be updated with the
	// Node's Pod CIDRs for NodeIPAM.
	*cnipb.CniCmdArgs
	// K8s CNI_ARGS passed to the CNI plugin.
	*types.K8sArgs
}

// updateResultIfaceConfig processes the result from the IPAM plugin and does the following:
//   - updates the IP configuration for each assigned IP address: this includes computing the
//     gateway (if missing) based on the subnet and setting the interface pointer to the container
//     interface
//   - if there is no default route, add one using the provided default gateway
func updateResultIfaceConfig(result *current.Result, defaultIPv4Gateway net.IP, defaultIPv6Gateway net.IP) {
	for _, ipc := range result.IPs {
		// result.Interfaces[0] is host interface, and result.Interfaces[1] is container interface
		ipc.Interface = current.Int(1)
		if ipc.Gateway == nil {
			ipn := ipc.Address
			netID := ipn.IP.Mask(ipn.Mask)
			ipc.Gateway = ip.NextIP(netID)
		}
	}

	foundV4DefaultRoute := false
	foundV6DefaultRoute := false
	defaultV4RouteDst := "0.0.0.0/0"
	defaultV6RouteDst := "::/0"
	if result.Routes != nil {
		for _, rt := range result.Routes {
			if rt.Dst.String() == defaultV4RouteDst {
				foundV4DefaultRoute = true
			} else if rt.Dst.String() == defaultV6RouteDst {
				foundV6DefaultRoute = true
			}
		}
	} else {
		result.Routes = []*cnitypes.Route{}
	}

	if (!foundV4DefaultRoute) && (defaultIPv4Gateway != nil) {
		_, defaultV4RouteDstNet, _ := net.ParseCIDR(defaultV4RouteDst)
		result.Routes = append(result.Routes, &cnitypes.Route{Dst: *defaultV4RouteDstNet, GW: defaultIPv4Gateway})
	}
	if (!foundV6DefaultRoute) && (defaultIPv6Gateway != nil) {
		_, defaultV6RouteDstNet, _ := net.ParseCIDR(defaultV6RouteDst)
		result.Routes = append(result.Routes, &cnitypes.Route{Dst: *defaultV6RouteDstNet, GW: defaultIPv6Gateway})
	}
}

func resultToResponse(result cnitypes.Result) *cnipb.CniCmdResponse {
	var resultBytes bytes.Buffer
	_ = result.PrintTo(&resultBytes)
	return &cnipb.CniCmdResponse{CniResult: resultBytes.Bytes()}
}

func (s *CNIServer) loadNetworkConfig(request *cnipb.CniCmdRequest) (*CNIConfig, error) {
	cniConfig := CNIConfig{}
	if err := json.Unmarshal(request.CniArgs.NetworkConfiguration, &cniConfig); err != nil {
		return nil, err
	}
	cniConfig.K8sArgs = &types.K8sArgs{}
	if err := cnitypes.LoadArgs(request.CniArgs.Args, cniConfig.K8sArgs); err != nil {
		return nil, err
	}
	if cniConfig.MTU == 0 {
		cniConfig.MTU = s.networkConfig.InterfaceMTU
	}
	cniConfig.CniCmdArgs = request.CniArgs
	klog.V(3).Infof("Load network configurations: %v", cniConfig)
	return &cniConfig, nil
}

func (s *CNIServer) isCNIVersionSupported(reqVersion string) bool {
	_, exist := s.supportedCNIVersions[reqVersion]
	return exist
}

func (s *CNIServer) validateCNIAndIPAMType(cniConfig *CNIConfig) *cnipb.CniCmdResponse {
	var ipamType string
	if cniConfig.IPAM != nil {
		ipamType = cniConfig.IPAM.Type
	}
	if cniConfig.Type == antreaCNIType {
		if s.isChaining {
			return nil
		}
		if !ipam.IsIPAMTypeValid(ipamType) {
			klog.Errorf("Unsupported IPAM type %s", ipamType)
			return s.unsupportedFieldResponse("ipam/type", ipamType)
		}
		if s.enableBridgingMode {
			// When the bridging mode is enabled, Antrea ignores IPAM type from request.
			cniConfig.IPAM.Type = ipam.AntreaIPAMType

		}
		return nil
	}

	if !s.enableSecondaryNetworkIPAM {
		return s.unsupportedFieldResponse("type", cniConfig.Type)
	}
	if ipamType != ipam.AntreaIPAMType {
		klog.Errorf("Unsupported IPAM type %s", ipamType)
		return s.unsupportedFieldResponse("ipam/type", ipamType)
	}
	// IPAM for an interface not managed by Antrea CNI.
	cniConfig.secondaryNetworkIPAM = true
	return nil
}

func (s *CNIServer) validateRequestMessage(request *cnipb.CniCmdRequest) (*CNIConfig, *cnipb.CniCmdResponse) {
	cniConfig, err := s.loadNetworkConfig(request)
	if err != nil {
		klog.Errorf("Failed to parse network configuration: %v", err)
		return nil, s.decodingFailureResponse("network config")
	}

	cniVersion := cniConfig.CNIVersion
	// Check if CNI version in the request is supported
	if !s.isCNIVersionSupported(cniVersion) {
		klog.Errorf(fmt.Sprintf("Unsupported CNI version [%s], supported CNI versions %s", cniVersion, version.All.SupportedVersions()))
		return nil, s.incompatibleCniVersionResponse(cniVersion)
	}

	if resp := s.validateCNIAndIPAMType(cniConfig); resp != nil {
		return nil, resp
	}
	if !s.isChaining && !cniConfig.secondaryNetworkIPAM {
		s.updateLocalIPAMSubnet(cniConfig)
	}
	return cniConfig, nil
}

// updateLocalIPAMSubnet updates CNIConfig.CniCmdArgs with this Node's Pod CIDRs, which will be
// passed to the IPAM driver.
func (s *CNIServer) updateLocalIPAMSubnet(cniConfig *CNIConfig) {
	if (s.nodeConfig.GatewayConfig.IPv4 != nil) && (s.nodeConfig.PodIPv4CIDR != nil) {
		cniConfig.NetworkConfig.IPAM.Ranges = append(cniConfig.NetworkConfig.IPAM.Ranges,
			types.RangeSet{types.Range{Subnet: s.nodeConfig.PodIPv4CIDR.String(), Gateway: s.nodeConfig.GatewayConfig.IPv4.String()}})
	}
	if (s.nodeConfig.GatewayConfig.IPv6 != nil) && (s.nodeConfig.PodIPv6CIDR != nil) {
		cniConfig.NetworkConfig.IPAM.Ranges = append(cniConfig.NetworkConfig.IPAM.Ranges,
			types.RangeSet{types.Range{Subnet: s.nodeConfig.PodIPv6CIDR.String(), Gateway: s.nodeConfig.GatewayConfig.IPv6.String()}})
	}
	cniConfig.NetworkConfiguration, _ = json.Marshal(cniConfig.NetworkConfig)
}

func (s *CNIServer) generateCNIErrorResponse(cniErrorCode cnipb.ErrorCode, cniErrorMsg string) *cnipb.CniCmdResponse {
	return &cnipb.CniCmdResponse{
		Error: &cnipb.Error{
			Code:    cniErrorCode,
			Message: cniErrorMsg,
		},
	}
}

func (s *CNIServer) decodingFailureResponse(what string) *cnipb.CniCmdResponse {
	return s.generateCNIErrorResponse(
		cnipb.ErrorCode_DECODING_FAILURE,
		fmt.Sprintf("Failed to decode %s", what),
	)
}

func (s *CNIServer) incompatibleCniVersionResponse(cniVersion string) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_INCOMPATIBLE_CNI_VERSION
	cniErrorMsg := fmt.Sprintf("Unsupported CNI version [%s], supported versions %s", cniVersion, version.All.SupportedVersions())
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) unsupportedFieldResponse(key string, value interface{}) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_UNSUPPORTED_FIELD
	cniErrorMsg := fmt.Sprintf("Network configuration does not support key %s and value %v", key, value)
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) tryAgainLaterResponse() *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_TRY_AGAIN_LATER
	cniErrorMsg := "Server is busy, please retry later"
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) ipamFailureResponse(err error) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_IPAM_FAILURE
	cniErrorMsg := err.Error()
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) configInterfaceFailureResponse(err error) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_CONFIG_INTERFACE_FAILURE
	cniErrorMsg := err.Error()
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) checkInterfaceFailureResponse(err error) *cnipb.CniCmdResponse {
	cniErrorCode := cnipb.ErrorCode_CHECK_INTERFACE_FAILURE
	cniErrorMsg := err.Error()
	return s.generateCNIErrorResponse(cniErrorCode, cniErrorMsg)
}

func (s *CNIServer) invalidNetworkConfigResponse(msg string) *cnipb.CniCmdResponse {
	return s.generateCNIErrorResponse(
		cnipb.ErrorCode_INVALID_NETWORK_CONFIG,
		msg,
	)
}

func buildVersionSet() map[string]bool {
	versionSet := make(map[string]bool)
	for _, ver := range version.All.SupportedVersions() {
		versionSet[strings.Trim(ver, " ")] = true
	}
	return versionSet
}

func (s *CNIServer) parsePrevResultFromRequest(networkConfig *types.NetworkConfig) (*current.Result, *cnipb.CniCmdResponse) {
	if networkConfig.PrevResult == nil && networkConfig.RawPrevResult == nil {
		klog.Errorf("Previous network configuration not specified")
		return nil, s.unsupportedFieldResponse("prevResult", "")
	}

	if err := parsePrevResult(networkConfig); err != nil {
		klog.Errorf("Failed to parse previous network configuration")
		return nil, s.decodingFailureResponse("prevResult")
	}
	// Convert whatever the result was into the current Result type (for the current CNI
	// version)
	prevResult, err := current.NewResultFromResult(networkConfig.PrevResult)
	if err != nil {
		klog.Errorf("Failed to construct prevResult using previous network configuration")
		return nil, s.unsupportedFieldResponse("prevResult", networkConfig.PrevResult)
	}
	prevResult.CNIVersion = networkConfig.CNIVersion
	return prevResult, nil
}

// validatePrevResult validates container and host interfaces configuration
// the return value is nil if prevResult is valid
func (s *CNIServer) validatePrevResult(cfgArgs *cnipb.CniCmdArgs, prevResult *current.Result, sriovVFDeviceID string) *cnipb.CniCmdResponse {
	containerID := cfgArgs.ContainerId
	netNS := s.hostNetNsPath(cfgArgs.Netns)

	// Find interfaces from previous configuration
	containerIntf := parseContainerIfaceFromResults(cfgArgs, prevResult)
	if containerIntf == nil {
		klog.Errorf("Failed to find interface %s of container %s", cfgArgs.Ifname, containerID)
		return s.invalidNetworkConfigResponse("prevResult does not match network configuration")
	}
	if err := s.podConfigurator.checkInterfaces(
		containerID,
		netNS,
		containerIntf,
		prevResult,
		sriovVFDeviceID); err != nil {
		return s.checkInterfaceFailureResponse(err)
	}

	return nil
}

func (s *CNIServer) GetPodConfigurator() *podConfigurator {
	return s.podConfigurator
}

// Declared variables for testing
var (
	ipamSecondaryNetworkAdd   = ipam.SecondaryNetworkAdd
	ipamSecondaryNetworkDel   = ipam.SecondaryNetworkDel
	ipamSecondaryNetworkCheck = ipam.SecondaryNetworkCheck
)

// Antrea IPAM for secondary network.
func (s *CNIServer) ipamAdd(cniConfig *CNIConfig) (*cnipb.CniCmdResponse, error) {
	ipamResult, err := ipamSecondaryNetworkAdd(cniConfig.CniCmdArgs, cniConfig.K8sArgs, cniConfig.NetworkConfig)
	if err != nil {
		return s.ipamFailureResponse(err), nil
	}
	cniResult, _ := ipamResult.GetAsVersion(cniConfig.CNIVersion)
	klog.InfoS("Allocated IP addresses", "container", cniConfig.ContainerId, "result", ipamResult)
	return resultToResponse(cniResult), nil
}

func (s *CNIServer) ipamDel(cniConfig *CNIConfig) (*cnipb.CniCmdResponse, error) {
	if err := ipamSecondaryNetworkDel(cniConfig.CniCmdArgs, cniConfig.K8sArgs, cniConfig.NetworkConfig); err != nil {
		return s.ipamFailureResponse(err), nil
	}
	return &cnipb.CniCmdResponse{CniResult: []byte("")}, nil
}

func (s *CNIServer) ipamCheck(cniConfig *CNIConfig) (*cnipb.CniCmdResponse, error) {
	if err := ipamSecondaryNetworkCheck(cniConfig.CniCmdArgs, cniConfig.K8sArgs, cniConfig.NetworkConfig); err != nil {
		return s.ipamFailureResponse(err), nil
	}
	// CNI CHECK is not implemented for secondary network IPAM, and so the func will always
	// return an error, but never reach here.
	return &cnipb.CniCmdResponse{CniResult: []byte("")}, nil
}

func (s *CNIServer) CmdAdd(ctx context.Context, request *cnipb.CniCmdRequest) (*cnipb.CniCmdResponse, error) {
	klog.Infof("Received CmdAdd request %v", request)
	cniConfig, response := s.validateRequestMessage(request)
	if response != nil {
		return response, nil
	}

	infraContainer := cniConfig.getInfraContainer()
	if cniConfig.secondaryNetworkIPAM {
		klog.InfoS("Antrea IPAM add", "CNI", cniConfig.Type, "network", cniConfig.Name)
		s.containerAccess.lockContainer(infraContainer)
		resp, err := s.ipamAdd(cniConfig)
		s.containerAccess.unlockContainer(infraContainer)
		return resp, err
	}

	select {
	case <-time.After(networkReadyTimeout):
		klog.Errorf("Cannot process CmdAdd request for container %v because network is not ready", cniConfig.ContainerId)
		return s.tryAgainLaterResponse(), nil
	case <-s.networkReadyCh:
	}

	result := &ipam.IPAMResult{Result: current.Result{CNIVersion: current.ImplementedSpecVersion}}
	netNS := s.hostNetNsPath(cniConfig.Netns)
	isInfraContainer := isInfraContainer(netNS)

	success := false
	defer func() {
		// Rollback to delete configurations once ADD is failure.
		if !success {
			if isInfraContainer {
				klog.Warningf("CmdAdd for container %v failed, and try to rollback", cniConfig.ContainerId)
				if _, err := s.CmdDel(ctx, request); err != nil {
					klog.Warningf("Failed to rollback after CNI add failure: %v", err)
				}
			} else {
				klog.Warningf("CmdAdd for container %v failed", cniConfig.ContainerId)
			}
		}
	}()

	// Serialize CNI calls for one Pod.
	s.containerAccess.lockContainer(infraContainer)
	defer s.containerAccess.unlockContainer(infraContainer)

	if s.isChaining {
		resp, err := s.interceptAdd(cniConfig)
		if err == nil {
			success = true
		}
		return resp, err
	}

	var ipamResult *ipam.IPAMResult
	var err error
	// Only allocate IP when handling CNI request from infra container.
	// On Windows platform, CNI plugin is called for all containers in a Pod.
	if !isInfraContainer {
		if ipamResult, _ = ipam.GetIPFromCache(infraContainer); ipamResult == nil {
			return nil, fmt.Errorf("allocated IP address not found")
		}
	} else {
		// Request IP Address from IPAM driver.
		ipamResult, err = ipam.ExecIPAMAdd(cniConfig.CniCmdArgs, cniConfig.K8sArgs, cniConfig.IPAM.Type, infraContainer)
		if err != nil {
			klog.Errorf("Failed to request IP addresses for container %v: %v", cniConfig.ContainerId, err)
			return s.ipamFailureResponse(err), nil
		}
	}
	klog.InfoS("Allocated IP addresses", "container", cniConfig.ContainerId, "result", ipamResult)
	result.IPs = ipamResult.IPs
	result.Routes = ipamResult.Routes
	result.VLANID = ipamResult.VLANID
	// Ensure interface gateway setting and mapping relations between result.Interfaces and result.IPs
	updateResultIfaceConfig(&result.Result, s.nodeConfig.GatewayConfig.IPv4, s.nodeConfig.GatewayConfig.IPv6)
	updateResultDNSConfig(&result.Result, cniConfig)

	// Setup pod interfaces and connect to ovs bridge
	podName := string(cniConfig.K8S_POD_NAME)
	podNamespace := string(cniConfig.K8S_POD_NAMESPACE)
	if err = s.podConfigurator.configureInterfaces(
		podName,
		podNamespace,
		cniConfig.ContainerId,
		netNS,
		cniConfig.Ifname,
		cniConfig.MTU,
		cniConfig.DeviceID,
		result,
		isInfraContainer,
		s.containerAccess,
	); err != nil {
		klog.Errorf("Failed to configure interfaces for container %s: %v", cniConfig.ContainerId, err)
		return s.configInterfaceFailureResponse(err), nil
	}
	cniVersion := cniConfig.CNIVersion
	cniResult, _ := result.Result.GetAsVersion(cniVersion)

	klog.Infof("CmdAdd for container %v succeeded", cniConfig.ContainerId)
	// mark success as true to avoid rollback
	success = true

	if s.secondaryNetworkEnabled {
		// Go cache the CNI server info at CNIConfigInfo cache, for podWatch usage
		cniInfo := &cnipodcache.CNIConfigInfo{CNIVersion: cniVersion, PodName: podName, PodNameSpace: podNamespace,
			ContainerID: cniConfig.ContainerId, ContainerNetNS: netNS, PodCNIDeleted: false,
			MTU: cniConfig.MTU}
		s.podConfigurator.podInfoStore.AddCNIConfigInfo(cniInfo)
	}

	return resultToResponse(cniResult), nil
}

func (s *CNIServer) CmdDel(_ context.Context, request *cnipb.CniCmdRequest) (
	*cnipb.CniCmdResponse, error) {
	klog.Infof("Received CmdDel request %v", request)

	cniConfig, response := s.validateRequestMessage(request)
	if response != nil {
		return response, nil
	}

	infraContainer := cniConfig.getInfraContainer()
	s.containerAccess.lockContainer(infraContainer)
	defer s.containerAccess.unlockContainer(infraContainer)

	if cniConfig.secondaryNetworkIPAM {
		klog.InfoS("Antrea IPAM del", "CNI", cniConfig.Type, "network", cniConfig.Name)
		return s.ipamDel(cniConfig)
	}

	if s.isChaining {
		return s.interceptDel(cniConfig)
	}
	// Release IP to IPAM driver
	if err := ipam.ExecIPAMDelete(cniConfig.CniCmdArgs, cniConfig.K8sArgs, cniConfig.IPAM.Type, infraContainer); err != nil {
		klog.Errorf("Failed to delete IP addresses for container %v: %v", cniConfig.ContainerId, err)
		return s.ipamFailureResponse(err), nil
	}
	klog.Infof("Deleted IP addresses for container %v", cniConfig.ContainerId)
	// Remove host interface and OVS configuration
	if err := s.podConfigurator.removeInterfaces(cniConfig.ContainerId); err != nil {
		klog.Errorf("Failed to remove interfaces for container %s: %v", cniConfig.ContainerId, err)
		return s.configInterfaceFailureResponse(err), nil
	}
	klog.Infof("CmdDel for container %v succeeded", cniConfig.ContainerId)
	if s.secondaryNetworkEnabled {
		podName := string(cniConfig.K8S_POD_NAME)
		podNamespace := string(cniConfig.K8S_POD_NAMESPACE)
		containerInfo := s.podConfigurator.podInfoStore.GetCNIConfigInfoByContainerID(podName, podNamespace, cniConfig.ContainerId)
		if containerInfo != nil {
			// Update PodCNIDeleted = true.
			// This is to let Podwatch controller know that the CNI server cleaned up this Pod's primary network configuration.
			s.podConfigurator.podInfoStore.SetPodCNIDeleted(containerInfo)
		}
	}
	return &cnipb.CniCmdResponse{CniResult: []byte("")}, nil
}

func (s *CNIServer) CmdCheck(_ context.Context, request *cnipb.CniCmdRequest) (
	*cnipb.CniCmdResponse, error) {
	klog.Infof("Received CmdCheck request %v", request)

	cniConfig, response := s.validateRequestMessage(request)
	if response != nil {
		return response, nil
	}

	infraContainer := cniConfig.getInfraContainer()
	s.containerAccess.lockContainer(infraContainer)
	defer s.containerAccess.unlockContainer(infraContainer)

	if cniConfig.secondaryNetworkIPAM {
		klog.InfoS("Antrea IPAM check", "CNI", cniConfig.Type, "network", cniConfig.Name)
		return s.ipamCheck(cniConfig)
	}

	if s.isChaining {
		return s.interceptCheck(cniConfig)
	}

	if err := ipam.ExecIPAMCheck(cniConfig.CniCmdArgs, cniConfig.K8sArgs, cniConfig.IPAM.Type); err != nil {
		klog.Errorf("Failed to check IPAM configuration for container %v: %v", cniConfig.ContainerId, err)
		return s.ipamFailureResponse(err), nil
	}

	cniVersion := cniConfig.CNIVersion
	if valid, _ := version.GreaterThanOrEqualTo(cniVersion, "0.4.0"); valid {
		if prevResult, response := s.parsePrevResultFromRequest(cniConfig.NetworkConfig); response != nil {
			return response, nil
		} else if response := s.validatePrevResult(cniConfig.CniCmdArgs, prevResult, cniConfig.DeviceID); response != nil {
			return response, nil
		}
	}
	klog.Infof("CmdCheck for container %v succeeded", cniConfig.ContainerId)
	return &cnipb.CniCmdResponse{CniResult: []byte("")}, nil
}

func New(
	cniSocket, hostProcPathPrefix string,
	nodeConfig *config.NodeConfig,
	kubeClient clientset.Interface,
	routeClient route.Interface,
	isChaining, enableBridgingMode, enableSecondaryNetworkIPAM, disableTXChecksumOffload bool,
	networkConfig *config.NetworkConfig,
	networkReadyCh <-chan struct{},
) *CNIServer {
	return &CNIServer{
		cniSocket:                  cniSocket,
		supportedCNIVersions:       supportedCNIVersionSet,
		serverVersion:              cni.AntreaCNIVersion,
		nodeConfig:                 nodeConfig,
		hostProcPathPrefix:         hostProcPathPrefix,
		kubeClient:                 kubeClient,
		containerAccess:            newContainerAccessArbitrator(),
		routeClient:                routeClient,
		isChaining:                 isChaining,
		enableBridgingMode:         enableBridgingMode,
		disableTXChecksumOffload:   disableTXChecksumOffload,
		enableSecondaryNetworkIPAM: enableSecondaryNetworkIPAM,
		networkConfig:              networkConfig,
		networkReadyCh:             networkReadyCh,
	}
}

func (s *CNIServer) Initialize(
	ovsBridgeClient ovsconfig.OVSBridgeClient,
	ofClient openflow.Client,
	ifaceStore interfacestore.InterfaceStore,
	podUpdateNotifier channel.Notifier,
	podInfoStore cnipodcache.CNIPodInfoStore,
) error {
	var err error
	// If podInfoStore is not nil, secondaryNetwork configuration is supported.
	if podInfoStore != nil {
		s.secondaryNetworkEnabled = true
	} else {
		s.secondaryNetworkEnabled = false
	}

	s.podConfigurator, err = newPodConfigurator(
		ovsBridgeClient, ofClient, s.routeClient, ifaceStore, s.nodeConfig.GatewayConfig.MAC,
		ovsBridgeClient.GetOVSDatapathType(), ovsBridgeClient.IsHardwareOffloadEnabled(), podUpdateNotifier,
		podInfoStore, s.disableTXChecksumOffload,
	)
	if err != nil {
		return fmt.Errorf("error during initialize podConfigurator: %v", err)
	}
	if err := s.reconcile(); err != nil {
		return fmt.Errorf("error during initial reconciliation for CNI server: %v", err)
	}
	return nil
}

func (s *CNIServer) Run(stopCh <-chan struct{}) {
	klog.Info("Starting CNI server")
	defer klog.Info("Shutting down CNI server")

	listener, err := util.ListenLocalSocket(s.cniSocket)
	if err != nil {
		klog.Fatalf("Failed to bind on %s: %v", s.cniSocket, err)
	}
	rpcServer := grpc.NewServer()

	cnipb.RegisterCniServer(rpcServer, s)
	klog.Info("CNI server is listening ...")
	go func() {
		if err := rpcServer.Serve(listener); err != nil {
			klog.Errorf("Failed to serve connections: %v", err)
		}
	}()
	<-stopCh
}

// interceptAdd handles Add request in policy only mode. Another CNI must already
// be called prior to Antrea CNI to allocate IP and ports. Antrea takes allocated port
// and hooks it to OVS br-int.
func (s *CNIServer) interceptAdd(cniConfig *CNIConfig) (*cnipb.CniCmdResponse, error) {
	klog.Infof("CNI Chaining: add for container %s", cniConfig.ContainerId)
	prevResult, response := s.parsePrevResultFromRequest(cniConfig.NetworkConfig)
	if response != nil {
		klog.Infof("Failed to parse prev result for container %s", cniConfig.ContainerId)
		return response, nil
	}
	podName := string(cniConfig.K8S_POD_NAME)
	podNamespace := string(cniConfig.K8S_POD_NAMESPACE)
	if err := s.podConfigurator.connectInterceptedInterface(
		podName,
		podNamespace,
		cniConfig.ContainerId,
		s.hostNetNsPath(cniConfig.Netns),
		cniConfig.Ifname,
		prevResult.IPs,
		s.containerAccess); err != nil {
		return &cnipb.CniCmdResponse{CniResult: []byte("")}, fmt.Errorf("failed to connect container %s to ovs: %w", cniConfig.ContainerId, err)
	}

	// Packets for multi-cluster traffic will always be encapsulated and sent through
	// tunnels. So here we need to reduce interface MTU for different tunnel types.
	if s.networkConfig.MTUDeduction != 0 {
		if err := s.podConfigurator.ifConfigurator.changeContainerMTU(
			s.hostNetNsPath(cniConfig.Netns),
			cniConfig.Ifname,
			s.networkConfig.MTUDeduction,
		); err != nil {
			return &cnipb.CniCmdResponse{CniResult: []byte("")}, fmt.Errorf("failed to change container %s's MTU: %w", cniConfig.ContainerId, err)
		}
	}

	// we return prevResult, which should be exactly what we received from
	// the runtime, potentially converted to the current CNI version used by
	// Antrea.
	return resultToResponse(prevResult), nil
}

func (s *CNIServer) interceptDel(cniConfig *CNIConfig) (*cnipb.CniCmdResponse, error) {
	klog.Infof("CNI Chaining: delete for container %s", cniConfig.ContainerId)
	return &cnipb.CniCmdResponse{CniResult: []byte("")}, s.podConfigurator.disconnectInterceptedInterface(
		string(cniConfig.K8S_POD_NAME),
		string(cniConfig.K8S_POD_NAMESPACE),
		cniConfig.ContainerId)
}

func (s *CNIServer) interceptCheck(cniConfig *CNIConfig) (*cnipb.CniCmdResponse, error) {
	klog.Infof("CNI Chaining: check for container %s", cniConfig.ContainerId)
	// TODO, check for host interface setup later
	return &cnipb.CniCmdResponse{CniResult: []byte("")}, nil
}

// reconcile performs startup reconciliation for the CNI server. The CNI server is in charge of
// installing Pod flows, so as part of this reconciliation process we retrieve the Pod list from the
// K8s apiserver and replay the necessary flows.
func (s *CNIServer) reconcile() error {
	klog.Infof("Reconciliation for CNI server")
	// For performance reasons, use ResourceVersion="0" in the ListOptions to ensure the request is served from
	// the watch cache in kube-apiserver.
	pods, err := s.kubeClient.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector:   "spec.nodeName=" + s.nodeConfig.Name,
		ResourceVersion: "0",
	})
	if err != nil {
		return fmt.Errorf("failed to list Pods running on Node %s: %v", s.nodeConfig.Name, err)
	}

	return s.podConfigurator.reconcile(pods.Items, s.containerAccess)
}

func init() {
	supportedCNIVersionSet = buildVersionSet()
}
