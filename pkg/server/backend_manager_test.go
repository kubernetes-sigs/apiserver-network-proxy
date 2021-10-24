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

	pkgagent "sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

type fakeAgentServiceConnectServer struct {
	agent.AgentService_ConnectServer
}

func TestAddRemoveBackends(t *testing.T) {
	conn1 := new(fakeAgentServiceConnectServer)
	conn12 := new(fakeAgentServiceConnectServer)
	conn2 := new(fakeAgentServiceConnectServer)
	conn22 := new(fakeAgentServiceConnectServer)
	conn3 := new(fakeAgentServiceConnectServer)

	p := NewDefaultBackendManager()

	p.AddBackend("agent1", "agent1", pkgagent.UID, conn1)
	p.RemoveBackend("agent1", "agent1", pkgagent.UID, conn1)
	expectedBackends := make(map[string][]*backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDefaultBackendManager()
	p.AddBackend("agent1", "agent1", pkgagent.UID, conn1)
	p.AddBackend("agent1", "agent1", pkgagent.UID, conn12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", "agent1", pkgagent.UID, conn12)
	p.AddBackend("agent2", "agent2", pkgagent.UID, conn2)
	p.AddBackend("agent2", "agent2", pkgagent.UID, conn22)
	p.AddBackend("agent3", "agent3", pkgagent.UID, conn3)
	p.RemoveBackend("agent2", "agent2", pkgagent.UID, conn22)
	p.RemoveBackend("agent2", "agent2", pkgagent.UID, conn2)
	p.RemoveBackend("agent1", "agent1", pkgagent.UID, conn1)
	// This is invalid. agent1 doesn't have conn3. This should be a no-op.
	p.RemoveBackend("agent1", "agent1", pkgagent.UID, conn3)
	expectedBackends = map[string][]*backend{
		"agent1": {newBackend(conn12)},
		"agent3": {newBackend(conn3)},
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
	conn1 := new(fakeAgentServiceConnectServer)
	conn12 := new(fakeAgentServiceConnectServer)
	conn2 := new(fakeAgentServiceConnectServer)
	conn22 := new(fakeAgentServiceConnectServer)
	conn3 := new(fakeAgentServiceConnectServer)

	p := NewDefaultRouteBackendManager()

	p.AddBackend("agent1", "agent1", pkgagent.DefaultRoute, conn1)
	p.RemoveBackend("agent1", "agent1", pkgagent.DefaultRoute, conn1)
	expectedBackends := make(map[string][]*backend)
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
	p.AddBackend("agent1", "agent1", pkgagent.DefaultRoute, conn1)
	p.AddBackend("agent1", "agent1", pkgagent.DefaultRoute, conn12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", "agent1", pkgagent.DefaultRoute, conn12)
	p.AddBackend("agent2", "agent2", pkgagent.DefaultRoute, conn2)
	p.AddBackend("agent2", "agent2", pkgagent.DefaultRoute, conn22)
	p.AddBackend("agent3", "agent3", pkgagent.DefaultRoute, conn3)
	p.RemoveBackend("agent2", "agent2", pkgagent.DefaultRoute, conn22)
	p.RemoveBackend("agent2", "agent2", pkgagent.DefaultRoute, conn2)
	p.RemoveBackend("agent1", "agent1", pkgagent.DefaultRoute, conn1)
	// This is invalid. agent1 doesn't have conn3. This should be a no-op.
	p.RemoveBackend("agent1", "agent1", pkgagent.DefaultRoute, conn3)

	expectedBackends = map[string][]*backend{
		"agent1": {newBackend(conn12)},
		"agent3": {newBackend(conn3)},
	}
	expectedDefaultRouteAgentIDs := []string{"agent1", "agent3"}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestAddRemoveBackendsWithDestHost(t *testing.T) {
	conn1 := new(fakeAgentServiceConnectServer)
	conn12 := new(fakeAgentServiceConnectServer)
	conn2 := new(fakeAgentServiceConnectServer)
	conn22 := new(fakeAgentServiceConnectServer)
	conn3 := new(fakeAgentServiceConnectServer)

	p := NewDestHostBackendManager()

	p.AddBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn1)
	p.RemoveBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn1)
	expectedBackends := make(map[string][]*backend)
	expectedAgentIDs := []string{}
	expectedHostToUniqueAgentIDs := make(map[string][]string)
	expectedHostToUniqueAgentIDs["10.10.10.10"] = []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedHostToUniqueAgentIDs["10.10.10.10"], p.hostToUniqueAgentIDTracker["10.10.10.10"]; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDestHostBackendManager()
	p.AddBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn1)
	p.AddBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn12)
	p.AddBackend("agent2", "10.10.10.10", pkgagent.IPv4, conn2)
	p.AddBackend("agent2", "10.10.10.10", pkgagent.IPv4, conn22)
	p.AddBackend("agent3", "10.10.10.10", pkgagent.IPv4, conn3)
	p.RemoveBackend("agent2", "10.10.10.10", pkgagent.IPv4, conn22)
	p.RemoveBackend("agent2", "10.10.10.10", pkgagent.IPv4, conn2)
	p.RemoveBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn1)
	// This is invalid. agent1 doesn't have conn3. This should be a no-op.
	p.RemoveBackend("agent1", "10.10.10.10", pkgagent.IPv4, conn3)

	expectedBackends = map[string][]*backend{
		"agent1": {newBackend(conn12)},
		"agent3": {newBackend(conn3)},
	}
	expectedHostToUniqueAgentIDs = make(map[string][]string)
	expectedHostToUniqueAgentIDs["10.10.10.10"] = []string{"agent1", "agent3"}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedHostToUniqueAgentIDs["10.10.10.10"], p.hostToUniqueAgentIDTracker["10.10.10.10"]; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}
