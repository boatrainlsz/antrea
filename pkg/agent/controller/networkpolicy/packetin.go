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

package networkpolicy

import (
	"errors"
	"fmt"
	"net"
	"time"

	"antrea.io/libOpenflow/openflow15"
	"antrea.io/ofnet/ofctrl"
	"github.com/vmware/go-ipfix/pkg/registry"
	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent/flowexporter"
	"antrea.io/antrea/pkg/agent/openflow"
	binding "antrea.io/antrea/pkg/ovs/openflow"
)

// HandlePacketIn is the packetIn handler registered to openflow by Antrea network
// policy agent controller. It performs the appropriate operations based on which
// bits are set in the "custom reasons" field of the packet received from OVS.
func (c *Controller) HandlePacketIn(pktIn *ofctrl.PacketIn) error {
	if pktIn == nil {
		return errors.New("empty packetIn for Antrea Policy")
	}

	if len(pktIn.UserData) < 2 {
		return errors.New("packetIn for Antrea Policy miss the required userdata")
	}
	packetInOperations := pktIn.UserData[1]
	// Choose operations.
	var checkOperation = func(operation uint8) bool {
		return packetInOperations&operation == operation
	}
	if checkOperation(openflow.PacketInNPLoggingOperation) {
		if err := c.logPacketAction(pktIn); err != nil {
			return err
		}
	}
	if checkOperation(openflow.PacketInNPRejectOperation) {
		if err := c.rejectRequestAction(pktIn); err != nil {
			return err
		}
	}
	if checkOperation(openflow.PacketInNPStoreDenyOperation) {
		if err := c.storeDenyConnectionAction(pktIn); err != nil {
			return err
		}
	}
	return nil
}

// getMatchRegField returns match to the regNum register.
func getMatchRegField(matchers *ofctrl.Matchers, field *binding.RegField) *ofctrl.MatchField {
	return openflow.GetMatchFieldByRegID(matchers, field.GetRegID())
}

// getMatch receives ofctrl matchers and table id, match field.
// Modifies match field to Ingress/Egress register based on tableID.
func getMatch(matchers *ofctrl.Matchers, tableID uint8, disposition uint32) *ofctrl.MatchField {
	// Get match from CNPDenyConjIDReg if disposition is Drop or Reject.
	if disposition == openflow.DispositionDrop || disposition == openflow.DispositionRej {
		return getMatchRegField(matchers, openflow.APConjIDField)
	}
	// Get match from ingress/egress reg if disposition is Allow or Pass.
	for _, table := range append(openflow.GetAntreaPolicyEgressTables(), openflow.EgressRuleTable) {
		if tableID == table.GetID() {
			return getMatchRegField(matchers, openflow.TFEgressConjIDField)
		}
	}
	for _, table := range append(openflow.GetAntreaPolicyIngressTables(), openflow.IngressRuleTable) {
		if tableID == table.GetID() {
			return getMatchRegField(matchers, openflow.TFIngressConjIDField)
		}
	}
	return nil
}

// getInfoInReg unloads and returns data stored in the match field.
func getInfoInReg(regMatch *ofctrl.MatchField, rng *openflow15.NXRange) (uint32, error) {
	regValue, ok := regMatch.GetValue().(*ofctrl.NXRegister)
	if !ok {
		return 0, errors.New("register value cannot be retrieved")
	}
	if rng != nil {
		return ofctrl.GetUint32ValueWithRange(regValue.Data, rng), nil
	}
	return regValue.Data, nil
}

