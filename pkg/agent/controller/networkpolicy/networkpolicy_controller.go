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

package networkpolicy

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"antrea.io/ofnet/ofctrl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent"
	"antrea.io/antrea/pkg/agent/config"
	"antrea.io/antrea/pkg/agent/controller/networkpolicy/l7engine"
	"antrea.io/antrea/pkg/agent/flowexporter/connections"
	"antrea.io/antrea/pkg/agent/interfacestore"
	"antrea.io/antrea/pkg/agent/openflow"
	proxytypes "antrea.io/antrea/pkg/agent/proxy/types"
	"antrea.io/antrea/pkg/agent/types"
	"antrea.io/antrea/pkg/apis/controlplane/v1beta2"
	"antrea.io/antrea/pkg/querier"
	"antrea.io/antrea/pkg/util/channel"
)

const (
	// How long to wait before retrying the processing of a network policy change.
	minRetryDelay = 5 * time.Second
	maxRetryDelay = 300 * time.Second
	// Default number of workers processing a rule change.
	defaultWorkers = 4
	// Default number of workers for making DNS queries.
	defaultDNSWorkers = 4
	// Reserved OVS rule ID for installing the DNS response intercept rule.
	// It is a special OVS rule which intercepts DNS query responses from DNS
	// services to the workloads that have FQDN policy rules applied.
	dnsInterceptRuleID = uint32(1)
)

type L7RuleReconciler interface {
	AddRule(ruleID, policyName string, vlanID uint32, l7Protocols []v1beta2.L7Protocol, enableLogging bool) error
	DeleteRule(ruleID string, vlanID uint32) error
}

var emptyWatch = watch.NewEmptyWatch()

type packetInAction func(*ofctrl.PacketIn) error

// Controller is responsible for watching Antrea AddressGroups, AppliedToGroups,
// and NetworkPolicies, feeding them to ruleCache, getting dirty rules from
// ruleCache, invoking reconciler to reconcile them.
//
//	        a.Feed AddressGroups,AppliedToGroups
//	             and NetworkPolicies
//	|-----------|    <--------    |----------- |  c. Reconcile dirty rules |----------- |
//	| ruleCache |                 | Controller |     ------------>         | reconciler |
//	| ----------|    -------->    |----------- |                           |----------- |
//	            b. Notify dirty rules
type Controller struct {
	// antreaPolicyEnabled indicates whether Antrea NetworkPolicy and
	// ClusterNetworkPolicy are enabled.
	antreaPolicyEnabled    bool
	l7NetworkPolicyEnabled bool
	// antreaProxyEnabled indicates whether Antrea proxy is enabled.
	antreaProxyEnabled bool
	// statusManagerEnabled indicates whether a statusManager is configured.
	statusManagerEnabled bool
	// multicastEnabled indicates whether multicast is enabled.
	multicastEnabled bool
	// loggingEnabled indicates where Antrea policy audit logging is enabled.
	loggingEnabled bool
	// nodeType indicates type of the Node where Antrea Agent is running on.
	nodeType config.NodeType
	// antreaClientProvider provides interfaces to get antreaClient, which can be
	// used to watch Antrea AddressGroups, AppliedToGroups, and NetworkPolicies.
	// We need to get antreaClient dynamically because the apiserver cert can be
	// rotated and we need a new client with the updated CA cert.
	// Verifying server certificate only takes place for new requests and existing
	// watches won't be interrupted by rotating cert. The new client will be used
	// after the existing watches expire.
	antreaClientProvider agent.AntreaClientProvider
	// queue maintains the NetworkPolicy ruleIDs that need to be synced.
	queue workqueue.RateLimitingInterface
	// ruleCache maintains the desired state of NetworkPolicy rules.
	ruleCache *ruleCache
	// reconciler provides interfaces to reconcile the desired state of
	// NetworkPolicy rules with the actual state of Openflow entries.
	reconciler Reconciler
	// l7RuleReconciler provides interfaces to reconcile the desired state of
	// NetworkPolicy rules which have L7 rules with the actual state of Suricata rules.
	l7RuleReconciler L7RuleReconciler
	// l7VlanIDAllocator allocates a VLAN ID for every L7 rule.
	l7VlanIDAllocator *l7VlanIDAllocator
	// ofClient registers packetin for Antrea Policy logging.
	ofClient           openflow.Client
	antreaPolicyLogger *AntreaPolicyLogger
	// statusManager syncs NetworkPolicy statuses with the antrea-controller.
	// It's only for Antrea NetworkPolicies.
	statusManager         StatusManager
	fqdnController        *fqdnController
	networkPolicyWatcher  *watcher
	appliedToGroupWatcher *watcher
	addressGroupWatcher   *watcher
	fullSyncGroup         sync.WaitGroup
	ifaceStore            interfacestore.InterfaceStore
	// denyConnStore is for storing deny connections for flow exporter.
	denyConnStore *connections.DenyConnectionStore
	gwPort        uint32
	tunPort       uint32
	nodeConfig    *config.NodeConfig

	logPacketAction           packetInAction
	rejectRequestAction       packetInAction
	storeDenyConnectionAction packetInAction
}

