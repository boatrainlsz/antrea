// Copyright 2023 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// Code generated by MockGen. DO NOT EDIT.
// Source: antrea.io/antrea/pkg/querier (interfaces: AgentNetworkPolicyInfoQuerier,AgentMulticastInfoQuerier,EgressQuerier)

// Package testing is a generated GoMock package.
package testing

import (
	interfacestore "antrea.io/antrea/pkg/agent/interfacestore"
	multicast "antrea.io/antrea/pkg/agent/multicast"
	types "antrea.io/antrea/pkg/agent/types"
	v1beta2 "antrea.io/antrea/pkg/apis/controlplane/v1beta2"
	querier "antrea.io/antrea/pkg/querier"
	gomock "github.com/golang/mock/gomock"
	types0 "k8s.io/apimachinery/pkg/types"
	reflect "reflect"
)

// MockAgentNetworkPolicyInfoQuerier is a mock of AgentNetworkPolicyInfoQuerier interface
type MockAgentNetworkPolicyInfoQuerier struct {
	ctrl     *gomock.Controller
	recorder *MockAgentNetworkPolicyInfoQuerierMockRecorder
}

// MockAgentNetworkPolicyInfoQuerierMockRecorder is the mock recorder for MockAgentNetworkPolicyInfoQuerier
type MockAgentNetworkPolicyInfoQuerierMockRecorder struct {
	mock *MockAgentNetworkPolicyInfoQuerier
}

// NewMockAgentNetworkPolicyInfoQuerier creates a new mock instance
func NewMockAgentNetworkPolicyInfoQuerier(ctrl *gomock.Controller) *MockAgentNetworkPolicyInfoQuerier {
	mock := &MockAgentNetworkPolicyInfoQuerier{ctrl: ctrl}
	mock.recorder = &MockAgentNetworkPolicyInfoQuerierMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockAgentNetworkPolicyInfoQuerier) EXPECT() *MockAgentNetworkPolicyInfoQuerierMockRecorder {
	return m.recorder
}

// GetAddressGroupNum mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetAddressGroupNum() int {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAddressGroupNum")
	ret0, _ := ret[0].(int)
	return ret0
}

// GetAddressGroupNum indicates an expected call of GetAddressGroupNum
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetAddressGroupNum() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAddressGroupNum", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetAddressGroupNum))
}

// GetAddressGroups mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetAddressGroups() []v1beta2.AddressGroup {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAddressGroups")
	ret0, _ := ret[0].([]v1beta2.AddressGroup)
	return ret0
}

// GetAddressGroups indicates an expected call of GetAddressGroups
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetAddressGroups() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAddressGroups", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetAddressGroups))
}

// GetAppliedNetworkPolicies mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetAppliedNetworkPolicies(arg0, arg1 string, arg2 *querier.NetworkPolicyQueryFilter) []v1beta2.NetworkPolicy {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAppliedNetworkPolicies", arg0, arg1, arg2)
	ret0, _ := ret[0].([]v1beta2.NetworkPolicy)
	return ret0
}

// GetAppliedNetworkPolicies indicates an expected call of GetAppliedNetworkPolicies
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetAppliedNetworkPolicies(arg0, arg1, arg2 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAppliedNetworkPolicies", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetAppliedNetworkPolicies), arg0, arg1, arg2)
}

// GetAppliedToGroupNum mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetAppliedToGroupNum() int {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAppliedToGroupNum")
	ret0, _ := ret[0].(int)
	return ret0
}

// GetAppliedToGroupNum indicates an expected call of GetAppliedToGroupNum
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetAppliedToGroupNum() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAppliedToGroupNum", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetAppliedToGroupNum))
}

// GetAppliedToGroups mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetAppliedToGroups() []v1beta2.AppliedToGroup {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAppliedToGroups")
	ret0, _ := ret[0].([]v1beta2.AppliedToGroup)
	return ret0
}

// GetAppliedToGroups indicates an expected call of GetAppliedToGroups
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetAppliedToGroups() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAppliedToGroups", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetAppliedToGroups))
}