func (c *Controller) storeDenyConnection(pktIn *ofctrl.PacketIn) error {
	packet, err := binding.ParsePacketIn(pktIn)
	if err != nil {
		return fmt.Errorf("error in parsing packetIn: %v", err)
	}

	// Get 5-tuple information
	tuple := flowexporter.Tuple{
		SourcePort:      packet.SourcePort,
		DestinationPort: packet.DestinationPort,
		Protocol:        packet.IPProto,
	}
	// Make deep copy of IP addresses
	tuple.SourceAddress = make(net.IP, len(packet.SourceIP))
	tuple.DestinationAddress = make(net.IP, len(packet.DestinationIP))
	copy(tuple.SourceAddress, packet.SourceIP)
	copy(tuple.DestinationAddress, packet.DestinationIP)

	// Generate deny connection and add to deny connection store
	denyConn := flowexporter.Connection{}
	denyConn.FlowKey = tuple
	denyConn.DestinationServiceAddress = tuple.DestinationAddress
	denyConn.DestinationServicePort = tuple.DestinationPort

	// No need to obtain connection info again if it already exists in denyConnectionStore.
	if conn, exist := c.denyConnStore.GetConnByKey(flowexporter.NewConnectionKey(&denyConn)); exist {
		c.denyConnStore.AddOrUpdateConn(conn, time.Now(), uint64(packet.IPLength))
		return nil
	}

	matchers := pktIn.GetMatches()
	var match *ofctrl.MatchField
	// Get table ID
	tableID := pktIn.TableId
	// Get disposition Allow, Drop or Reject
	match = getMatchRegField(matchers, openflow.APDispositionField)
	id, err := getInfoInReg(match, openflow.APDispositionField.GetRange().ToNXRange())
	if err != nil {
		return fmt.Errorf("error when getting disposition from reg: %v", err)
	}
	disposition := openflow.DispositionToString[id]

	// Set match to corresponding ingress/egress reg according to disposition
	match = getMatch(matchers, tableID, id)
	if match != nil {
		ruleID, err := getInfoInReg(match, nil)
		if err != nil {
			return fmt.Errorf("error when obtaining rule id from reg: %v", err)
		}
		policy := c.GetNetworkPolicyByRuleFlowID(ruleID)
		rule := c.GetRuleByFlowID(ruleID)
		if policy == nil || rule == nil {
			klog.V(4).Infof("Cannot find NetworkPolicy or rule that has ruleID %v", ruleID)
			// Ignore the connection if there is no matching NetworkPolicy or rule: the
			// NetworkPolicy must have been deleted or updated.
			return nil
		}
		// Get name and namespace for Antrea Network Policy or Antrea Cluster Network Policy
		if isAntreaPolicyIngressTable(tableID) {
			denyConn.IngressNetworkPolicyName = policy.Name
			denyConn.IngressNetworkPolicyNamespace = policy.Namespace
			denyConn.IngressNetworkPolicyType = flowexporter.PolicyTypeToUint8(policy.Type)
			denyConn.IngressNetworkPolicyRuleName = rule.Name
			denyConn.IngressNetworkPolicyRuleAction = flowexporter.RuleActionToUint8(disposition)
		} else if isAntreaPolicyEgressTable(tableID) {
			denyConn.EgressNetworkPolicyName = policy.Name
			denyConn.EgressNetworkPolicyNamespace = policy.Namespace
			denyConn.EgressNetworkPolicyType = flowexporter.PolicyTypeToUint8(policy.Type)
			denyConn.EgressNetworkPolicyRuleName = rule.Name
			denyConn.EgressNetworkPolicyRuleAction = flowexporter.RuleActionToUint8(disposition)
		}
	} else {
		// For K8s NetworkPolicy implicit drop action, we cannot get Namespace/name.
		if tableID == openflow.IngressDefaultTable.GetID() {
			denyConn.IngressNetworkPolicyType = registry.PolicyTypeK8sNetworkPolicy
			denyConn.IngressNetworkPolicyRuleAction = flowexporter.RuleActionToUint8(disposition)
		} else if tableID == openflow.EgressDefaultTable.GetID() {
			denyConn.EgressNetworkPolicyType = registry.PolicyTypeK8sNetworkPolicy
			denyConn.EgressNetworkPolicyRuleAction = flowexporter.RuleActionToUint8(disposition)
		}
	}
	c.denyConnStore.AddOrUpdateConn(&denyConn, time.Now(), uint64(packet.IPLength))
	return nil
}

func isAntreaPolicyIngressTable(tableID uint8) bool {
	for _, table := range openflow.GetAntreaPolicyIngressTables() {
		if table.GetID() == tableID {
			return true
		}
	}
	return false
}

func isAntreaPolicyEgressTable(tableID uint8) bool {
	for _, table := range openflow.GetAntreaPolicyEgressTables() {
		if table.GetID() == tableID {
			return true
		}
	}
	return false
}
