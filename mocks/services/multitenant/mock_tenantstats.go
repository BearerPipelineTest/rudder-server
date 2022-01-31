// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/rudderlabs/rudder-server/services/multitenant (interfaces: MultiTenantI)

// Package mock_tenantstats is a generated GoMock package.
package mock_tenantstats

import (
	reflect "reflect"
	time "time"

	gomock "github.com/golang/mock/gomock"
	misc "github.com/rudderlabs/rudder-server/utils/misc"
)

// MockMultiTenantI is a mock of MultiTenantI interface.
type MockMultiTenantI struct {
	ctrl     *gomock.Controller
	recorder *MockMultiTenantIMockRecorder
}

// MockMultiTenantIMockRecorder is the mock recorder for MockMultiTenantI.
type MockMultiTenantIMockRecorder struct {
	mock *MockMultiTenantI
}

// NewMockMultiTenantI creates a new mock instance.
func NewMockMultiTenantI(ctrl *gomock.Controller) *MockMultiTenantI {
	mock := &MockMultiTenantI{ctrl: ctrl}
	mock.recorder = &MockMultiTenantIMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockMultiTenantI) EXPECT() *MockMultiTenantIMockRecorder {
	return m.recorder
}

// AddToInMemoryCount mocks base method.
func (m *MockMultiTenantI) AddToInMemoryCount(arg0, arg1 string, arg2 int, arg3 string) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "AddToInMemoryCount", arg0, arg1, arg2, arg3)
}

// AddToInMemoryCount indicates an expected call of AddToInMemoryCount.
func (mr *MockMultiTenantIMockRecorder) AddToInMemoryCount(arg0, arg1, arg2, arg3 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "AddToInMemoryCount", reflect.TypeOf((*MockMultiTenantI)(nil).AddToInMemoryCount), arg0, arg1, arg2, arg3)
}

// CalculateSuccessFailureCounts mocks base method.
func (m *MockMultiTenantI) CalculateSuccessFailureCounts(arg0, arg1 string, arg2, arg3 bool) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "CalculateSuccessFailureCounts", arg0, arg1, arg2, arg3)
}

// CalculateSuccessFailureCounts indicates an expected call of CalculateSuccessFailureCounts.
func (mr *MockMultiTenantIMockRecorder) CalculateSuccessFailureCounts(arg0, arg1, arg2, arg3 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CalculateSuccessFailureCounts", reflect.TypeOf((*MockMultiTenantI)(nil).CalculateSuccessFailureCounts), arg0, arg1, arg2, arg3)
}

// GetRouterPickupJobs mocks base method.
func (m *MockMultiTenantI) GetRouterPickupJobs(arg0 string, arg1 int, arg2 time.Duration, arg3 map[string]misc.MovingAverage, arg4 int, arg5 float64) (map[string]int, map[string]float64) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "GetRouterPickupJobs", arg0, arg1, arg2, arg3, arg4, arg5)
	ret0, _ := ret[0].(map[string]int)
	ret1, _ := ret[1].(map[string]float64)
	return ret0, ret1
}

// GetRouterPickupJobs indicates an expected call of GetRouterPickupJobs.
func (mr *MockMultiTenantIMockRecorder) GetRouterPickupJobs(arg0, arg1, arg2, arg3, arg4, arg5 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "GetRouterPickupJobs", reflect.TypeOf((*MockMultiTenantI)(nil).GetRouterPickupJobs), arg0, arg1, arg2, arg3, arg4, arg5)
}

// RemoveFromInMemoryCount mocks base method.
func (m *MockMultiTenantI) RemoveFromInMemoryCount(arg0, arg1 string, arg2 int, arg3 string) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "RemoveFromInMemoryCount", arg0, arg1, arg2, arg3)
}

// RemoveFromInMemoryCount indicates an expected call of RemoveFromInMemoryCount.
func (mr *MockMultiTenantIMockRecorder) RemoveFromInMemoryCount(arg0, arg1, arg2, arg3 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "RemoveFromInMemoryCount", reflect.TypeOf((*MockMultiTenantI)(nil).RemoveFromInMemoryCount), arg0, arg1, arg2, arg3)
}

// ReportProcLoopAddStats mocks base method.
func (m *MockMultiTenantI) ReportProcLoopAddStats(arg0 map[string]map[string]int, arg1 time.Duration, arg2 string) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "ReportProcLoopAddStats", arg0, arg1, arg2)
}

// ReportProcLoopAddStats indicates an expected call of ReportProcLoopAddStats.
func (mr *MockMultiTenantIMockRecorder) ReportProcLoopAddStats(arg0, arg1, arg2 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "ReportProcLoopAddStats", reflect.TypeOf((*MockMultiTenantI)(nil).ReportProcLoopAddStats), arg0, arg1, arg2)
}