// NewNetworkPolicyController returns a new *Controller.
func NewNetworkPolicyController(antreaClientGetter agent.AntreaClientProvider,
	ofClient openflow.Client,
	ifaceStore interfacestore.InterfaceStore,
	nodeName string,
	podUpdateSubscriber channel.Subscriber,
	externalEntityUpdateSubscriber channel.Subscriber,
	groupCounters []proxytypes.GroupCounter,
	groupIDUpdates <-chan string,
	antreaPolicyEnabled bool,
	l7NetworkPolicyEnabled bool,
	antreaProxyEnabled bool,
	statusManagerEnabled bool,
	multicastEnabled bool,
	loggingEnabled bool,
	asyncRuleDeleteInterval time.Duration,
	dnsServerOverride string,
	nodeType config.NodeType,
	v4Enabled bool,
	v6Enabled bool,
	gwPort, tunPort uint32,
	nodeConfig *config.NodeConfig) (*Controller, error) {
	idAllocator := newIDAllocator(asyncRuleDeleteInterval, dnsInterceptRuleID)
	c := &Controller{
		antreaClientProvider:   antreaClientGetter,
		queue:                  workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "networkpolicyrule"),
		ofClient:               ofClient,
		nodeType:               nodeType,
		antreaPolicyEnabled:    antreaPolicyEnabled,
		l7NetworkPolicyEnabled: l7NetworkPolicyEnabled,
		antreaProxyEnabled:     antreaProxyEnabled,
		statusManagerEnabled:   statusManagerEnabled,
		multicastEnabled:       multicastEnabled,
		loggingEnabled:         loggingEnabled,
		gwPort:                 gwPort,
		tunPort:                tunPort,
		nodeConfig:             nodeConfig,
	}

	if l7NetworkPolicyEnabled {
		c.l7RuleReconciler = l7engine.NewReconciler()
		c.l7VlanIDAllocator = newL7VlanIDAllocator()
	}

	if antreaPolicyEnabled {
		var err error
		if c.fqdnController, err = newFQDNController(ofClient, idAllocator, dnsServerOverride, c.enqueueRule, v4Enabled, v6Enabled, gwPort); err != nil {
			return nil, err
		}

		if c.ofClient != nil {
			c.ofClient.RegisterPacketInHandler(uint8(openflow.PacketInCategoryDNS), c.fqdnController)
		}
	}
	c.reconciler = newReconciler(ofClient, ifaceStore, idAllocator, c.fqdnController, groupCounters,
		v4Enabled, v6Enabled, antreaPolicyEnabled, multicastEnabled)
	c.ruleCache = newRuleCache(c.enqueueRule, podUpdateSubscriber, externalEntityUpdateSubscriber, groupIDUpdates, nodeType)
	if statusManagerEnabled {
		c.statusManager = newStatusController(antreaClientGetter, nodeName, c.ruleCache)
	}
	// Create a WaitGroup that is used to block network policy workers from asynchronously processing
	// NP rules until the events preceding bookmark are synced. It can also be used as part of the
	// solution to a deterministic mechanism for when to cleanup flows from previous round.
	// Wait until appliedToGroupWatcher, addressGroupWatcher and networkPolicyWatcher to receive bookmark event.
	c.fullSyncGroup.Add(3)

	if c.ofClient != nil && antreaPolicyEnabled {
		// Register packetInHandler
		c.ofClient.RegisterPacketInHandler(uint8(openflow.PacketInCategoryNP), c)
		if loggingEnabled {
			// Initiate logger for Antrea Policy audit logging
			antreaPolicyLogger, err := newAntreaPolicyLogger()
			if err != nil {
				return nil, err
			}
			c.antreaPolicyLogger = antreaPolicyLogger
		}
	}

	// Use nodeName to filter resources when watching resources.
	options := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("nodeName", nodeName).String(),
	}

	c.networkPolicyWatcher = &watcher{
		objectType: "NetworkPolicy",
		watchFunc: func() (watch.Interface, error) {
			antreaClient, err := c.antreaClientProvider.GetAntreaClient()
			if err != nil {
				return nil, err
			}
			return antreaClient.ControlplaneV1beta2().NetworkPolicies().Watch(context.TODO(), options)
		},
		AddFunc: func(obj runtime.Object) error {
			policy, ok := obj.(*v1beta2.NetworkPolicy)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.NetworkPolicy: %v", obj)
			}
			if !c.antreaPolicyEnabled && policy.SourceRef.Type != v1beta2.K8sNetworkPolicy {
				klog.Infof("Ignore Antrea NetworkPolicy %s since AntreaPolicy feature gate is not enabled",
					policy.SourceRef.ToString())
				return nil
			}
			c.ruleCache.AddNetworkPolicy(policy)
			klog.Infof("NetworkPolicy %s applied to Pods on this Node", policy.SourceRef.ToString())
			return nil
		},
		UpdateFunc: func(obj runtime.Object) error {
			policy, ok := obj.(*v1beta2.NetworkPolicy)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.NetworkPolicy: %v", obj)
			}
			if !c.antreaPolicyEnabled && policy.SourceRef.Type != v1beta2.K8sNetworkPolicy {
				klog.Infof("Ignore Antrea NetworkPolicy %s since AntreaPolicy feature gate is not enabled",
					policy.SourceRef.ToString())
				return nil
			}
			updated := c.ruleCache.UpdateNetworkPolicy(policy)
			// If any rule or the generation changes, we ensure statusManager will resync the policy's status once, in
			// case the changes don't cause any actual rule update but the whole policy's generation is changed.
			if c.statusManagerEnabled && updated && policy.SourceRef.Type != v1beta2.K8sNetworkPolicy {
				c.statusManager.Resync(policy.UID)
			}
			return nil
		},
		DeleteFunc: func(obj runtime.Object) error {
			policy, ok := obj.(*v1beta2.NetworkPolicy)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.NetworkPolicy: %v", obj)
			}
			if !c.antreaPolicyEnabled && policy.SourceRef.Type != v1beta2.K8sNetworkPolicy {
				klog.Infof("Ignore Antrea NetworkPolicy %s since AntreaPolicy feature gate is not enabled",
					policy.SourceRef.ToString())
				return nil
			}
			c.ruleCache.DeleteNetworkPolicy(policy)
			klog.Infof("NetworkPolicy %s no longer applied to Pods on this Node", policy.SourceRef.ToString())
			return nil
		},
		ReplaceFunc: func(objs []runtime.Object) error {
			policies := make([]*v1beta2.NetworkPolicy, len(objs))
			var ok bool
			for i := range objs {
				policies[i], ok = objs[i].(*v1beta2.NetworkPolicy)
				if !ok {
					return fmt.Errorf("cannot convert to *v1beta1.NetworkPolicy: %v", objs[i])
				}
				if !c.antreaPolicyEnabled && policies[i].SourceRef.Type != v1beta2.K8sNetworkPolicy {
					klog.Infof("Ignore Antrea NetworkPolicy %s since AntreaPolicy feature gate is not enabled",
						policies[i].SourceRef.ToString())
					return nil
				}
				klog.Infof("NetworkPolicy %s applied to Pods on this Node", policies[i].SourceRef.ToString())
				// When ReplaceFunc is called, either the controller restarted or this was a regular reconnection.
				// For the former case, agent must resync the statuses as the controller lost the previous statuses.
				// For the latter case, agent doesn't need to do anything. However, we are not able to differentiate the
				// two cases. Anyway there's no harm to do a periodical resync.
				if c.statusManagerEnabled && policies[i].SourceRef.Type != v1beta2.K8sNetworkPolicy {
					c.statusManager.Resync(policies[i].UID)
				}
			}
			c.ruleCache.ReplaceNetworkPolicies(policies)
			return nil
		},
		fullSyncWaitGroup: &c.fullSyncGroup,
		fullSynced:        false,
	}

	c.appliedToGroupWatcher = &watcher{
		objectType: "AppliedToGroup",
		watchFunc: func() (watch.Interface, error) {
			antreaClient, err := c.antreaClientProvider.GetAntreaClient()
			if err != nil {
				return nil, err
			}
			return antreaClient.ControlplaneV1beta2().AppliedToGroups().Watch(context.TODO(), options)
		},
		AddFunc: func(obj runtime.Object) error {
			group, ok := obj.(*v1beta2.AppliedToGroup)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.AppliedToGroup: %v", obj)
			}
			c.ruleCache.AddAppliedToGroup(group)
			return nil
		},
		UpdateFunc: func(obj runtime.Object) error {
			group, ok := obj.(*v1beta2.AppliedToGroupPatch)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.AppliedToGroup: %v", obj)
			}
			c.ruleCache.PatchAppliedToGroup(group)
			return nil
		},
		DeleteFunc: func(obj runtime.Object) error {
			group, ok := obj.(*v1beta2.AppliedToGroup)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.AppliedToGroup: %v", obj)
			}
			c.ruleCache.DeleteAppliedToGroup(group)
			return nil
		},
		ReplaceFunc: func(objs []runtime.Object) error {
			groups := make([]*v1beta2.AppliedToGroup, len(objs))
			var ok bool
			for i := range objs {
				groups[i], ok = objs[i].(*v1beta2.AppliedToGroup)
				if !ok {
					return fmt.Errorf("cannot convert to *v1beta1.AppliedToGroup: %v", objs[i])
				}
			}
			c.ruleCache.ReplaceAppliedToGroups(groups)
			return nil
		},
		fullSyncWaitGroup: &c.fullSyncGroup,
		fullSynced:        false,
	}

	c.addressGroupWatcher = &watcher{
		objectType: "AddressGroup",
		watchFunc: func() (watch.Interface, error) {
			antreaClient, err := c.antreaClientProvider.GetAntreaClient()
			if err != nil {
				return nil, err
			}
			return antreaClient.ControlplaneV1beta2().AddressGroups().Watch(context.TODO(), options)
		},
		AddFunc: func(obj runtime.Object) error {
			group, ok := obj.(*v1beta2.AddressGroup)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.AddressGroup: %v", obj)
			}
			c.ruleCache.AddAddressGroup(group)
			return nil
		},
		UpdateFunc: func(obj runtime.Object) error {
			group, ok := obj.(*v1beta2.AddressGroupPatch)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.AddressGroup: %v", obj)
			}
			c.ruleCache.PatchAddressGroup(group)
			return nil
		},
		DeleteFunc: func(obj runtime.Object) error {
			group, ok := obj.(*v1beta2.AddressGroup)
			if !ok {
				return fmt.Errorf("cannot convert to *v1beta1.AddressGroup: %v", obj)
			}
			c.ruleCache.DeleteAddressGroup(group)
			return nil
		},
		ReplaceFunc: func(objs []runtime.Object) error {
			groups := make([]*v1beta2.AddressGroup, len(objs))
			var ok bool
			for i := range objs {
				groups[i], ok = objs[i].(*v1beta2.AddressGroup)
				if !ok {
					return fmt.Errorf("cannot convert to *v1beta1.AddressGroup: %v", objs[i])
				}
			}
			c.ruleCache.ReplaceAddressGroups(groups)
			return nil
		},
		fullSyncWaitGroup: &c.fullSyncGroup,
		fullSynced:        false,
	}
	c.ifaceStore = ifaceStore
	c.logPacketAction = c.logPacket
	c.rejectRequestAction = c.rejectRequest
	c.storeDenyConnectionAction = c.storeDenyConnection
	return c, nil
}

