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

	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/metadata"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
)

func mockAgentConn(ctrl *gomock.Controller, agentID string, agentIdentifiers []string) *agentmock.MockAgentService_ConnectServer[client.Packet, client.Packet] {
	agentConn := agentmock.NewMockAgentService_ConnectServer[client.Packet, client.Packet](ctrl)
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
			agentConn := agentmock.NewMockAgentService_ConnectServer[client.Packet, client.Packet](ctrl)
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

func TestDefaultBackendManager_AddRemoveBackends(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend12, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend22, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))

	p := NewDefaultBackendManager()

	p.AddBackend(backend1)
	p.RemoveBackend(backend1)
	expectedBackends := make(map[string][]*Backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDefaultBackendManager()
	p.AddBackend(backend1)
	p.AddBackend(backend12)
	// Adding the same connection again should be a no-op.
	p.AddBackend(backend12)
	p.AddBackend(backend2)
	p.AddBackend(backend22)
	p.AddBackend(backend3)
	p.RemoveBackend(backend22)
	p.RemoveBackend(backend2)
	p.RemoveBackend(backend1)
	expectedBackends = map[string][]*Backend{
		"agent1": {backend12},
		"agent3": {backend3},
	}
	expectedAgentIDs = []string{"agent1", "agent3"}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestDefaultRouteBackendManager_AddRemoveBackends(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"default-route=true"}))
	backend12, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"default-route=true"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=true"}))
	backend22, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=true"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"default-route=true"}))

	p := NewDefaultRouteBackendManager()

	p.AddBackend(backend1)
	p.RemoveBackend(backend1)
	expectedBackends := make(map[string][]*Backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDefaultRouteBackendManager()
	p.AddBackend(backend1)
	p.AddBackend(backend12)
	// Adding the same connection again should be a no-op.
	p.AddBackend(backend12)
	p.AddBackend(backend2)
	p.AddBackend(backend22)
	p.AddBackend(backend3)
	p.RemoveBackend(backend22)
	p.RemoveBackend(backend2)
	p.RemoveBackend(backend1)

	expectedBackends = map[string][]*Backend{
		"agent1": {backend12},
		"agent3": {backend3},
	}
	expectedAgentIDs = []string{"agent1", "agent3"}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestDestHostBackendManager_AddRemoveBackends(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	// backend2 has no desthost relevant identifiers
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"default-route=true"}))
	// TODO: if backend3 is given conflicting identifiers with backend1, the wrong thing happens in RemoveBackend.
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewDestHostBackendManager()

	p.AddBackend(backend1)
	p.RemoveBackend(backend1)
	expectedBackends := make(map[string][]*Backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDestHostBackendManager()
	p.AddBackend(backend1)

	expectedBackends = map[string][]*Backend{
		"localhost":                 {backend1},
		"1.2.3.4":                   {backend1},
		"9878::7675:1292:9183:7562": {backend1},
		"node1.mydomain.com":        {backend1},
	}
	expectedAgentIDs = []string{
		"1.2.3.4",
		"9878::7675:1292:9183:7562",
		"localhost",
		"node1.mydomain.com",
	}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.AddBackend(backend2)
	p.AddBackend(backend3)

	expectedBackends = map[string][]*Backend{
		"localhost":                 {backend1},
		"node1.mydomain.com":        {backend1},
		"node2.mydomain.com":        {backend3},
		"1.2.3.4":                   {backend1},
		"5.6.7.8":                   {backend3},
		"9878::7675:1292:9183:7562": {backend1},
		"::":                        {backend3},
	}
	expectedAgentIDs = []string{
		"1.2.3.4",
		"9878::7675:1292:9183:7562",
		"localhost",
		"node1.mydomain.com",
		"5.6.7.8",
		"::",
		"node2.mydomain.com",
	}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.RemoveBackend(backend2)
	p.RemoveBackend(backend1)

	expectedBackends = map[string][]*Backend{
		"node2.mydomain.com": {backend3},
		"5.6.7.8":            {backend3},
		"::":                 {backend3},
	}
	expectedAgentIDs = []string{
		"node2.mydomain.com",
		"::",
		"5.6.7.8",
	}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.RemoveBackend(backend3)
	expectedBackends = map[string][]*Backend{}
	expectedAgentIDs = []string{}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestDestHostBackendManager_WithDuplicateIdents(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"host=localhost&host=node1.mydomain.com&ipv4=1.2.3.4&ipv6=9878::7675:1292:9183:7562"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=localhost&host=node2.mydomain.com&ipv4=5.6.7.8&ipv6=::"}))

	p := NewDestHostBackendManager()

	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)

	expectedBackends := map[string][]*Backend{
		"localhost":                 {backend1, backend2, backend3},
		"1.2.3.4":                   {backend1, backend2},
		"5.6.7.8":                   {backend3},
		"9878::7675:1292:9183:7562": {backend1, backend2},
		"::":                        {backend3},
		"node1.mydomain.com":        {backend1, backend2},
		"node2.mydomain.com":        {backend3},
	}
	expectedAgentIDs := []string{
		"1.2.3.4",
		"9878::7675:1292:9183:7562",
		"localhost",
		"node1.mydomain.com",
		"5.6.7.8",
		"::",
		"node2.mydomain.com",
	}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.RemoveBackend(backend1)
	p.RemoveBackend(backend3)

	expectedBackends = map[string][]*Backend{
		"localhost":                 {backend2},
		"1.2.3.4":                   {backend2},
		"9878::7675:1292:9183:7562": {backend2},
		"node1.mydomain.com":        {backend2},
	}
	expectedAgentIDs = []string{
		"1.2.3.4",
		"9878::7675:1292:9183:7562",
		"localhost",
		"node1.mydomain.com",
	}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.RemoveBackend(backend2)
	expectedBackends = map[string][]*Backend{}
	expectedAgentIDs = []string{}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestDefaultBackendManager_GetRandomBackend_DrainingFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{}))

	p := NewDefaultBackendManager()

	// Test 1: Empty backend manager returns ErrNotFound
	_, err := p.Backend(context.Background())
	if _, ok := err.(*ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	// Add backends
	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)

	// Test 2: Non-draining backends are returned
	b, err := p.Backend(context.Background())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if b.IsDraining() {
		t.Errorf("expected non-draining backend, got draining")
	}

	// Test 3: When some backends are draining, non-draining ones are prioritized
	backend1.SetDraining()

	// Call multiple times to ensure we never get the draining backend
	for i := 0; i < 20; i++ {
		b, err = p.Backend(context.Background())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if b == backend1 {
			t.Errorf("expected non-draining backend, got draining backend1")
		}
	}

	// Test 4: When all backends are draining, fallback to a draining backend
	backend2.SetDraining()
	backend3.SetDraining()

	b, err = p.Backend(context.Background())
	if err != nil {
		t.Errorf("expected fallback to draining backend, got error: %v", err)
	}
	if b == nil {
		t.Error("expected a backend, got nil")
	}
	if !b.IsDraining() {
		t.Error("expected draining backend as fallback")
	}
}

