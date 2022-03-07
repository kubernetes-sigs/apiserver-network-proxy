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
	"errors"
	"reflect"
	"testing"

	pkgagent "sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

type fakeAgentServiceConnectServer struct {
	agent.AgentService_ConnectServer
	name string
}

func fakeNewBackend(conn fakeAgentServiceConnectServer) *backend {
	return &backend{conn: conn}
}

func TestAddRemoveBackends(t *testing.T) {
	conn1 := new(fakeAgentServiceConnectServer)
	conn12 := new(fakeAgentServiceConnectServer)
	conn2 := new(fakeAgentServiceConnectServer)
	conn22 := new(fakeAgentServiceConnectServer)
	conn3 := new(fakeAgentServiceConnectServer)

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefault})

	p.AddBackend("agent1", pkgagent.Identifiers{}, conn1)
	p.RemoveBackend("agent1", pkgagent.UID, conn1)
	expectedBackends := make(map[string][]*backend)
	expectedAgentIDs := []string{}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p = NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefault})
	p.AddBackend("agent1", pkgagent.Identifiers{}, conn1)
	p.AddBackend("agent1", pkgagent.Identifiers{}, conn12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", pkgagent.Identifiers{}, conn12)
	p.AddBackend("agent2", pkgagent.Identifiers{}, conn2)
	p.AddBackend("agent2", pkgagent.Identifiers{}, conn22)
	p.AddBackend("agent3", pkgagent.Identifiers{}, conn3)
	p.RemoveBackend("agent2", pkgagent.UID, conn22)
	p.RemoveBackend("agent2", pkgagent.UID, conn2)
	p.RemoveBackend("agent1", pkgagent.UID, conn1)
	// This is invalid. agent1 doesn't have conn3. This should be a no-op.
	p.RemoveBackend("agent1", pkgagent.UID, conn3)
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

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefaultRoute})

	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn1)
	p.RemoveBackend("agent1", pkgagent.DefaultRoute, conn1)
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

	p = NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefaultRoute})
	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn1)
	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn12)
	// Adding the same connection again should be a no-op.
	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn12)
	p.AddBackend("agent2", pkgagent.Identifiers{DefaultRoute: true}, conn2)
	p.AddBackend("agent2", pkgagent.Identifiers{DefaultRoute: true}, conn22)
	p.AddBackend("agent3", pkgagent.Identifiers{DefaultRoute: true}, conn3)
	p.RemoveBackend("agent2", pkgagent.DefaultRoute, conn22)
	p.RemoveBackend("agent2", pkgagent.DefaultRoute, conn2)
	p.RemoveBackend("agent1", pkgagent.DefaultRoute, conn1)
	// This is invalid. agent1 doesn't have conn3. This should be a no-op.
	p.RemoveBackend("agent1", pkgagent.DefaultRoute, conn3)

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