func (c *Controller) GetNetworkPolicyNum() int {
	return c.ruleCache.GetNetworkPolicyNum()
}

func (c *Controller) GetAddressGroupNum() int {
	return c.ruleCache.GetAddressGroupNum()
}

func (c *Controller) GetAppliedToGroupNum() int {
	return c.ruleCache.GetAppliedToGroupNum()
}

// GetNetworkPolicies returns the requested NetworkPolicies.
// This func will return all NetworkPolicies that can match all provided attributes in NetworkPolicyQueryFilter.
// These not provided attributes in NetworkPolicyQueryFilter means match all.
func (c *Controller) GetNetworkPolicies(npFilter *querier.NetworkPolicyQueryFilter) []v1beta2.NetworkPolicy {
	return c.ruleCache.getNetworkPolicies(npFilter)
}

// GetAppliedNetworkPolicies returns the NetworkPolicies applied to the Pod and match the filter.
func (c *Controller) GetAppliedNetworkPolicies(pod, namespace string, npFilter *querier.NetworkPolicyQueryFilter) []v1beta2.NetworkPolicy {
	return c.ruleCache.getAppliedNetworkPolicies(pod, namespace, npFilter)
}

func (c *Controller) GetAddressGroups() []v1beta2.AddressGroup {
	return c.ruleCache.GetAddressGroups()
}

