// Automatically generated by MockGen. DO NOT EDIT!
// Source: src/github.com/mozilla-services/pushgo/simplepush/balancer.go

package simplepush

import (
	gomock "github.com/rafrombrc/gomock/gomock"
)

// Mock of Balancer interface
type MockBalancer struct {
	ctrl     *gomock.Controller
	recorder *_MockBalancerRecorder
}

// Recorder for MockBalancer (not exported)
type _MockBalancerRecorder struct {
	mock *MockBalancer
}

func NewMockBalancer(ctrl *gomock.Controller) *MockBalancer {
	mock := &MockBalancer{ctrl: ctrl}
	mock.recorder = &_MockBalancerRecorder{mock}
	return mock
}

func (_m *MockBalancer) EXPECT() *_MockBalancerRecorder {
	return _m.recorder
}

func (_m *MockBalancer) RedirectURL() (string, bool, error) {
	ret := _m.ctrl.Call(_m, "RedirectURL")
	ret0, _ := ret[0].(string)
	ret1, _ := ret[1].(bool)
	ret2, _ := ret[2].(error)
	return ret0, ret1, ret2
}

func (_mr *_MockBalancerRecorder) RedirectURL() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "RedirectURL")
}

func (_m *MockBalancer) Status() (bool, error) {
	ret := _m.ctrl.Call(_m, "Status")
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

func (_mr *_MockBalancerRecorder) Status() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Status")
}

func (_m *MockBalancer) Close() error {
	ret := _m.ctrl.Call(_m, "Close")
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockBalancerRecorder) Close() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Close")
}
