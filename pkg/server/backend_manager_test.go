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
	"reflect"
	"testing"

	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type fakeAgentServiceConnectServer struct {
	agent.AgentService_ConnectServer
}

func TestAddRemoveBackends(t *testing.T) {
	backend1 := NewBackend(new(fakeAgentServiceConnectServer))
	backend12 := NewBackend(new(fakeAgentServiceConnectServer))
	backend2 := NewBackend(new(fakeAgentServiceConnectServer))
	backend22 := NewBackend(new(fakeAgentServiceConnectServer))
	backend3 := NewBackend(new(fakeAgentServiceConnectServer))

	p := NewDefaultBackendManager()

	p.AddBackend("agent1", header.UID, backend1)
	p.RemoveBackend("agent1", header.UID, backend1)
	expectedBackends := make(map[string][]Backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
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
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestAddRemoveBackendsWithDefaultRoute(t *testing.T) {
	backend1 := NewBackend(new(fakeAgentServiceConnectServer))
	backend12 := NewBackend(new(fakeAgentServiceConnectServer))
	backend2 := NewBackend(new(fakeAgentServiceConnectServer))
	backend22 := NewBackend(new(fakeAgentServiceConnectServer))
	backend3 := NewBackend(new(fakeAgentServiceConnectServer))

	p := NewDefaultRouteBackendManager()

	p.AddBackend("agent1", header.DefaultRoute, backend1)
	p.RemoveBackend("agent1", header.DefaultRoute, backend1)
	expectedBackends := make(map[string][]Backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
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
	// This is invalid. agent1 doesn't have conn3. This should be a no-op.
	p.RemoveBackend("agent1", header.DefaultRoute, backend3)

	expectedBackends = map[string][]Backend{
		"agent1": {backend12},
		"agent3": {backend3},
	}
	expectedDefaultRouteAgentIDs := []string{"agent1", "agent3"}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}