func (c *Controller) GetAppliedToGroups() []v1beta2.AppliedToGroup {
	return c.ruleCache.GetAppliedToGroups()
}

func (c *Controller) GetNetworkPolicyByRuleFlowID(ruleFlowID uint32) *v1beta2.NetworkPolicyReference {
	rule := c.GetRuleByFlowID(ruleFlowID)
	if rule == nil {
		return nil
	}
	return rule.PolicyRef
}

func (c *Controller) GetRuleByFlowID(ruleFlowID uint32) *types.PolicyRule {
	rule, exists, err := c.reconciler.GetRuleByFlowID(ruleFlowID)
	if err != nil {
		klog.Errorf("Error when getting network policy by rule flow ID: %v", err)
		return nil
	}
	if !exists {
		return nil
	}
	return rule
}

func (c *Controller) GetControllerConnectionStatus() bool {
	// When the watchers are connected, controller connection status is true. Otherwise, it is false.
	return c.addressGroupWatcher.isConnected() && c.appliedToGroupWatcher.isConnected() && c.networkPolicyWatcher.isConnected()
}

func (c *Controller) SetDenyConnStore(denyConnStore *connections.DenyConnectionStore) {
	c.denyConnStore = denyConnStore
}

// Run begins watching and processing Antrea AddressGroups, AppliedToGroups
// and NetworkPolicies, and spawns workers that reconciles NetworkPolicy rules.
// Run will not return until stopCh is closed.
func (c *Controller) Run(stopCh <-chan struct{}) {
	attempts := 0
	if err := wait.PollImmediateUntil(200*time.Millisecond, func() (bool, error) {
		if attempts%10 == 0 {
			klog.Info("Waiting for Antrea client to be ready")
		}
		if _, err := c.antreaClientProvider.GetAntreaClient(); err != nil {
			attempts++
			return false, nil
		}
		return true, nil
	}, stopCh); err != nil {
		klog.Info("Stopped waiting for Antrea client")
		return
	}
	klog.Info("Antrea client is ready")

	// Use NonSlidingUntil so that normal reconnection (disconnected after
	// running a while) can reconnect immediately while abnormal reconnection
	// won't be too aggressive.
	go wait.NonSlidingUntil(c.appliedToGroupWatcher.watch, 5*time.Second, stopCh)
	go wait.NonSlidingUntil(c.addressGroupWatcher.watch, 5*time.Second, stopCh)
	go wait.NonSlidingUntil(c.networkPolicyWatcher.watch, 5*time.Second, stopCh)

	if c.antreaPolicyEnabled {
		for i := 0; i < defaultDNSWorkers; i++ {
			go wait.Until(c.fqdnController.worker, time.Second, stopCh)
		}
		go c.fqdnController.runRuleSyncTracker(stopCh)
	}
	klog.Infof("Waiting for all watchers to complete full sync")
	c.fullSyncGroup.Wait()
	klog.Infof("All watchers have completed full sync, installing flows for init events")
	// Batch install all rules in queue after fullSync is finished.
	c.processAllItemsInQueue()

	klog.Infof("Starting NetworkPolicy workers now")
	defer c.queue.ShutDown()
	for i := 0; i < defaultWorkers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}

	klog.Infof("Starting IDAllocator worker to maintain the async rule cache")
	go c.reconciler.RunIDAllocatorWorker(stopCh)

	if c.statusManagerEnabled {
		go c.statusManager.Run(stopCh)
	}

	<-stopCh
}

