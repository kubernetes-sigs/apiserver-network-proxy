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
	"reflect"
	"testing"

	"github.com/golang/mock/gomock"
	"google.golang.org/grpc/metadata"

	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

func mockAgentConn(ctrl *gomock.Controller, agentID string, agentIdentifiers []string) *agentmock.MockAgentService_ConnectServer {
	agentConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	agentConnMD := metadata.MD{
		":authority":       []string{"127.0.0.1:8091"},
		"agentid":          []string{agentID},
		"agentidentifiers": agentIdentifiers,
		"content-type":     []string{"application/grpc"},
		"user-agent":       []string{"grpc-go/1.42.0"},
	}
	agentConnCtx := metadata.NewIncomingContext(context.Background(), agentConnMD)
	agentConn.EXPECT().Context().Return(agentConnCtx).AnyTimes()
	return agentConn
}

func TestNewBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc    string
		ids     []string
		idents  []string
		wantErr bool
	}{
		{
			desc:    "no agentID",
			wantErr: true,
		},
		{
			desc:    "multiple agentID",
			ids:     []string{"agent-id", "agent-id"},
			wantErr: true,
		},
		{
			desc:    "multiple identifiers",
			ids:     []string{"agent-id"},
			idents:  []string{"host=localhost", "host=localhost"},
			wantErr: true,
		},
		{
			desc:    "invalid identifiers",
			ids:     []string{"agent-id"},
			idents:  []string{";"},
			wantErr: true,
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

			_, err := NewBackend(agentConn)
			if gotErr := (err != nil); gotErr != tc.wantErr {
				t.Errorf("NewBackend got err %q; wantErr = %t", err, tc.wantErr)
			}
		})
	}
}

func TestAddRemoveBackendsWithDefaultStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend12, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend22, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))

	p := NewDefaultBackendManager()

	p.AddBackend("agent1", header.UID, backend1)
	p.RemoveBackend("agent1", header.UID, backend1)
	expectedBackends := make(map[string][]Backend)
	expectedAgentIDs := []string{}
	expectedDefaultRouteAgentIDs := []string(nil)
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDefaultBackendManager()
	p.AddBackend("agent1", header.UID, backend1)
	p.AddBackend("agent1", header.UID, backend12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", header.UID, backend12)
	p.AddBackend("agent2", header.UID, backend2)
	p.AddBackend("agent2", header.UID, backend22)
	p.AddBackend("agent3", header.UID, backend3)
	p.RemoveBackend("agent2", header.UID, backend22)
	p.RemoveBackend("agent2", header.UID, backend2)
	p.RemoveBackend("agent1", header.UID, backend1)
	// This is invalid. agent1 doesn't have backend3. This should be a no-op.
	p.RemoveBackend("agent1", header.UID, backend3)
	expectedBackends = map[string][]Backend{
		"agent1": {backend12},
		"agent3": {backend3},
	}
	expectedAgentIDs = []string{"agent1", "agent3"}
	expectedDefaultRouteAgentIDs = []string(nil)
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestAddRemoveBackendsWithDefaultRouteStrategy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"default-route"}))
	backend12, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"default-route"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route"}))
	backend22, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"default-route"}))

	p := NewDefaultRouteBackendManager()

	p.AddBackend("agent1", header.DefaultRoute, backend1)
	p.RemoveBackend("agent1", header.DefaultRoute, backend1)
	expectedBackends := make(map[string][]Backend)
	expectedAgentIDs := []string{}
	expectedDefaultRouteAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDefaultRouteBackendManager()
	p.AddBackend("agent1", header.DefaultRoute, backend1)
	p.AddBackend("agent1", header.DefaultRoute, backend12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", header.DefaultRoute, backend12)
	p.AddBackend("agent2", header.DefaultRoute, backend2)
	p.AddBackend("agent2", header.DefaultRoute, backend22)
	p.AddBackend("agent3", header.DefaultRoute, backend3)
	p.RemoveBackend("agent2", header.DefaultRoute, backend22)
	p.RemoveBackend("agent2", header.DefaultRoute, backend2)
	p.RemoveBackend("agent1", header.DefaultRoute, backend1)
	// This is invalid. agent1 doesn't have backend3. This should be a no-op.
	p.RemoveBackend("agent1", header.DefaultRoute, backend3)

	expectedBackends = map[string][]Backend{
		"agent1": {backend12},
		"agent3": {backend3},
	}
	expectedAgentIDs = []string{"agent1", "agent3"}
	expectedDefaultRouteAgentIDs = []string{"agent1", "agent3"}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}
