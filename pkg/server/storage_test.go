/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/metadata"

	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
)

func TestNewBackend(t *testing.T) {
	testCases := []struct {
		desc    string
		ids     []string
		idents  []string
		wantErr error
	}{
		{
			desc:    "no agentID",
			wantErr: fmt.Errorf("expected one agent ID in the context, got []"),
		},
		{
			desc:    "multiple agentID",
			ids:     []string{"agent-id", "agent-id"},
			wantErr: fmt.Errorf("expected one agent ID in the context, got [agent-id agent-id]"),
		},
		{
			desc:    "multiple identifiers",
			ids:     []string{"agent-id"},
			idents:  []string{"host=localhost", "host=localhost"},
			wantErr: fmt.Errorf("expected at most one set of agent identifiers in the context, got [host=localhost host=localhost]"),
		},
		{
			desc:    "invalid identifiers",
			ids:     []string{"agent-id"},
			idents:  []string{";"},
			wantErr: fmt.Errorf("fail to parse url encoded string: invalid semicolon separator in query"),
		},
		{
			desc: "success",
			ids:  []string{"agent-id"},
		},
		{
			desc:   "success with identifiers",
			ids:    []string{"agent-id"},
			idents: []string{"host=localhost&host=node1.mydomain.com&cidr=127.0.0.1/16&ipv4=1.2.3.4&ipv4=5.6.7.8&ipv6=:::::&default-route=true"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			agentConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
			agentConnMD := metadata.MD{
				":authority":       []string{"127.0.0.1:8091"},
				"agentid":          tc.ids,
				"agentidentifiers": tc.idents,
				"content-type":     []string{"application/grpc"},
				"user-agent":       []string{"grpc-go/1.42.0"},
			}
			agentConnCtx := metadata.NewIncomingContext(context.Background(), agentConnMD)
			agentConn.EXPECT().Context().Return(agentConnCtx).AnyTimes()

			_, got := NewBackend(agentConn)
			assert.Equal(t, got, tc.wantErr, "NewBackend() error %q, want %v", got, tc.wantErr)
		})
	}
}

func TestBackendStorage_AddBackend(t *testing.T) {
	testCases := []struct {
		desc        string
		keys        []string
		expectCount int
	}{
		{
			desc:        "empty",
			keys:        []string{},
			expectCount: 0,
		},
		{
			desc:        "one key",
			keys:        []string{"ident1"},
			expectCount: 1,
		},
		{
			desc:        "multi keys",
			keys:        []string{"ident1", "ident2", "ident3"},
			expectCount: 3,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
			s := NewDefaultBackendStorage()

			gotCount := s.AddBackend(tc.keys, backend1)
			if gotCount != tc.expectCount {
				t.Errorf("AddBackend(%v) = %d, want %d", tc.keys, gotCount, tc.expectCount)
			}
		})
	}
}

func TestBackendStorage_AddBackend_DuplicateKey(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"hostname1"}, backend1)
	gotCount := s.AddBackend([]string{"hostname1"}, backend2)
	if gotCount != 1 {
		t.Errorf("AddBackend() = %v, want 1", gotCount)
	}
}

func TestBackendStorage_AddBackend_SameBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"ident1"}, backend1)
	gotCount := s.AddBackend([]string{"ident1"}, backend1)
	if gotCount != 1 {
		t.Errorf("AddBackend() = %v, want 1", gotCount)
	}
	gotLen := len(s.backends["ident1"])
	if gotLen != 1 {
		t.Errorf("backends list = %v, want length 1", s.backends["ident1"])
	}
}

func TestBackendStorage_GetBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend22, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"ident1"}, backend1)
	s.AddBackend([]string{"ident1", "ident2"}, backend2)
	s.AddBackend([]string{"ident1", "ident2"}, backend22)
	s.AddBackend([]string{"ident1", "ident2", "ident3"}, backend3)

	testCases := []struct {
		desc    string
		key     string
		want    *Backend
		wantErr error
	}{
		{
			desc:    "empty",
			key:     "",
			wantErr: &ErrNotFound{},
		},
		{
			desc: "by key",
			key:  "ident3",
			want: backend3,
		},
		{
			desc: "prefer backend first added",
			key:  "ident1",
			want: backend1,
		},
		{
			desc: "prefer first stream for a given agent",
			key:  "ident2",
			want: backend2,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := s.GetBackend(tc.key)
			assert.Equal(t, got, tc.want, "GetBackend(%v) = %v, want %v", tc.key, got, tc.want)
			assert.Equal(t, err, tc.wantErr, "GetBackend(%v) error %q, want %v", tc.key, err, tc.wantErr)
		})
	}
}