// GetControllerConnectionStatus mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetControllerConnectionStatus() bool {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetControllerConnectionStatus")
	ret0, _ := ret[0].(bool)
	return ret0
}

// GetControllerConnectionStatus indicates an expected call of GetControllerConnectionStatus
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetControllerConnectionStatus() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetControllerConnectionStatus", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetControllerConnectionStatus))
}

// GetNetworkPolicies mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetNetworkPolicies(arg0 *querier.NetworkPolicyQueryFilter) []v1beta2.NetworkPolicy {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetNetworkPolicies", arg0)
	ret0, _ := ret[0].([]v1beta2.NetworkPolicy)
	return ret0
}

// GetNetworkPolicies indicates an expected call of GetNetworkPolicies
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetNetworkPolicies(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetNetworkPolicies", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetNetworkPolicies), arg0)
}

// GetNetworkPolicyByRuleFlowID mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetNetworkPolicyByRuleFlowID(arg0 uint32) *v1beta2.NetworkPolicyReference {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetNetworkPolicyByRuleFlowID", arg0)
	ret0, _ := ret[0].(*v1beta2.NetworkPolicyReference)
	return ret0
}

// GetNetworkPolicyByRuleFlowID indicates an expected call of GetNetworkPolicyByRuleFlowID
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetNetworkPolicyByRuleFlowID(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetNetworkPolicyByRuleFlowID", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetNetworkPolicyByRuleFlowID), arg0)
}

// GetNetworkPolicyNum mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetNetworkPolicyNum() int {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetNetworkPolicyNum")
	ret0, _ := ret[0].(int)
	return ret0
}

// GetNetworkPolicyNum indicates an expected call of GetNetworkPolicyNum
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetNetworkPolicyNum() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetNetworkPolicyNum", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetNetworkPolicyNum))
}

// GetRuleByFlowID mocks base method
func (m *MockAgentNetworkPolicyInfoQuerier) GetRuleByFlowID(arg0 uint32) *types.PolicyRule {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetRuleByFlowID", arg0)
	ret0, _ := ret[0].(*types.PolicyRule)
	return ret0
}

// GetRuleByFlowID indicates an expected call of GetRuleByFlowID
func (mr *MockAgentNetworkPolicyInfoQuerierMockRecorder) GetRuleByFlowID(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetRuleByFlowID", reflect.TypeOf((*MockAgentNetworkPolicyInfoQuerier)(nil).GetRuleByFlowID), arg0)
}

// MockAgentMulticastInfoQuerier is a mock of AgentMulticastInfoQuerier interface
type MockAgentMulticastInfoQuerier struct {
	ctrl     *gomock.Controller
	recorder *MockAgentMulticastInfoQuerierMockRecorder
}

// MockAgentMulticastInfoQuerierMockRecorder is the mock recorder for MockAgentMulticastInfoQuerier
type MockAgentMulticastInfoQuerierMockRecorder struct {
	mock *MockAgentMulticastInfoQuerier
}

// NewMockAgentMulticastInfoQuerier creates a new mock instance
func NewMockAgentMulticastInfoQuerier(ctrl *gomock.Controller) *MockAgentMulticastInfoQuerier {
	mock := &MockAgentMulticastInfoQuerier{ctrl: ctrl}
	mock.recorder = &MockAgentMulticastInfoQuerierMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockAgentMulticastInfoQuerier) EXPECT() *MockAgentMulticastInfoQuerierMockRecorder {
	return m.recorder
}

// CollectIGMPReportNPStats mocks base method
func (m *MockAgentMulticastInfoQuerier) CollectIGMPReportNPStats() (map[types0.UID]map[string]*types.RuleMetric, map[types0.UID]map[string]*types.RuleMetric) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "CollectIGMPReportNPStats")
	ret0, _ := ret[0].(map[types0.UID]map[string]*types.RuleMetric)
	ret1, _ := ret[1].(map[types0.UID]map[string]*types.RuleMetric)
	return ret0, ret1
}