func TestStrategiesDestHost(t *testing.T) {
	conn1 := fakeAgentServiceConnectServer{name: "conn1"}
	conn2 := fakeAgentServiceConnectServer{name: "conn2"}
	conn3 := fakeAgentServiceConnectServer{name: "conn3"}
	conn4 := fakeAgentServiceConnectServer{name: "conn4"}

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost})

	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn1)
	p.AddBackend("agent2", pkgagent.Identifiers{DefaultRoute: true}, conn2)
	p.AddBackend("agent3", pkgagent.Identifiers{DefaultRoute: true, IPv4: []string{"192.168.1.103"}}, conn3)
	p.AddBackend("agent4", pkgagent.Identifiers{IPv4: []string{"192.168.1.104"}}, conn4)
	expectedBackends := map[string][]*backend{
		"agent1":        {fakeNewBackend(conn1)},
		"agent2":        {fakeNewBackend(conn2)},
		"agent3":        {fakeNewBackend(conn3)},
		"192.168.1.103": {fakeNewBackend(conn3)},
		"agent4":        {fakeNewBackend(conn4)},
		"192.168.1.104": {fakeNewBackend(conn4)},
	}
	expectedAgentIDs := []string{"agent1", "agent2", "agent3", "agent4"}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// All agents are added, but not for defaultRoute strategy
	// as the server is not configured to with DefaultRoute strategy
	if len(p.defaultRouteAgentIDs) > 0 {
		t.Errorf("expected 0, got %v", len(p.defaultRouteAgentIDs))
	}
	ctx := context.Background()
	// Get agent for host 192.168.1.103 (backend3)
	ctx = context.WithValue(ctx, destHost, "192.168.1.103")
	res, _ := p.Backend(ctx)
	v := res.(*backend)
	if e, a := v.conn, conn3; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get agent for host 192.168.1.104 (backend4)
	ctx = context.WithValue(ctx, destHost, "192.168.1.104")
	res, _ = p.Backend(ctx)
	v = res.(*backend)
	if e, a := v.conn, conn4; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// No agent for host 192.168.1.105
	ctx = context.WithValue(ctx, destHost, "192.168.1.105")
	_, err := p.Backend(ctx)
	if e, a := err.Error(), errors.New("No backend available").Error(); e != a {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestStrategiesDestHostAndDefault(t *testing.T) {
	conn1 := fakeAgentServiceConnectServer{name: "conn1"}
	conn2 := fakeAgentServiceConnectServer{name: "conn2"}
	conn3 := fakeAgentServiceConnectServer{name: "conn3"}
	conn4 := fakeAgentServiceConnectServer{name: "conn4"}

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefault})

	p.AddBackend("agent1", pkgagent.Identifiers{}, conn1)
	p.AddBackend("agent2", pkgagent.Identifiers{}, conn2)
	p.AddBackend("agent3", pkgagent.Identifiers{DefaultRoute: true, IPv4: []string{"192.168.1.103"}}, conn3)
	p.AddBackend("agent4", pkgagent.Identifiers{IPv4: []string{"192.168.1.104"}}, conn4)
	expectedBackends := map[string][]*backend{
		"agent1":        {fakeNewBackend(conn1)},
		"agent2":        {fakeNewBackend(conn2)},
		"agent3":        {fakeNewBackend(conn3)},
		"192.168.1.103": {fakeNewBackend(conn3)},
		"agent4":        {fakeNewBackend(conn4)},
		"192.168.1.104": {fakeNewBackend(conn4)},
	}
	expectedAgentIDs := []string{"agent1", "agent2", "agent3", "agent4"}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// All agents are added, but not for defaultRoute strategy
	// as the server is not configured to with DefaultRoute strategy
	if len(p.defaultRouteAgentIDs) > 0 {
		t.Errorf("expected 0, got %v", len(p.defaultRouteAgentIDs))
	}
	ctx := context.Background()
	// Get agent for host 192.168.1.103 (backend3)
	ctx = context.WithValue(ctx, destHost, "192.168.1.103")
	res, _ := p.Backend(ctx)
	v := res.(*backend)
	if e, a := v.conn, conn3; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get agent for host 192.168.1.104 (backend4)
	ctx = context.WithValue(ctx, destHost, "192.168.1.104")
	res, _ = p.Backend(ctx)
	v = res.(*backend)
	if e, a := v.conn, conn4; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get random agent for host 192.168.1.105 with success
	ctx = context.WithValue(ctx, destHost, "192.168.1.105")
	_, err := p.Backend(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestStrategiesDefaultRoute(t *testing.T) {
	conn1 := fakeAgentServiceConnectServer{name: "conn1"}
	conn2 := fakeAgentServiceConnectServer{name: "conn2"}
	ctx := context.Background()

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDefaultRoute})
	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn1)
	p.AddBackend("agent2", pkgagent.Identifiers{DefaultRoute: true}, conn2)
	expectedBackends := map[string][]*backend{
		"agent1": {fakeNewBackend(conn1)},
		"agent2": {fakeNewBackend(conn2)},
	}
	expectedAgentIDs := []string{"agent1", "agent2"}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	expectedDefaultRouteAgentIDs := []string{"agent1", "agent2"}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get backend successfully
	res, err := p.Backend(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	v := res.(*backend)
	if e1, e2, a := conn1, conn2, v.conn; !reflect.DeepEqual(e1, a) && !reflect.DeepEqual(e2, a) {
		t.Errorf("expected %v or %v, got %v", e1, e2, a)
	}

	// Get backend successfully second time
	res, err = p.Backend(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	v = res.(*backend)
	if e1, e2, a := conn1, conn2, v.conn; !reflect.DeepEqual(e1, a) && !reflect.DeepEqual(e2, a) {
		t.Errorf("expected %v or %v, got %v", e1, e2, a)
	}
}

func TestStrategiesDestHostAndDefaultRoute(t *testing.T) {
	conn1 := fakeAgentServiceConnectServer{name: "conn1"}
	conn2 := fakeAgentServiceConnectServer{name: "conn2"}
	conn3 := fakeAgentServiceConnectServer{name: "conn3"}
	conn4 := fakeAgentServiceConnectServer{name: "conn4"}
	ctx := context.Background()

	p := NewDefaultBackendManager([]ProxyStrategy{ProxyStrategyDestHost, ProxyStrategyDefaultRoute})
	p.AddBackend("agent1", pkgagent.Identifiers{DefaultRoute: true}, conn1)
	p.AddBackend("agent2", pkgagent.Identifiers{DefaultRoute: true}, conn2)
	p.AddBackend("agent3", pkgagent.Identifiers{IPv4: []string{"192.168.1.103"}}, conn3)
	p.AddBackend("agent4", pkgagent.Identifiers{IPv4: []string{"192.168.1.104"}}, conn4)
	expectedBackends := map[string][]*backend{
		"agent1":        {fakeNewBackend(conn1)},
		"agent2":        {fakeNewBackend(conn2)},
		"agent3":        {fakeNewBackend(conn3)},
		"192.168.1.103": {fakeNewBackend(conn3)},
		"agent4":        {fakeNewBackend(conn4)},
		"192.168.1.104": {fakeNewBackend(conn4)},
	}
	expectedAgentIDs := []string{"agent1", "agent2", "agent3", "agent4"}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	if e, a := expectedAgentIDs, p.agentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	expectedDefaultRouteAgentIDs := []string{"agent1", "agent2"}
	if e, a := expectedDefaultRouteAgentIDs, p.defaultRouteAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get agent for host 192.168.1.103 (backend3)
	ctx = context.WithValue(ctx, destHost, "192.168.1.103")
	res, _ := p.Backend(ctx)
	v := res.(*backend)
	if e, a := v.conn, conn3; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get agent for host 192.168.1.104 (backend4)
	ctx = context.WithValue(ctx, destHost, "192.168.1.104")
	res, _ = p.Backend(ctx)
	v = res.(*backend)
	if e, a := v.conn, conn4; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	// Get random agent for host 172.31.11.35 with success
	ctx = context.WithValue(ctx, destHost, "172.31.11.35")
	res, err := p.Backend(ctx)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	v = res.(*backend)
	// it must be conn1 or conn2
	// conn3 and conn4 are not having the default-route identifier
	if e1, e2, a := conn1, conn2, v.conn; !reflect.DeepEqual(e1, a) && !reflect.DeepEqual(e2, a) {
		t.Errorf("expected %v or %v, got %v", e1, e2, a)
	}
}
