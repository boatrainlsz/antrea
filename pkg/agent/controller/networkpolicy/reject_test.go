// Copyright 2023 Antrea Authors
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
	"encoding/binary"
	"net"
	"testing"

	"antrea.io/libOpenflow/openflow15"
	"antrea.io/ofnet/ofctrl"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"antrea.io/antrea/pkg/agent/config"
	"antrea.io/antrea/pkg/agent/interfacestore"
	"antrea.io/antrea/pkg/agent/openflow"
	binding "antrea.io/antrea/pkg/ovs/openflow"
	mocks "antrea.io/antrea/pkg/ovs/openflow/testing"
)

func TestGetRejectType(t *testing.T) {
	tests := []struct {
		name               string
		isServiceTraffic   bool
		antreaProxyEnabled bool
		srcIsLocal         bool
		dstIsLocal         bool
		expectRejectType   RejectType
	}{
		{
			name:               "RejectPodLocal",
			isServiceTraffic:   false,
			antreaProxyEnabled: true,
			srcIsLocal:         true,
			dstIsLocal:         true,
			expectRejectType:   RejectPodLocal,
		},
		{
			name:               "RejectPodRemoteToLocal",
			isServiceTraffic:   false,
			antreaProxyEnabled: true,
			srcIsLocal:         false,
			dstIsLocal:         true,
			expectRejectType:   RejectPodRemoteToLocal,
		},
		{
			name:               "RejectPodLocalToRemote",
			isServiceTraffic:   false,
			antreaProxyEnabled: true,
			srcIsLocal:         true,
			dstIsLocal:         false,
			expectRejectType:   RejectPodLocalToRemote,
		},
		{
			name:               "RejectServiceLocal",
			isServiceTraffic:   true,
			antreaProxyEnabled: true,
			srcIsLocal:         true,
			dstIsLocal:         true,
			expectRejectType:   RejectServiceLocal,
		},
		{
			name:               "RejectServiceRemoteToLocal",
			isServiceTraffic:   true,
			antreaProxyEnabled: true,
			srcIsLocal:         false,
			dstIsLocal:         true,
			expectRejectType:   RejectServiceRemoteToLocal,
		},
		{
			name:               "RejectServiceLocalToRemote",
			isServiceTraffic:   true,
			antreaProxyEnabled: true,
			srcIsLocal:         true,
			dstIsLocal:         false,
			expectRejectType:   RejectServiceLocalToRemote,
		},
		{
			name:               "RejectNoAPServiceLocal",
			isServiceTraffic:   true,
			antreaProxyEnabled: false,
			srcIsLocal:         true,
			dstIsLocal:         true,
			expectRejectType:   RejectNoAPServiceLocal,
		},
		{
			name:               "RejectNoAPServiceRemoteToLocal",
			isServiceTraffic:   true,
			antreaProxyEnabled: false,
			srcIsLocal:         false,
			dstIsLocal:         true,
			expectRejectType:   RejectNoAPServiceRemoteToLocal,
		},
		{
			name:               "RejectServiceRemoteToExternal",
			isServiceTraffic:   true,
			antreaProxyEnabled: true,
			srcIsLocal:         false,
			dstIsLocal:         false,
			expectRejectType:   RejectServiceRemoteToExternal,
		},
		{
			name:               "Unsupported pod2pod remote2remote",
			isServiceTraffic:   false,
			antreaProxyEnabled: true,
			srcIsLocal:         false,
			dstIsLocal:         false,
			expectRejectType:   Unsupported,
		},
		{
			name:               "Unsupported noAP remote2remote",
			isServiceTraffic:   true,
			antreaProxyEnabled: false,
			srcIsLocal:         false,
			dstIsLocal:         false,
			expectRejectType:   Unsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rejectType := getRejectType(tt.isServiceTraffic, tt.antreaProxyEnabled, tt.srcIsLocal, tt.dstIsLocal)
			assert.Equal(t, tt.expectRejectType, rejectType)
		})
	}
}