// CollectIGMPReportNPStats indicates an expected call of CollectIGMPReportNPStats
func (mr *MockAgentMulticastInfoQuerierMockRecorder) CollectIGMPReportNPStats() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CollectIGMPReportNPStats", reflect.TypeOf((*MockAgentMulticastInfoQuerier)(nil).CollectIGMPReportNPStats))
}

// GetAllPodsStats mocks base method
func (m *MockAgentMulticastInfoQuerier) GetAllPodsStats() map[*interfacestore.InterfaceConfig]*multicast.PodTrafficStats {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetAllPodsStats")
	ret0, _ := ret[0].(map[*interfacestore.InterfaceConfig]*multicast.PodTrafficStats)
	return ret0
}

// GetAllPodsStats indicates an expected call of GetAllPodsStats
func (mr *MockAgentMulticastInfoQuerierMockRecorder) GetAllPodsStats() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetAllPodsStats", reflect.TypeOf((*MockAgentMulticastInfoQuerier)(nil).GetAllPodsStats))
}

// GetGroupPods mocks base method
func (m *MockAgentMulticastInfoQuerier) GetGroupPods() map[string][]v1beta2.PodReference {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetGroupPods")
	ret0, _ := ret[0].(map[string][]v1beta2.PodReference)
	return ret0
}

// GetGroupPods indicates an expected call of GetGroupPods
func (mr *MockAgentMulticastInfoQuerierMockRecorder) GetGroupPods() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetGroupPods", reflect.TypeOf((*MockAgentMulticastInfoQuerier)(nil).GetGroupPods))
}

// GetPodStats mocks base method
func (m *MockAgentMulticastInfoQuerier) GetPodStats(arg0, arg1 string) *multicast.PodTrafficStats {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetPodStats", arg0, arg1)
	ret0, _ := ret[0].(*multicast.PodTrafficStats)
	return ret0
}

// GetPodStats indicates an expected call of GetPodStats
func (mr *MockAgentMulticastInfoQuerierMockRecorder) GetPodStats(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetPodStats", reflect.TypeOf((*MockAgentMulticastInfoQuerier)(nil).GetPodStats), arg0, arg1)
}

// MockEgressQuerier is a mock of EgressQuerier interface
type MockEgressQuerier struct {
	ctrl     *gomock.Controller
	recorder *MockEgressQuerierMockRecorder
}

// MockEgressQuerierMockRecorder is the mock recorder for MockEgressQuerier
type MockEgressQuerierMockRecorder struct {
	mock *MockEgressQuerier
}

// NewMockEgressQuerier creates a new mock instance
func NewMockEgressQuerier(ctrl *gomock.Controller) *MockEgressQuerier {
	mock := &MockEgressQuerier{ctrl: ctrl}
	mock.recorder = &MockEgressQuerierMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockEgressQuerier) EXPECT() *MockEgressQuerierMockRecorder {
	return m.recorder
}

// GetEgress mocks base method
func (m *MockEgressQuerier) GetEgress(arg0, arg1 string) (string, string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetEgress", arg0, arg1)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(string)
	ret2, _ := ret[2].(error)
	return ret0, ret1, ret2
}

// GetEgress indicates an expected call of GetEgress
func (mr *MockEgressQuerierMockRecorder) GetEgress(arg0, arg1 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetEgress", reflect.TypeOf((*MockEgressQuerier)(nil).GetEgress), arg0, arg1)
}

// GetEgressIPByMark mocks base method
func (m *MockEgressQuerier) GetEgressIPByMark(arg0 uint32) (string, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetEgressIPByMark", arg0)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// GetEgressIPByMark indicates an expected call of GetEgressIPByMark
func (mr *MockEgressQuerierMockRecorder) GetEgressIPByMark(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetEgressIPByMark", reflect.TypeOf((*MockEgressQuerier)(nil).GetEgressIPByMark), arg0)
}
