/*
Copyright 2024 The Kubernetes Authors.

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

package agent

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type FakeServerCounter struct {
	count int
}

func (f *FakeServerCounter) Count() int {
	return f.count
}

// blockingServer answers the Connect headers and then holds the stream open.
type blockingServer struct {
	serverID string
	connects atomic.Int32
}

func (s *blockingServer) Connect(stream agent.AgentService_ConnectServer) error {
	s.connects.Add(1)
	h := metadata.Pairs(header.ServerID, s.serverID, header.ServerCount, "1")
	if err := stream.SendHeader(h); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

func startBlockingServer(t *testing.T) (*blockingServer, *grpc.Server, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("failed to open port:", err)
	}
	srv := &blockingServer{serverID: "server-1"}
	gs := grpc.NewServer()
	gs.RegisterService(&agent.AgentService_ServiceDesc, srv)
	go gs.Serve(l)
	t.Cleanup(gs.Stop)
	return srv, gs, l.Addr().String()
}

func TestConnectOnce_ServerConnectionAttempts(t *testing.T) {
	metrics.Metrics.Reset()
	_, gs, addr := startBlockingServer(t)

	stopCh := make(chan struct{})
	defer close(stopCh)
	cs := &ClientSet{
		clients:       make(map[string]*Client),
		address:       addr,
		syncForever:   true,
		probeInterval: time.Hour,
		dialOptions:   []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		serverCounter: &FakeServerCounter{count: 1},
		stopCh:        stopCh,
	}
	defer cs.shutdown()

	if err := cs.connectOnce(); err != nil {
		t.Fatalf("first connectOnce: %v", err)
	}
	if err := cs.connectOnce(); !errors.As(err, new(*DuplicateServerError)) {
		t.Fatalf("second connectOnce: want DuplicateServerError, got %v", err)
	}
	gs.Stop()
	if err := cs.connectOnce(); err == nil || errors.As(err, new(*DuplicateServerError)) {
		t.Fatalf("connectOnce after server stop: want connection error, got %v", err)
	}

	expect := map[metrics.ServerConnectionAttemptResult]int{
		metrics.ServerConnectionAttemptConnected: 1,
		metrics.ServerConnectionAttemptError:     1,
	}
	if err := metricstest.DefaultTester.ExpectAgentServerConnectionAttempts(expect); err != nil {
		t.Error(err)
	}
}

// TestConnectOnce_ConnectedToAllServers covers whether the sync loop dials
// when the agent already holds a connection to every server it knows about.
func TestConnectOnce_ConnectedToAllServers(t *testing.T) {
	testcases := []struct {
		name              string
		syncForever       bool
		countServerLeases bool
		serverCount       int
		wantDial          bool
	}{
		{name: "sync once", syncForever: false, serverCount: 1, wantDial: false},
		{name: "sync forever, count from responses", syncForever: true, serverCount: 1, wantDial: true},
		{name: "sync forever, count from leases", syncForever: true, countServerLeases: true, serverCount: 1, wantDial: false},
		{name: "sync forever, count from leases, under-connected", syncForever: true, countServerLeases: true, serverCount: 2, wantDial: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, addr := startBlockingServer(t)
			stopCh := make(chan struct{})
			defer close(stopCh)
			cs := &ClientSet{
				clients:           make(map[string]*Client),
				address:           addr,
				syncForever:       tc.syncForever,
				countServerLeases: tc.countServerLeases,
				probeInterval:     time.Hour,
				dialOptions:       []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
				serverCounter:     &FakeServerCounter{count: tc.serverCount},
				stopCh:            stopCh,
			}
			defer cs.shutdown()

			if err := cs.connectOnce(); err != nil {
				t.Fatalf("first connectOnce: %v", err)
			}
			// One server behind the address: any further dial lands on it again.
			err := cs.connectOnce()
			dialed := srv.connects.Load() > 1
			if dialed != tc.wantDial {
				t.Fatalf("second connectOnce dialed=%v, want %v (err=%v)", dialed, tc.wantDial, err)
			}
			if tc.wantDial && !errors.As(err, new(*DuplicateServerError)) {
				t.Errorf("second connectOnce: want DuplicateServerError, got %v", err)
			}
			if !tc.wantDial && err != nil {
				t.Errorf("second connectOnce: want nil, got %v", err)
			}
		})
	}
}

func TestAggregateServerCounter(t *testing.T) {
	testCases := []struct {
		name            string
		source          string
		leaseCounter    ServerCounter
		responseCounter ServerCounter
		want            int
	}{
		{
			name:            "max: higher from response",
			source:          "max",
			leaseCounter:    &FakeServerCounter{count: 24},
			responseCounter: &FakeServerCounter{count: 42},
			want:            42,
		},
		{
			name:            "max: higher from leases",
			source:          "max",
			leaseCounter:    &FakeServerCounter{count: 6},
			responseCounter: &FakeServerCounter{count: 3},
			want:            6,
		},
		{
			name:            "max: both zero",
			source:          "max",
			leaseCounter:    &FakeServerCounter{count: 0},
			responseCounter: &FakeServerCounter{count: 0},
			want:            1, // fallback
		},
		{
			name:            "default: lease counter is nil",
			source:          "default",
			leaseCounter:    nil,
			responseCounter: &FakeServerCounter{count: 3},
			want:            3,
		},
		{
			name:            "default: lease counter is present",
			source:          "default",
			leaseCounter:    &FakeServerCounter{count: 3},
			responseCounter: &FakeServerCounter{count: 6},
			want:            3, // lease count is preferred
		},
		{
			name:            "default: lease count is zero",
			source:          "default",
			leaseCounter:    &FakeServerCounter{count: 0},
			responseCounter: &FakeServerCounter{count: 6},
			want:            1, // fallback
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			agg := NewAggregateServerCounter(tc.leaseCounter, tc.responseCounter, tc.source)
			if got := agg.Count(); got != tc.want {
				t.Errorf("agg.Count() = %v, want: %v", got, tc.want)
			}
		})
	}
}

func TestResponseBasedCounter(t *testing.T) {
	testCases := []struct {
		name          string
		responseCount int
		want          int
	}{
		{
			name:          "non-zero count",
			responseCount: 5,
			want:          5,
		},
		{
			name:          "zero count",
			responseCount: 0,
			want:          1, // fallback
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := &ClientSet{lastReceivedServerCount: tc.responseCount}
			rbc := NewResponseBasedCounter(cs)
			if got := rbc.Count(); got != tc.want {
				t.Errorf("rbc.Count() = %v, want: %v", got, tc.want)
			}
		})
	}
}