func Test_parseFlexibleIPAMStatus(t *testing.T) {
	ctZone := uint16(1)
	ctZoneBytes := make([]byte, 8)
	binary.BigEndian.PutUint16(ctZoneBytes, ctZone)
	type args struct {
		pktIn      *ofctrl.PacketIn
		nodeConfig *config.NodeConfig
		srcIP      string
		srcIsLocal bool
		dstIP      string
		dstIsLocal bool
	}
	tests := []struct {
		name                  string
		args                  args
		wantIsFlexibleIPAMSrc bool
		wantIsFlexibleIPAMDst bool
		wantCtZone            uint32
		wantErr               bool
	}{
		{
			name: "NoFlexibleIPAM",
			args: args{
				pktIn:      &ofctrl.PacketIn{PacketIn: &openflow15.PacketIn{}},
				nodeConfig: nil,
				srcIP:      "",
				srcIsLocal: false,
				dstIP:      "",
				dstIsLocal: false,
			},
			wantIsFlexibleIPAMSrc: false,
			wantIsFlexibleIPAMDst: false,
			wantCtZone:            0,
			wantErr:               false,
		},
		{
			name: "FlexibleIPAM",
			args: args{
				pktIn: &ofctrl.PacketIn{
					PacketIn: &openflow15.PacketIn{
						Match: openflow15.Match{
							Type:   0,
							Length: 0,
							Fields: []openflow15.MatchField{{
								Class:          openflow15.OXM_CLASS_PACKET_REGS,
								Field:          4,
								HasMask:        false,
								Length:         0,
								ExperimenterID: 0,
								Value: &openflow15.ByteArrayField{
									Data:   []byte{0, 0, 0, 1, 0, 0, 0, 0},
									Length: 64,
								},
								Mask: nil,
							}},
						},
					},
				},
				nodeConfig: &config.NodeConfig{
					PodIPv4CIDR: &net.IPNet{
						IP:   net.IPv4(1, 2, 2, 0),
						Mask: net.IPv4Mask(255, 255, 255, 0),
					},
				},
				srcIP:      "1.2.3.4",
				srcIsLocal: true,
				dstIP:      "1.2.3.5",
				dstIsLocal: true,
			},
			wantIsFlexibleIPAMSrc: true,
			wantIsFlexibleIPAMDst: true,
			wantCtZone:            1,
			wantErr:               false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIsFlexibleIPAMSrc, gotIsFlexibleIPAMDst, gotCtZone, err := parseFlexibleIPAMStatus(tt.args.pktIn, tt.args.nodeConfig, tt.args.srcIP, tt.args.srcIsLocal, tt.args.dstIP, tt.args.dstIsLocal)
			matches := tt.args.pktIn.GetMatches()
			match := getMatchRegField(matches, openflow.CtZoneField)
			t.Logf("match: %+v", match)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseFlexibleIPAMStatus() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotIsFlexibleIPAMSrc != tt.wantIsFlexibleIPAMSrc {
				t.Errorf("parseFlexibleIPAMStatus() gotIsFlexibleIPAMSrc = %v, want %v", gotIsFlexibleIPAMSrc, tt.wantIsFlexibleIPAMSrc)
			}
			if gotIsFlexibleIPAMDst != tt.wantIsFlexibleIPAMDst {
				t.Errorf("parseFlexibleIPAMStatus() gotIsFlexibleIPAMDst = %v, want %v", gotIsFlexibleIPAMDst, tt.wantIsFlexibleIPAMDst)
			}
			if gotCtZone != tt.wantCtZone {
				t.Errorf("parseFlexibleIPAMStatus() gotCtZone = %v, want %v", gotCtZone, tt.wantCtZone)
			}
		})
	}
}