func (c *Controller) matchIGMPType(r *rule, igmpType uint8, groupAddress string) bool {
	for _, s := range r.Services {
		if (s.IGMPType == nil || uint8(*s.IGMPType) == igmpType) && (s.GroupAddress == "" || s.GroupAddress == groupAddress) {
			return true
		}
	}
	return false
}

// GetIGMPNPRuleInfo looks up the IGMP NetworkPolicy rule that matches the given Pod and groupAddress,
// and returns the rule information if found.
func (c *Controller) GetIGMPNPRuleInfo(podName, podNamespace string, groupAddress net.IP, igmpType uint8) (*types.IGMPNPRuleInfo, error) {
	member := &v1beta2.GroupMember{
		Pod: &v1beta2.PodReference{
			Name:      podName,
			Namespace: podNamespace,
		},
	}

	var ruleInfo *types.IGMPNPRuleInfo
	objects, _ := c.ruleCache.rules.ByIndex(toIGMPReportGroupAddressIndex, groupAddress.String())
	objects2, _ := c.ruleCache.rules.ByIndex(toIGMPReportGroupAddressIndex, "")
	objects = append(objects, objects2...)
	var matchedRule *rule
	for _, obj := range objects {
		rule := obj.(*rule)
		groupMembers, anyExists := c.ruleCache.unionAppliedToGroups(rule.AppliedToGroups)
		if !anyExists {
			continue
		}
		if groupMembers.Has(member) && (matchedRule == nil || matchedRule.Less(rule)) &&
			c.matchIGMPType(rule, igmpType, groupAddress.String()) {
			matchedRule = rule
		}
	}

	if matchedRule != nil {
		ruleInfo = &types.IGMPNPRuleInfo{
			RuleAction: *matchedRule.Action,
			UUID:       matchedRule.PolicyUID,
			NPType:     &matchedRule.SourceRef.Type,
			Name:       matchedRule.Name,
		}
	}
	return ruleInfo, nil
}