func TestDestHostBackendManager_Backend_DrainingFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backend1, _ := NewBackend(mockAgentConn(ctrl, "agent1", []string{"host=localhost"}))
	backend2, _ := NewBackend(mockAgentConn(ctrl, "agent2", []string{"host=localhost"}))
	backend3, _ := NewBackend(mockAgentConn(ctrl, "agent3", []string{"host=otherhost"}))

	p := NewDestHostBackendManager()

	// Add backends
	p.AddBackend(backend1)
	p.AddBackend(backend2)
	p.AddBackend(backend3)

	ctx := context.WithValue(context.Background(), destHostKey, "localhost")

	// Test 1: Non-draining backends are returned
	b, err := p.Backend(ctx)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if b.IsDraining() {
		t.Errorf("expected non-draining backend, got draining")
	}

	// Test 2: When some backends for destHost are draining, non-draining ones are prioritized
	backend1.SetDraining()

	b, err = p.Backend(ctx)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if b != backend2 {
		t.Errorf("expected backend2 (non-draining), got different backend")
	}

	// Test 3: When all backends for destHost are draining, fallback to a draining backend
	backend2.SetDraining()

	b, err = p.Backend(ctx)
	if err != nil {
		t.Errorf("expected fallback to draining backend, got error: %v", err)
	}
	if b == nil {
		t.Error("expected a backend, got nil")
	}
	if !b.IsDraining() {
		t.Error("expected draining backend as fallback")
	}
	// Verify we got one of the localhost backends, not otherhost
	if b != backend1 && b != backend2 {
		t.Error("expected fallback to be one of the localhost backends")
	}

	// Test 4: Different destHost still works independently
	ctx2 := context.WithValue(context.Background(), destHostKey, "otherhost")
	b, err = p.Backend(ctx2)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if b != backend3 {
		t.Errorf("expected backend3 for otherhost")
	}
	if b.IsDraining() {
		t.Error("expected non-draining backend for otherhost")
	}
}