func TestGetRejectOFPorts(t *testing.T) {
	unsetPort := uint32(0)
	tunPort := uint32(1)
	gwPort := uint32(2)
	srcIface := &interfacestore.InterfaceConfig{
		OVSPortConfig: &interfacestore.OVSPortConfig{
			OFPort: 3,
		},
	}
	externalSrcIface := &interfacestore.InterfaceConfig{
		Type: interfacestore.ExternalEntityInterface,
		OVSPortConfig: &interfacestore.OVSPortConfig{
			OFPort: 3,
		},
		EntityInterfaceConfig: &interfacestore.EntityInterfaceConfig{
			UplinkPort: &interfacestore.OVSPortConfig{
				OFPort: 4,
			},
		},
	}
	dstIface := &interfacestore.InterfaceConfig{
		OVSPortConfig: &interfacestore.OVSPortConfig{
			OFPort: 5,
		},
	}
	externalDstIface := &interfacestore.InterfaceConfig{
		Type: interfacestore.ExternalEntityInterface,
		OVSPortConfig: &interfacestore.OVSPortConfig{
			OFPort: 5,
		},
		EntityInterfaceConfig: &interfacestore.EntityInterfaceConfig{
			UplinkPort: &interfacestore.OVSPortConfig{
				OFPort: 6,
			},
		},
	}
	tests := []struct {
		name          string
		rejectType    RejectType
		tunPort       uint32
		srcInterface  *interfacestore.InterfaceConfig
		dstInterface  *interfacestore.InterfaceConfig
		expectInPort  uint32
		expectOutPort uint32
	}{
		{
			name:          "RejectPodLocal",
			rejectType:    RejectPodLocal,
			srcInterface:  srcIface,
			dstInterface:  dstIface,
			expectInPort:  uint32(srcIface.OFPort),
			expectOutPort: uint32(dstIface.OFPort),
		},
		{
			name:          "RejectPodLocalToRemote",
			rejectType:    RejectPodLocalToRemote,
			srcInterface:  srcIface,
			expectInPort:  uint32(srcIface.OFPort),
			expectOutPort: unsetPort,
		},
		{
			name:          "RejectPodLocalToRemoteExternal",
			rejectType:    RejectPodLocalToRemote,
			srcInterface:  externalSrcIface,
			expectInPort:  uint32(externalSrcIface.OFPort),
			expectOutPort: uint32(externalSrcIface.UplinkPort.OFPort),
		},
		{
			name:          "RejectPodRemoteToLocal",
			rejectType:    RejectPodRemoteToLocal,
			dstInterface:  dstIface,
			expectInPort:  gwPort,
			expectOutPort: uint32(dstIface.OFPort),
		},
		{
			name:          "RejectPodRemoteToLocalExternal",
			rejectType:    RejectPodRemoteToLocal,
			dstInterface:  externalDstIface,
			expectInPort:  uint32(externalDstIface.UplinkPort.OFPort),
			expectOutPort: uint32(externalDstIface.OFPort),
		},
		{
			name:          "RejectServiceLocal",
			rejectType:    RejectServiceLocal,
			srcInterface:  srcIface,
			expectInPort:  uint32(srcIface.OFPort),
			expectOutPort: unsetPort,
		},
		{
			name:          "RejectServiceLocalToRemote",
			rejectType:    RejectServiceLocalToRemote,
			srcInterface:  srcIface,
			expectInPort:  uint32(srcIface.OFPort),
			expectOutPort: unsetPort,
		},
		{
			name:          "RejectServiceRemoteToLocal",
			rejectType:    RejectServiceRemoteToLocal,
			expectInPort:  gwPort,
			expectOutPort: unsetPort,
		},
		{
			name:          "RejectNoAPServiceLocal",
			rejectType:    RejectNoAPServiceLocal,
			srcInterface:  srcIface,
			expectInPort:  uint32(srcIface.OFPort),
			expectOutPort: gwPort,
		},
		{
			name:          "RejectNoAPServiceRemoteToLocal",
			rejectType:    RejectNoAPServiceRemoteToLocal,
			tunPort:       tunPort,
			expectInPort:  tunPort,
			expectOutPort: gwPort,
		},
		{
			name:          "RejectNoAPServiceRemoteToLocalWithoutTun",
			rejectType:    RejectNoAPServiceRemoteToLocal,
			tunPort:       unsetPort,
			expectInPort:  gwPort,
			expectOutPort: gwPort,
		},
		{
			name:          "RejectServiceRemoteToExternal",
			rejectType:    RejectServiceRemoteToExternal,
			tunPort:       tunPort,
			expectInPort:  tunPort,
			expectOutPort: unsetPort,
		},
		{
			name:          "RejectServiceRemoteToExternalWithoutTun",
			rejectType:    RejectServiceRemoteToExternal,
			tunPort:       unsetPort,
			expectInPort:  gwPort,
			expectOutPort: unsetPort,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inPort, outPort := getRejectOFPorts(tt.rejectType, tt.srcInterface, tt.dstInterface, gwPort, tt.tunPort)
			assert.Equal(t, tt.expectInPort, inPort)
			assert.Equal(t, tt.expectOutPort, outPort)
		})
	}
}