func (c *Controller) enqueueRule(ruleID string) {
	c.queue.Add(ruleID)
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same rule at
// the same time.
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncRule(key.(string))
	c.handleErr(err, key)

	return true
}

// processAllItemsInQueue pops all rule keys queued at the moment and calls syncRules to
// reconcile those rules in batch.
func (c *Controller) processAllItemsInQueue() {
	numRules := c.queue.Len()
	batchSyncRuleKeys := make([]string, numRules)
	for i := 0; i < numRules; i++ {
		ruleKey, _ := c.queue.Get()
		batchSyncRuleKeys[i] = ruleKey.(string)
		// set key to done to prevent missing watched updates between here and fullSync finish.
		c.queue.Done(ruleKey)
	}
	// Reconcile all rule keys at once.
	if err := c.syncRules(batchSyncRuleKeys); err != nil {
		klog.Errorf("Error occurred when reconciling all rules for init events: %v", err)
		for _, k := range batchSyncRuleKeys {
			c.queue.AddRateLimited(k)
		}
	}
}

func (c *Controller) syncRule(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).InfoS("Finished syncing rule", "ruleID", key, "duration", time.Since(startTime))
	}()
	rule, effective, realizable := c.ruleCache.GetCompletedRule(key)
	if !effective {
		klog.V(2).InfoS("Rule was not effective, removing it", "ruleID", key)
		if err := c.reconciler.Forget(key); err != nil {
			return err
		}
		if c.statusManagerEnabled {
			// We don't know whether this is a rule owned by Antrea Policy, but
			// harmless to delete it.
			c.statusManager.DeleteRuleRealization(key)
		}
		if c.l7NetworkPolicyEnabled {
			if vlanID := c.l7VlanIDAllocator.query(key); vlanID != 0 {
				if err := c.l7RuleReconciler.DeleteRule(key, vlanID); err != nil {
					return err
				}
				c.l7VlanIDAllocator.release(key)
			}
		}
		return nil
	}
	// If the rule is not realizable, we can simply skip it as it will be marked as dirty
	// and queued again when we receive the missing group it missed.
	if !realizable {
		klog.V(2).InfoS("Rule is not realizable, skipping", "ruleID", key)
		return nil
	}

	if c.l7NetworkPolicyEnabled && len(rule.L7Protocols) != 0 {
		// Allocate VLAN ID for the L7 rule.
		vlanID := c.l7VlanIDAllocator.allocate(key)
		rule.L7RuleVlanID = &vlanID

		if err := c.l7RuleReconciler.AddRule(key, rule.SourceRef.ToString(), vlanID, rule.L7Protocols, rule.EnableLogging); err != nil {
			return err
		}
	}

	err := c.reconciler.Reconcile(rule)
	if c.fqdnController != nil {
		// No matter whether the rule reconciliation succeeds or not, fqdnController
		// needs to be notified of the status.
		klog.V(2).InfoS("Rule realization was done", "ruleID", key)
		c.fqdnController.notifyRuleUpdate(key, err)
	}
	if err != nil {
		return err
	}
	if c.statusManagerEnabled && rule.SourceRef.Type != v1beta2.K8sNetworkPolicy {
		c.statusManager.SetRuleRealization(key, rule.PolicyUID)
	}
	return nil
}

