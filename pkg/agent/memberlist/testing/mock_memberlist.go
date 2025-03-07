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
// Source: antrea.io/antrea/pkg/agent/memberlist (interfaces: Interface)

// Package testing is a generated GoMock package.
package testing

import (
	memberlist "antrea.io/antrea/pkg/agent/memberlist"
	gomock "github.com/golang/mock/gomock"
	sets "k8s.io/apimachinery/pkg/util/sets"
	reflect "reflect"
)

// MockInterface is a mock of Interface interface
type MockInterface struct {
	ctrl     *gomock.Controller
	recorder *MockInterfaceMockRecorder
}

// MockInterfaceMockRecorder is the mock recorder for MockInterface
type MockInterfaceMockRecorder struct {
	mock *MockInterface
}

// NewMockInterface creates a new mock instance
func NewMockInterface(ctrl *gomock.Controller) *MockInterface {
	mock := &MockInterface{ctrl: ctrl}
	mock.recorder = &MockInterfaceMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockInterface) EXPECT() *MockInterfaceMockRecorder {
	return m.recorder
}

// AddClusterEventHandler mocks base method
func (m *MockInterface) AddClusterEventHandler(arg0 memberlist.ClusterNodeEventHandler) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "AddClusterEventHandler", arg0)
}

// AddClusterEventHandler indicates an expected call of AddClusterEventHandler
func (mr *MockInterfaceMockRecorder) AddClusterEventHandler(arg0 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "AddClusterEventHandler", reflect.TypeOf((*MockInterface)(nil).AddClusterEventHandler), arg0)
}

// AliveNodes mocks base method
func (m *MockInterface) AliveNodes() sets.Set[string] {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "AliveNodes")
	ret0, _ := ret[0].(sets.Set[string])
	return ret0
}

// AliveNodes indicates an expected call of AliveNodes
func (mr *MockInterfaceMockRecorder) AliveNodes() *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "AliveNodes", reflect.TypeOf((*MockInterface)(nil).AliveNodes))
}

// SelectNodeForIP mocks base method
func (m *MockInterface) SelectNodeForIP(arg0, arg1 string, arg2 ...func(string) bool) (string, error) {
	m.ctrl.T.Helper()
	varargs := []interface{}{arg0, arg1}
	for _, a := range arg2 {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "SelectNodeForIP", varargs...)
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// SelectNodeForIP indicates an expected call of SelectNodeForIP
func (mr *MockInterfaceMockRecorder) SelectNodeForIP(arg0, arg1 interface{}, arg2 ...interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]interface{}{arg0, arg1}, arg2...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "SelectNodeForIP", reflect.TypeOf((*MockInterface)(nil).SelectNodeForIP), varargs...)
}

// ShouldSelectIP mocks base method
func (m *MockInterface) ShouldSelectIP(arg0, arg1 string, arg2 ...func(string) bool) (bool, error) {
	m.ctrl.T.Helper()
	varargs := []interface{}{arg0, arg1}
	for _, a := range arg2 {
		varargs = append(varargs, a)
	}
	ret := m.ctrl.Call(m, "ShouldSelectIP", varargs...)
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// ShouldSelectIP indicates an expected call of ShouldSelectIP
func (mr *MockInterfaceMockRecorder) ShouldSelectIP(arg0, arg1 interface{}, arg2 ...interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	varargs := append([]interface{}{arg0, arg1}, arg2...)
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ShouldSelectIP", reflect.TypeOf((*MockInterface)(nil).ShouldSelectIP), varargs...)
}