func Test_getRejectPacketOutMutateFunc(t *testing.T) {
	openflow.InitMockTables(
		map[*openflow.Table]uint8{
			openflow.ConntrackTable:        uint8(5),
			openflow.L3ForwardingTable:     uint8(6),
			openflow.L2ForwardingCalcTable: uint8(7),
		})
	conntrackTableID := openflow.ConntrackTable.GetID()
	l3ForwardingTableID := openflow.L3ForwardingTable.GetID()
	ctrl := gomock.NewController(t)
	type args struct {
		rejectType        RejectType
		nodeType          config.NodeType
		isFlexibleIPAMSrc bool
		isFlexibleIPAMDst bool
		ctZone            uint32
	}
	tests := []struct {
		name        string
		args        args
		prepareFunc func(builder *mocks.MockPacketOutBuilder)
	}{
		{
			name: "RejectServiceLocalFlexibleIPAMSrc",
			args: args{
				rejectType:        RejectServiceLocal,
				nodeType:          config.K8sNode,
				isFlexibleIPAMSrc: true,
				isFlexibleIPAMDst: false,
				ctZone:            1,
			},
			prepareFunc: func(builder *mocks.MockPacketOutBuilder) {
				builder.EXPECT().AddLoadRegMark(openflow.GeneratedRejectPacketOutRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(openflow.AntreaFlexibleIPAMRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(binding.NewRegMark(openflow.CtZoneField, 1)).Return(builder)
				builder.EXPECT().AddResubmitAction(nil, &conntrackTableID).Return(builder)
			},
		},
		{
			name: "RejectServiceOtherSrc",
			args: args{
				rejectType:        RejectServiceLocal,
				nodeType:          config.K8sNode,
				isFlexibleIPAMSrc: false,
				isFlexibleIPAMDst: false,
				ctZone:            1,
			},
			prepareFunc: func(builder *mocks.MockPacketOutBuilder) {
				builder.EXPECT().AddLoadRegMark(openflow.GeneratedRejectPacketOutRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(binding.NewRegMark(openflow.CtZoneField, 1)).Return(builder)
				builder.EXPECT().AddResubmitAction(nil, &conntrackTableID).Return(builder)
			},
		},
		{
			name: "RejectLocalToRemoteFlexibleIPAMSrc",
			args: args{
				rejectType:        RejectPodLocalToRemote,
				nodeType:          config.K8sNode,
				isFlexibleIPAMSrc: true,
				isFlexibleIPAMDst: false,
				ctZone:            1,
			},
			prepareFunc: func(builder *mocks.MockPacketOutBuilder) {
				builder.EXPECT().AddLoadRegMark(openflow.GeneratedRejectPacketOutRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(openflow.AntreaFlexibleIPAMRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(binding.NewRegMark(openflow.CtZoneField, 1)).Return(builder)
				builder.EXPECT().AddResubmitAction(nil, &l3ForwardingTableID).Return(builder)
			},
		},
		{
			name: "RejectLocalToRemoteFlexibleIPAMSrc",
			args: args{
				rejectType:        RejectPodLocalToRemote,
				nodeType:          config.K8sNode,
				isFlexibleIPAMSrc: false,
				isFlexibleIPAMDst: false,
				ctZone:            1,
			},
			prepareFunc: func(builder *mocks.MockPacketOutBuilder) {
				builder.EXPECT().AddLoadRegMark(openflow.GeneratedRejectPacketOutRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(binding.NewRegMark(openflow.CtZoneField, 1)).Return(builder)
				builder.EXPECT().AddResubmitAction(nil, &l3ForwardingTableID).Return(builder)
			},
		},
		{
			name: "RejectServiceRemoteToLocalFlexibleIPAMDst",
			args: args{
				rejectType:        RejectServiceRemoteToLocal,
				nodeType:          config.K8sNode,
				isFlexibleIPAMSrc: false,
				isFlexibleIPAMDst: true,
				ctZone:            1,
			},
			prepareFunc: func(builder *mocks.MockPacketOutBuilder) {
				builder.EXPECT().AddLoadRegMark(openflow.GeneratedRejectPacketOutRegMark).Return(builder)
				builder.EXPECT().AddLoadRegMark(binding.NewRegMark(openflow.CtZoneField, 1)).Return(builder)
				builder.EXPECT().AddResubmitAction(nil, &conntrackTableID).Return(builder)
			},
		},
		{
			name: "Default",
			args: args{
				rejectType:        RejectServiceRemoteToLocal,
				nodeType:          config.K8sNode,
				isFlexibleIPAMSrc: false,
				isFlexibleIPAMDst: false,
				ctZone:            0,
			},
			prepareFunc: func(builder *mocks.MockPacketOutBuilder) {
				builder.EXPECT().AddLoadRegMark(openflow.GeneratedRejectPacketOutRegMark).Return(builder)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := mocks.NewMockPacketOutBuilder(ctrl)
			tt.prepareFunc(builder)
			getRejectPacketOutMutateFunc(tt.args.rejectType, tt.args.nodeType, tt.args.isFlexibleIPAMSrc, tt.args.isFlexibleIPAMDst, tt.args.ctZone)(builder)
		})
	}
}