// syncRules calls the reconciler to sync all the rules after watchers complete full sync.
// After flows for those init events are installed, subsequent rules will be handled asynchronously
// by the syncRule() function.
func (c *Controller) syncRules(keys []string) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing all rules before bookmark event (%v)", time.Since(startTime))
	}()

	var allRules []*CompletedRule
	for _, key := range keys {
		rule, effective, realizable := c.ruleCache.GetCompletedRule(key)
		// It's normal that a rule is not effective on this Node but abnormal that it is not realizable after watchers
		// complete full sync.
		if !effective {
			klog.Infof("Rule %s is not effective on this Node", key)
		} else if !realizable {
			klog.Errorf("Rule %s is effective but not realizable", key)
		} else {
			if c.l7NetworkPolicyEnabled && len(rule.L7Protocols) != 0 {
				// Allocate VLAN ID for the L7 rule.
				vlanID := c.l7VlanIDAllocator.allocate(key)
				rule.L7RuleVlanID = &vlanID

				if err := c.l7RuleReconciler.AddRule(key, rule.SourceRef.ToString(), vlanID, rule.L7Protocols, rule.EnableLogging); err != nil {
					return err
				}
			}
			allRules = append(allRules, rule)
		}
	}
	if err := c.reconciler.BatchReconcile(allRules); err != nil {
		return err
	}
	if c.statusManagerEnabled {
		for _, rule := range allRules {
			if rule.SourceRef.Type != v1beta2.K8sNetworkPolicy {
				c.statusManager.SetRuleRealization(rule.ID, rule.PolicyUID)
			}
		}
	}
	return nil
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	klog.Errorf("Error syncing rule %q, retrying. Error: %v", key, err)
	c.queue.AddRateLimited(key)
}

