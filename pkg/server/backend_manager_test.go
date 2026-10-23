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
	"math/rand"
	"reflect"
	"testing"

	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/metadata"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
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

func assertAgentIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("agent IDs = %v, want %v", got, want)
	}
	gotSet := make(map[string]struct{}, len(got))
	for _, agentID := range got {
		gotSet[agentID] = struct{}{}
	}
	wantSet := make(map[string]struct{}, len(want))
	for _, agentID := range want {
		wantSet[agentID] = struct{}{}
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("agent IDs = %v, want %v", got, want)
	}
}

func markRandomBackendDraining(t *testing.T, manager BackendManager, backend *Backend) {
	t.Helper()
	backend.SetDraining()
	marker, ok := manager.(backendDrainingMarker)
	if !ok {
		t.Fatalf("manager %T does not maintain draining-state lists", manager)
	}
	marker.markBackendDraining(backend)
}

func setBackendRecvChannel(backend *Backend, capacity, packets int) chan *client.Packet {
	recvCh := make(chan *client.Packet, capacity)
	for i := 0; i < packets; i++ {
		recvCh <- &client.Packet{}
	}
	backend.recvCh = recvCh
	return recvCh
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
	if e, a := expectedAgentIDs, p.nonDrainingAgentIDs; !reflect.DeepEqual(e, a) {
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
	if e, a := expectedAgentIDs, p.nonDrainingAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestDefaultBackendManager_AgentListsFollowPrimaryBackendState(t *testing.T) {
	ctrl := gomock.NewController(t)
	primary, _ := NewBackend(mockAgentConn(ctrl, "agent1", nil))
	drainingReplacement, _ := NewBackend(mockAgentConn(ctrl, "agent1", nil))
	healthyReplacement, _ := NewBackend(mockAgentConn(ctrl, "agent1", nil))
	drainingAgent, _ := NewBackend(mockAgentConn(ctrl, "agent2", nil))
	drainingAgent.SetDraining()

	manager := NewDefaultBackendManager()
	manager.AddBackend(primary)
	manager.AddBackend(drainingReplacement)
	manager.AddBackend(healthyReplacement)
	manager.AddBackend(drainingAgent)
	assertAgentIDs(t, manager.nonDrainingAgentIDs, "agent1")
	assertAgentIDs(t, manager.drainingAgentIDs, "agent2")

	// Draining a secondary connection does not change the agent's state while
	// its non-draining primary remains selected.
	drainingReplacement.SetDraining()
	manager.markBackendDraining(drainingReplacement)
	assertAgentIDs(t, manager.nonDrainingAgentIDs, "agent1")
	assertAgentIDs(t, manager.drainingAgentIDs, "agent2")

	primary.SetDraining()
	manager.markBackendDraining(primary)
	manager.markBackendDraining(primary) // The state transition is idempotent.
	assertAgentIDs(t, manager.nonDrainingAgentIDs)
	assertAgentIDs(t, manager.drainingAgentIDs, "agent1", "agent2")

	// Removing primaries reclassifies the agent according to the promoted
	// connection rather than retaining the removed primary's state.
	manager.RemoveBackend(primary)
	assertAgentIDs(t, manager.nonDrainingAgentIDs)
	assertAgentIDs(t, manager.drainingAgentIDs, "agent1", "agent2")
	manager.RemoveBackend(drainingReplacement)
	assertAgentIDs(t, manager.nonDrainingAgentIDs, "agent1")
	assertAgentIDs(t, manager.drainingAgentIDs, "agent2")

	manager.RemoveBackend(healthyReplacement)
	manager.RemoveBackend(drainingAgent)
	assertAgentIDs(t, manager.nonDrainingAgentIDs)
	assertAgentIDs(t, manager.drainingAgentIDs)
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
	if e, a := expectedAgentIDs, p.nonDrainingAgentIDs; !reflect.DeepEqual(e, a) {
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
	if e, a := expectedAgentIDs, p.nonDrainingAgentIDs; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestDefaultBackendManager_SelectionRepairsDirectRetirement(t *testing.T) {
	ctrl := gomock.NewController(t)
	retired, _ := NewBackend(mockAgentConn(ctrl, "retired", nil))
	healthy, _ := NewBackend(mockAgentConn(ctrl, "healthy", nil))

	manager := NewDefaultBackendManager()
	manager.AddBackend(retired)
	manager.AddBackend(healthy)

	// Some lifecycle paths publish retirement immediately and remove the
	// backend from its managers shortly afterward. Selection must repair the
	// classification during that interval.
	retired.Retire()
	for i := 0; i < 20; i++ {
		got, err := manager.Backend(context.Background())
		if err != nil {
			t.Fatalf("Backend failed: %v", err)
		}
		if got != healthy {
			t.Fatalf("Backend returned retired backend %q, want %q", got.id, healthy.id)
		}
	}
	assertAgentIDs(t, manager.nonDrainingAgentIDs, "healthy")
	assertAgentIDs(t, manager.drainingAgentIDs, "retired")
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
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
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
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
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
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	assertAgentIDs(t, p.nonDrainingAgentIDs)
	assertAgentIDs(t, p.drainingAgentIDs)

	p.RemoveBackend(backend2)
	p.RemoveBackend(backend1)

	expectedBackends = map[string][]*Backend{
		"node2.mydomain.com": {backend3},
		"5.6.7.8":            {backend3},
		"::":                 {backend3},
	}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.RemoveBackend(backend3)
	expectedBackends = map[string][]*Backend{}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
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
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}
	assertAgentIDs(t, p.nonDrainingAgentIDs)
	assertAgentIDs(t, p.drainingAgentIDs)

	p.RemoveBackend(backend1)
	p.RemoveBackend(backend3)

	expectedBackends = map[string][]*Backend{
		"localhost":                 {backend2},
		"1.2.3.4":                   {backend2},
		"9878::7675:1292:9183:7562": {backend2},
		"node1.mydomain.com":        {backend2},
	}
	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
		t.Errorf("expected %v, got %v", e, a)
	}

	p.RemoveBackend(backend2)
	expectedBackends = map[string][]*Backend{}

	if e, a := expectedBackends, p.backends; !reflect.DeepEqual(e, a) {
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
	p.markBackendDraining(backend1)

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

	// Test 4: When only one backend is not draining, always use it
	backend2.SetDraining()
	p.markBackendDraining(backend2)

	for i := 0; i < 20; i++ {
		b, err = p.Backend(context.Background())
		if err != nil {
			t.Fatalf("unexpected error with one non-draining backend: %v", err)
		}
		if b != backend3 {
			t.Fatalf("expected sole non-draining backend3, got %p", b)
		}
	}

	// Test 5: When all backends are draining, fallback to a draining backend
	backend3.SetDraining()
	p.markBackendDraining(backend3)

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

func TestRandomBackendManagersPreferLowerRecvChannelOccupancyAmongNonDrainingAgents(t *testing.T) {
	tests := []struct {
		name             string
		agentIdentifiers []string
		newManager       func() BackendManager
	}{
		{
			name:       "default",
			newManager: func() BackendManager { return NewDefaultBackendManager() },
		},
		{
			name:             "default route",
			agentIdentifiers: []string{"default-route=true"},
			newManager:       func() BackendManager { return NewDefaultRouteBackendManager() },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			busy, _ := NewBackend(mockAgentConn(ctrl, "busy", test.agentIdentifiers))
			lessBusy, _ := NewBackend(mockAgentConn(ctrl, "less-busy", test.agentIdentifiers))
			draining, _ := NewBackend(mockAgentConn(ctrl, "draining", test.agentIdentifiers))
			setBackendRecvChannel(busy, 10, 9)
			setBackendRecvChannel(lessBusy, 10, 1)
			setBackendRecvChannel(draining, 10, 0)

			manager := test.newManager()
			manager.AddBackend(busy)
			manager.AddBackend(lessBusy)
			manager.AddBackend(draining)
			markRandomBackendDraining(t, manager, draining)

			for i := 0; i < 20; i++ {
				got, err := manager.Backend(context.Background())
				if err != nil {
					t.Fatalf("Backend failed: %v", err)
				}
				if got != lessBusy {
					t.Fatalf("Backend returned %p, want less occupied backend %p", got, lessBusy)
				}
			}
		})
	}
}

func TestDefaultBackendManager_GetRandomBackend_RecvChannelOccupancyRecovers(t *testing.T) {
	ctrl := gomock.NewController(t)
	first, _ := NewBackend(mockAgentConn(ctrl, "first", nil))
	second, _ := NewBackend(mockAgentConn(ctrl, "second", nil))
	setBackendRecvChannel(first, 10, 2)
	secondRecvCh := setBackendRecvChannel(second, 10, 8)

	manager := NewDefaultBackendManager()
	manager.AddBackend(first)
	manager.AddBackend(second)

	for i := 0; i < 20; i++ {
		got, err := manager.Backend(context.Background())
		if err != nil {
			t.Fatalf("Backend failed: %v", err)
		}
		if got != first {
			t.Fatalf("Backend returned %p, want initially less occupied backend %p", got, first)
		}
	}

	for i := 0; i < 7; i++ {
		<-secondRecvCh
	}
	for i := 0; i < 20; i++ {
		got, err := manager.Backend(context.Background())
		if err != nil {
			t.Fatalf("Backend failed after recovery: %v", err)
		}
		if got != second {
			t.Fatalf("Backend returned %p after recovery, want %p", got, second)
		}
	}
}

func TestDefaultBackendManager_GetRandomBackend_BreaksOccupancyTiesRandomly(t *testing.T) {
	ctrl := gomock.NewController(t)
	first, _ := NewBackend(mockAgentConn(ctrl, "first", nil))
	second, _ := NewBackend(mockAgentConn(ctrl, "second", nil))
	setBackendRecvChannel(first, 10, 0)
	setBackendRecvChannel(second, 10, 0)

	manager := NewDefaultBackendManager()
	manager.random = rand.New(rand.NewSource(1)) // #nosec G404 -- deterministic test source
	manager.AddBackend(first)
	manager.AddBackend(second)

	selected := map[*Backend]int{}
	for i := 0; i < 200; i++ {
		got, err := manager.Backend(context.Background())
		if err != nil {
			t.Fatalf("Backend failed: %v", err)
		}
		selected[got]++
	}
	for _, backend := range []*Backend{first, second} {
		if selected[backend] < 70 || selected[backend] > 130 {
			t.Fatalf("backend %q selected %d times, want a random share", backend.id, selected[backend])
		}
	}
}

func TestDefaultBackendManager_GetRandomBackend_UsesPowerOfTwoChoices(t *testing.T) {
	ctrl := gomock.NewController(t)
	leastBusy, _ := NewBackend(mockAgentConn(ctrl, "least-busy", nil))
	middle, _ := NewBackend(mockAgentConn(ctrl, "middle", nil))
	mostBusy, _ := NewBackend(mockAgentConn(ctrl, "most-busy", nil))
	setBackendRecvChannel(leastBusy, 10, 0)
	setBackendRecvChannel(middle, 10, 5)
	setBackendRecvChannel(mostBusy, 10, 10)

	manager := NewDefaultBackendManager()
	manager.random = rand.New(rand.NewSource(2)) // #nosec G404 -- deterministic test source
	manager.AddBackend(leastBusy)
	manager.AddBackend(middle)
	manager.AddBackend(mostBusy)

	selected := map[*Backend]bool{}
	for i := 0; i < 200; i++ {
		got, err := manager.Backend(context.Background())
		if err != nil {
			t.Fatalf("Backend failed: %v", err)
		}
		if got == mostBusy {
			t.Fatal("most occupied backend won a two-backend comparison")
		}
		selected[got] = true
	}
	if !selected[leastBusy] || !selected[middle] {
		t.Fatalf("selected backends = %v, want both lower-pressure choices to be reachable", selected)
	}
}

func TestDefaultBackendManager_GetRandomBackend_DrainingAgentsDoNotBiasEligibleSelection(t *testing.T) {
	ctrl := gomock.NewController(t)
	manager := NewDefaultBackendManager()
	manager.random = rand.New(rand.NewSource(3)) // #nosec G404 -- deterministic test source

	selected := map[*Backend]int{}
	for _, agentID := range []string{"eligible-a", "eligible-b", "eligible-c"} {
		backend, _ := NewBackend(mockAgentConn(ctrl, agentID, nil))
		setBackendRecvChannel(backend, 10, 0)
		manager.AddBackend(backend)
		selected[backend] = 0
	}
	for _, agentID := range []string{"draining-a", "draining-b", "draining-c"} {
		backend, _ := NewBackend(mockAgentConn(ctrl, agentID, nil))
		setBackendRecvChannel(backend, 10, 0)
		backend.SetDraining()
		manager.AddBackend(backend)
	}

	for i := 0; i < 6000; i++ {
		got, err := manager.Backend(context.Background())
		if err != nil {
			t.Fatalf("Backend failed: %v", err)
		}
		if got.IsDraining() {
			t.Fatalf("selected draining backend %q", got.id)
		}
		selected[got]++
	}
	for backend, count := range selected {
		if count < 1700 || count > 2300 {
			t.Fatalf("backend %q selected %d times, want approximately 2000", backend.id, count)
		}
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