func TestBackendStorage_RemoveBackend_Empty(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"ident1"}, backend1)

	gotCount := s.RemoveBackend([]string{""}, backend1)
	if gotCount != 1 {
		t.Errorf("RemoveBackend() = %v, want 1", gotCount)
	}
}

func TestBackendStorage_RemoveBackend_Unrecognized(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"ident1"}, backend1)

	gotCount := s.RemoveBackend([]string{"ident2"}, backend1)
	if gotCount != 1 {
		t.Errorf("RemoveBackend() = %v, want 1", gotCount)
	}
}

func TestBackendStorage_RemoveBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend22, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"ident1"}, backend1)
	s.AddBackend([]string{"ident1", "ident2"}, backend2)
	s.AddBackend([]string{"ident1", "ident2"}, backend22)
	s.AddBackend([]string{"ident2"}, backend3)

	wantBackends := map[string][]*Backend{
		"ident1": {backend1, backend2, backend22},
		"ident2": {backend2, backend22, backend3},
	}
	wantBackendKeys := []string{"ident1", "ident2"}
	if !reflect.DeepEqual(s.backends, wantBackends) {
		t.Errorf("s.backends = %v, want %v", s.backends, wantBackends)
	}
	if !reflect.DeepEqual(s.backendKeys, wantBackendKeys) {
		t.Errorf("s.backendKeys = %v, want %v", s.backendKeys, wantBackendKeys)
	}

	gotCount := s.RemoveBackend([]string{"ident1", "ident2"}, backend22)
	if gotCount != 2 {
		t.Errorf("RemoveBackend() = %v, want 2", gotCount)
	}
	wantBackends = map[string][]*Backend{
		"ident1": {backend1, backend2},
		"ident2": {backend2, backend3},
	}
	if !reflect.DeepEqual(s.backends, wantBackends) {
		t.Errorf("s.backends = %v, want %v", s.backends, wantBackends)
	}
	if !reflect.DeepEqual(s.backendKeys, wantBackendKeys) {
		t.Errorf("s.backendKeys = %v, want %v", s.backendKeys, wantBackendKeys)
	}
}

func TestBackendStorage_AddBackend_DuplicateAgentID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend12, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	s := NewDefaultBackendStorage()

	s.AddBackend([]string{"ident1"}, backend1)
	gotCount := s.AddBackend([]string{"ident1"}, backend12)
	if gotCount != 1 {
		t.Errorf("AddBackend() = %v, want 1", gotCount)
	}

	got, _ := s.GetBackend("ident1")
	if got != backend1 {
		t.Errorf("Backend() = %v, want %v", got, backend1)
	}
}

func TestBackendStorage_NumKeys(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	s := NewDefaultBackendStorage()

	gotCount := s.NumKeys()
	if gotCount != 0 {
		t.Errorf("NumKeys() = %d, want 0", gotCount)
	}
	s.AddBackend([]string{"ident1"}, backend1)
	gotCount = s.NumKeys()
	if gotCount != 1 {
		t.Errorf("NumKeys() = %d, want 1", gotCount)
	}
}

func TestBackendStorage_RandomBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	s := NewDefaultBackendStorage()

	got, err := s.RandomBackend()
	if got != nil {
		t.Errorf("RandomBackend() = %v, want nil", got)
	}
	if err == nil {
		t.Errorf("RandomBackend() = error nil, want error")
	}

	s.AddBackend([]string{"ident1"}, backend1)
	got, err = s.RandomBackend()
	if got != backend1 {
		t.Errorf("RandomBackend() = %v, want %v", got, backend1)
	}
	if err != nil {
		t.Errorf("RandomBackend() = error %v, want nil", err)
	}

	s.AddBackend([]string{"ident1"}, backend2)
	got, err = s.RandomBackend()
	if got != backend1 {
		t.Errorf("RandomBackend() = %v, want %v", got, backend1)
	}
	if err != nil {
		t.Errorf("RandomBackend() = error %v, want nil", err)
	}
}