// watcher is responsible for watching a given resource with the provided watchFunc
// and calling the eventHandlers when receiving events.
type watcher struct {
	// objectType is the type of objects being watched, used for logging.
	objectType string
	// watchFunc is the function that starts the watch.
	watchFunc func() (watch.Interface, error)
	// AddFunc is the function that handles added event.
	AddFunc func(obj runtime.Object) error
	// UpdateFunc is the function that handles modified event.
	UpdateFunc func(obj runtime.Object) error
	// DeleteFunc is the function that handles deleted event.
	DeleteFunc func(obj runtime.Object) error
	// ReplaceFunc is the function that handles init events.
	ReplaceFunc func(objs []runtime.Object) error
	// connected represents whether the watch has connected to apiserver successfully.
	connected bool
	// lock protects connected.
	lock sync.RWMutex
	// group to be notified when each watcher receives bookmark event
	fullSyncWaitGroup *sync.WaitGroup
	// fullSynced indicates if the resource has been synced at least once since agent started.
	fullSynced bool
}

func (w *watcher) isConnected() bool {
	w.lock.RLock()
	defer w.lock.RUnlock()
	return w.connected
}

func (w *watcher) setConnected(connected bool) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.connected = connected
}

func (w *watcher) watch() {
	klog.Infof("Starting watch for %s", w.objectType)
	watcher, err := w.watchFunc()
	if err != nil {
		klog.Warningf("Failed to start watch for %s: %v", w.objectType, err)
		return
	}
	// Watch method doesn't return error but "emptyWatch" in case of some partial data errors,
	// e.g. timeout error. Make sure that watcher is not empty and log warning otherwise.
	if reflect.TypeOf(watcher) == reflect.TypeOf(emptyWatch) {
		klog.Warningf("Failed to start watch for %s, please ensure antrea service is reachable for the agent", w.objectType)
		return
	}

	klog.Infof("Started watch for %s", w.objectType)
	w.setConnected(true)
	eventCount := 0
	defer func() {
		klog.Infof("Stopped watch for %s, total items received: %d", w.objectType, eventCount)
		w.setConnected(false)
		watcher.Stop()
	}()

	// First receive init events from the result channel and buffer them until
	// a Bookmark event is received, indicating that all init events have been
	// received.
	var initObjects []runtime.Object
loop:
	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				klog.Warningf("Result channel for %s was closed", w.objectType)
				return
			}
			switch event.Type {
			case watch.Added:
				klog.V(2).Infof("Added %s (%#v)", w.objectType, event.Object)
				initObjects = append(initObjects, event.Object)
			case watch.Bookmark:
				break loop
			}
		}
	}
	klog.Infof("Received %d init events for %s", len(initObjects), w.objectType)

	eventCount += len(initObjects)
	if err := w.ReplaceFunc(initObjects); err != nil {
		klog.Errorf("Failed to handle init events: %v", err)
		return
	}
	if !w.fullSynced {
		w.fullSynced = true
		// Notify fullSyncWaitGroup that all events before bookmark is handled
		w.fullSyncWaitGroup.Done()
	}

	for {
		select {
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			klog.V(2).InfoS("Received event", "eventType", event.Type, "objectType", w.objectType, "object", event.Object)
			switch event.Type {
			case watch.Added:
				if err := w.AddFunc(event.Object); err != nil {
					klog.Errorf("Failed to handle added event: %v", err)
					return
				}
			case watch.Modified:
				if err := w.UpdateFunc(event.Object); err != nil {
					klog.Errorf("Failed to handle modified event: %v", err)
					return
				}
			case watch.Deleted:
				if err := w.DeleteFunc(event.Object); err != nil {
					klog.Errorf("Failed to handle deleted event: %v", err)
					return
				}
			default:
				klog.Errorf("Unknown event: %v", event)
				return
			}
			eventCount++
		}
	}
}
