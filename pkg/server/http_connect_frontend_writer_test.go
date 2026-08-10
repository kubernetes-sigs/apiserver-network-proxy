/*
Copyright 2026 The Kubernetes Authors.

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
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
)

type controlledHTTPReadWriter struct {
	started     chan struct{}
	unblock     <-chan struct{}
	writes      chan string
	startedOnce sync.Once
}

type countingCloser struct {
	closeCalls atomic.Int32
	onClose    func()
}

func (c *countingCloser) Close() error {
	c.closeCalls.Add(1)
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

func (rw *controlledHTTPReadWriter) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (rw *controlledHTTPReadWriter) Write(p []byte) (int, error) {
	rw.startedOnce.Do(func() {
		if rw.started != nil {
			close(rw.started)
		}
	})
	if rw.unblock != nil {
		<-rw.unblock
	}
	if rw.writes != nil {
		rw.writes <- string(p)
	}
	return len(p), nil
}

func newReadyHTTPFrontend(rw io.ReadWriter, backend *Backend, agentID string, connectID int64, queueSize int, closeHTTP func() error, frontendWriterDone func()) *ProxyClientConnection {
	frontend := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      rw,
		CloseHTTP: closeHTTP,
		connectID: connectID,
		agentID:   agentID,
		backend:   backend,
	}
	frontend.startFrontendWriter(queueSize, frontendWriterDone)
	frontend.markFrontendWriteReady()
	return frontend
}

func waitForTestSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForTestWrite(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got frontend write %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for frontend write %q", want)
	}
}

func waitForFullFrontendWriteChannels(t *testing.T, want float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := promtest.ToFloat64(metrics.Metrics.FullFrontendWriteChannels()); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("got full frontend write channels %v, want %v", promtest.ToFloat64(metrics.Metrics.FullFrontendWriteChannels()), want)
}

func waitForFrontendWriteQueueLength(t *testing.T, frontend *ProxyClientConnection, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(frontend.frontendWriteCh) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("got frontend write queue length %d, want %d", len(frontend.frontendWriteCh), want)
}

func dialRspPkt(dialID, connectID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{
				Random:    dialID,
				ConnectID: connectID,
			},
		},
	}
}

func TestHTTPConnectionCloseIsIdempotent(t *testing.T) {
	tests := []struct {
		name       string
		concurrent bool
	}{
		{name: "sequential"},
		{name: "concurrent", concurrent: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := &countingCloser{}
			closed := make(chan struct{})
			closeHTTP := newHTTPConnectionCloseFunc(conn, closed)

			if test.concurrent {
				start := make(chan struct{})
				errs := make(chan error, 2)
				for i := 0; i < 2; i++ {
					go func() {
						<-start
						errs <- closeHTTP()
					}()
				}
				close(start)
				for i := 0; i < 2; i++ {
					if err := <-errs; err != nil {
						t.Fatalf("CloseHTTP failed: %v", err)
					}
				}
			} else {
				if err := closeHTTP(); err != nil {
					t.Fatalf("first CloseHTTP failed: %v", err)
				}
				if err := closeHTTP(); err != nil {
					t.Fatalf("second CloseHTTP failed: %v", err)
				}
			}

			waitForTestSignal(t, closed, "HTTP connection closed signal")
			if got := conn.closeCalls.Load(); got != 1 {
				t.Fatalf("conn.Close called %d times, want 1", got)
			}
		})
	}
}

func TestHTTPConnectFrontendWriterWaitsUntilTunnelReady(t *testing.T) {
	writeStarted := make(chan struct{})
	writes := make(chan string, 1)
	frontend := &ProxyClientConnection{
		Mode: ModeHTTPConnect,
		HTTP: &controlledHTTPReadWriter{started: writeStarted, writes: writes},
	}
	frontend.startFrontendWriter(1, nil)
	defer frontend.stopFrontendWriter()

	if err := frontend.sendEstablished(dataPkt(1, []byte("data"))); err != nil {
		t.Fatalf("failed to queue DATA: %v", err)
	}

	select {
	case <-writeStarted:
		t.Fatal("frontend writer sent DATA before the HTTP tunnel was ready")
	case <-time.After(100 * time.Millisecond):
	}

	frontend.markFrontendWriteReady()
	waitForTestWrite(t, writes, "data")
}

func TestHTTPConnectFrontendWriterPreservesDataAndCloseOrder(t *testing.T) {
	const (
		agentID   = "agent"
		connectID = int64(1)
	)
	proxyServer := NewProxyServer("server", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, 3)
	events := make(chan string, 4)
	rw := &controlledHTTPReadWriter{writes: events}
	frontend := newReadyHTTPFrontend(
		rw, nil, agentID, connectID, 3,
		func() error {
			events <- "close"
			return nil
		},
		func() {
			proxyServer.removeEstablished(agentID, connectID)
			events <- "removed"
		},
	)
	proxyServer.addEstablished(agentID, connectID, frontend)

	if err := frontend.sendEstablished(dataPkt(1, []byte("first"))); err != nil {
		t.Fatalf("failed to queue first DATA: %v", err)
	}
	if err := frontend.sendEstablished(dataPkt(1, []byte("second"))); err != nil {
		t.Fatalf("failed to queue second DATA: %v", err)
	}
	if err := frontend.sendEstablished(closeRspPkt(1, "")); err != nil {
		t.Fatalf("failed to queue CLOSE_RSP: %v", err)
	}

	waitForTestSignal(t, frontend.frontendWriteDone, "frontend writer to stop after CLOSE_RSP")
	for _, want := range []string{"first", "second", "close", "removed"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("got event %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
	if got, err := proxyServer.getFrontend(agentID, connectID); err == nil || got != nil {
		t.Fatalf("frontend remains tracked after queued CLOSE_RSP completed: got %p, err %v", got, err)
	}
}

func TestHTTPConnectFrontendWriterStopRemovesQueuedClose(t *testing.T) {
	metrics.Metrics.Reset()
	const (
		agentID   = "agent"
		connectID = int64(1)
	)

	proxyServer := NewProxyServer("server", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, 1)
	frontend := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      &controlledHTTPReadWriter{},
		CloseHTTP: func() error { return nil },
		agentID:   agentID,
		connectID: connectID,
	}
	frontend.initializeFrontendWriter(1)
	proxyServer.addEstablished(agentID, connectID, frontend)
	frontend.startFrontendWriter(1, func() {
		proxyServer.removeEstablished(agentID, connectID)
	})
	assertEstablishedConnsMetric(t, 1)

	got, firstClose, err := proxyServer.beginFrontendClose(agentID, connectID)
	if err != nil {
		t.Fatalf("failed to begin frontend close: %v", err)
	}
	if got != frontend || !firstClose {
		t.Fatalf("beginFrontendClose returned frontend %p, firstClose %t; want %p, true", got, firstClose, frontend)
	}
	if err := frontend.sendEstablished(closeRspPkt(connectID, "")); err != nil {
		t.Fatalf("failed to queue CLOSE_RSP: %v", err)
	}
	waitForFrontendWriteQueueLength(t, frontend, 1)

	// Model ServeHTTP returning before the writer can drain the queued close.
	frontend.stopFrontendWriter()
	waitForTestSignal(t, frontend.frontendWriteDone, "stopped frontend writer")

	proxyServer.fmu.RLock()
	_, tracked := proxyServer.established[agentID][connectID]
	proxyServer.fmu.RUnlock()
	if tracked {
		t.Fatal("stopped frontend writer left its connection tracked")
	}
	assertEstablishedConnsMetric(t, 0)
}

func TestBackendDisconnectInterruptsHTTPFrontendWithQueuedCloseResponse(t *testing.T) {
	const (
		agentID        = "agent"
		connectID      = int64(1)
		writeQueueSize = 1
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	agentConn := mockAgentConn(ctrl, agentID, nil)
	backendCloseSent := make(chan struct{})
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		closeRequest := pkt.GetCloseRequest()
		if pkt.Type != client.PacketType_CLOSE_REQ || closeRequest == nil || closeRequest.ConnectID != connectID {
			t.Errorf("got backend packet %v, want CLOSE_REQ for connection %d", pkt, connectID)
		}
		close(backendCloseSent)
		return nil
	}).Times(1)
	backend := &Backend{id: agentID, done: make(chan struct{}), conn: agentConn}
	writeStarted := make(chan struct{})
	writeUnblock := make(chan struct{})
	frontendClosed := make(chan struct{})
	conn := &countingCloser{onClose: func() { close(writeUnblock) }}
	closeHTTP := newHTTPConnectionCloseFunc(conn, frontendClosed)
	frontend := newReadyHTTPFrontend(
		&controlledHTTPReadWriter{started: writeStarted, unblock: writeUnblock},
		backend, agentID, connectID, writeQueueSize, closeHTTP, nil,
	)

	proxyServer := NewProxyServer("server", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, writeQueueSize)
	proxyServer.addEstablished(agentID, connectID, frontend)

	recvCh := make(chan *client.Packet)
	serveDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
		close(serveDone)
	}()

	recvCh <- dataPkt(connectID, []byte("blocked"))
	waitForTestSignal(t, writeStarted, "frontend write to block")
	recvCh <- closeRspPkt(connectID, "")
	waitForFrontendWriteQueueLength(t, frontend, 1)

	proxyServer.fmu.RLock()
	tracked := proxyServer.established[agentID][connectID]
	closing := tracked != nil && tracked.closing
	proxyServer.fmu.RUnlock()
	if tracked != frontend || !closing {
		t.Fatalf("frontend with queued CLOSE_RSP is not tracked as closing: got %p, closing %t", tracked, closing)
	}
	if got, err := proxyServer.getFrontend(agentID, connectID); err == nil || got != nil {
		t.Fatalf("closing frontend remains routable: got %p, err %v", got, err)
	}

	// A duplicate close and late DATA must not occupy space behind the queued
	// CLOSE_RSP. DATA follows the old missing-frontend path and asks the agent to
	// close the already-closing connection.
	recvCh <- closeRspPkt(connectID, "")
	recvCh <- dataPkt(connectID, []byte("late"))
	waitForTestSignal(t, backendCloseSent, "late DATA to be rejected")
	if got := len(frontend.frontendWriteCh); got != 1 {
		t.Fatalf("frontend write queue length after duplicate CLOSE_RSP and late DATA is %d, want 1", got)
	}

	close(recvCh)
	backend.Retire()
	waitForTestSignal(t, frontendClosed, "frontend with queued CLOSE_RSP to close on backend disconnect")
	waitForTestSignal(t, serveDone, "backend receive loop to stop")
	waitForTestSignal(t, frontend.frontendWriteDone, "frontend writer to stop")
	if got := conn.closeCalls.Load(); got != 1 {
		t.Fatalf("conn.Close called %d times, want 1", got)
	}
}

// TestServeRecvBackendHTTPConnectWriterDelaysHOLByQueueDepth pins the bounded
// isolation provided by the per-connection queue. With a blocked write and a
// queue of depth D, D more packets for that connection can be buffered while
// unrelated traffic progresses. The next packet restores shared-stream HOL
// until the blocked writer resumes.
func TestServeRecvBackendHTTPConnectWriterDelaysHOLByQueueDepth(t *testing.T) {
	metrics.Metrics.Reset()
	const (
		agentID         = "agent"
		slowID          = int64(1)
		healthyID       = int64(2)
		unrelatedDialID = int64(3)
		unrelatedConnID = int64(4)
		writeQueueSize  = 3
	)

	backend := &Backend{id: agentID, done: make(chan struct{})}
	slowStarted := make(chan struct{})
	slowUnblock := make(chan struct{})
	slowWrites := make(chan string, writeQueueSize+2)
	healthyWrites := make(chan string, writeQueueSize+1)
	slowFrontend := newReadyHTTPFrontend(
		&controlledHTTPReadWriter{started: slowStarted, unblock: slowUnblock, writes: slowWrites},
		backend, agentID, slowID, writeQueueSize, func() error { return nil },
		nil,
	)
	healthyFrontend := newReadyHTTPFrontend(
		&controlledHTTPReadWriter{writes: healthyWrites},
		backend, agentID, healthyID, writeQueueSize, func() error { return nil },
		nil,
	)
	unrelatedConnected := make(chan struct{})
	unrelatedFrontend := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      &controlledHTTPReadWriter{},
		CloseHTTP: func() error { return nil },
		connected: unrelatedConnected,
		start:     time.Now(),
		backend:   backend,
		dialID:    unrelatedDialID,
		agentID:   agentID,
	}
	unrelatedFrontend.initializeFrontendWriter(writeQueueSize)

	proxyServer := NewProxyServer("server", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, writeQueueSize)
	proxyServer.SetFrontendWriteChannelSize(writeQueueSize)
	proxyServer.addEstablished(agentID, slowID, slowFrontend)
	proxyServer.addEstablished(agentID, healthyID, healthyFrontend)
	proxyServer.PendingDial.Add(unrelatedDialID, unrelatedFrontend)

	recvCh := make(chan *client.Packet)
	serveDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
		close(serveDone)
	}()

	recvCh <- dataPkt(slowID, []byte("slow-1"))
	waitForTestSignal(t, slowStarted, "slow frontend write to block")
	recvCh <- dialRspPkt(unrelatedDialID, unrelatedConnID)
	waitForTestSignal(t, unrelatedConnected, "unrelated connection establishment")

	// Exactly writeQueueSize packets fit behind the blocked write. Healthy
	// traffic interleaved with those packets continues to make progress.
	for i := 1; i <= writeQueueSize; i++ {
		slowPayload := fmt.Sprintf("slow-%d", i+1)
		healthyPayload := fmt.Sprintf("healthy-%d", i)
		recvCh <- dataPkt(slowID, []byte(slowPayload))
		recvCh <- dataPkt(healthyID, []byte(healthyPayload))
		waitForTestWrite(t, healthyWrites, healthyPayload)
	}
	waitForFrontendWriteQueueLength(t, slowFrontend, writeQueueSize)

	// The following slow packet exceeds that budget and restores the old
	// blocking behavior. A healthy packet behind it cannot be dispatched yet.
	overflowPayload := fmt.Sprintf("slow-%d", writeQueueSize+2)
	recvCh <- dataPkt(slowID, []byte(overflowPayload))
	healthyPacketReceived := make(chan struct{})
	go func() {
		recvCh <- dataPkt(healthyID, []byte("healthy-after-full"))
		close(healthyPacketReceived)
	}()
	waitForFullFrontendWriteChannels(t, 1)
	select {
	case <-healthyPacketReceived:
		t.Fatal("healthy packet was received while serveRecvBackend was blocked on a full slow queue")
	default:
	}
	select {
	case got := <-healthyWrites:
		t.Fatalf("unexpected healthy frontend write while slow queue was full: %q", got)
	default:
	}

	close(slowUnblock)
	waitForTestSignal(t, healthyPacketReceived, "healthy packet dispatch after slow frontend resumed")
	waitForTestWrite(t, healthyWrites, "healthy-after-full")
	waitForFullFrontendWriteChannels(t, 0)
	for i := 1; i <= writeQueueSize+2; i++ {
		want := fmt.Sprintf("slow-%d", i)
		waitForTestWrite(t, slowWrites, want)
	}

	close(recvCh)
	waitForTestSignal(t, serveDone, "backend receive loop to stop")
	waitForTestSignal(t, slowFrontend.frontendWriteDone, "slow frontend writer to stop")
	waitForTestSignal(t, healthyFrontend.frontendWriteDone, "healthy frontend writer to stop")
	waitForTestSignal(t, unrelatedFrontend.frontendWriteDone, "unrelated frontend writer to stop")
}

func TestBackendDisconnectCancelsFullHTTPConnectWriteQueue(t *testing.T) {
	metrics.Metrics.Reset()
	const (
		agentID        = "agent"
		slowID         = int64(1)
		healthyID      = int64(2)
		writeQueueSize = 1
	)

	backend := &Backend{id: agentID, done: make(chan struct{})}
	slowStarted := make(chan struct{})
	slowUnblock := make(chan struct{})
	slowClosed := make(chan struct{})
	healthyClosed := make(chan struct{})
	slowConn := &countingCloser{onClose: func() { close(slowUnblock) }}
	healthyConn := &countingCloser{}
	slowFrontend := newReadyHTTPFrontend(
		&controlledHTTPReadWriter{started: slowStarted, unblock: slowUnblock},
		backend, agentID, slowID, writeQueueSize,
		newHTTPConnectionCloseFunc(slowConn, slowClosed),
		nil,
	)
	healthyFrontend := newReadyHTTPFrontend(
		&controlledHTTPReadWriter{}, backend, agentID, healthyID, writeQueueSize,
		newHTTPConnectionCloseFunc(healthyConn, healthyClosed),
		nil,
	)

	proxyServer := NewProxyServer("server", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, writeQueueSize)
	proxyServer.addEstablished(agentID, slowID, slowFrontend)
	proxyServer.addEstablished(agentID, healthyID, healthyFrontend)

	recvCh := make(chan *client.Packet)
	serveDone := make(chan struct{})
	go func() {
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
		close(serveDone)
	}()

	recvCh <- dataPkt(slowID, []byte("slow-1"))
	waitForTestSignal(t, slowStarted, "slow frontend write to block")
	recvCh <- dataPkt(slowID, []byte("slow-2"))
	// The writer owns slow-1, slow-2 fills its queue, and dispatch blocks
	// trying to enqueue slow-3.
	recvCh <- dataPkt(slowID, []byte("slow-3"))
	waitForFullFrontendWriteChannels(t, 1)

	close(recvCh)
	backend.Retire()

	waitForTestSignal(t, slowClosed, "stalled frontend to close on backend disconnect")
	waitForTestSignal(t, healthyClosed, "healthy frontend to close on backend disconnect")
	waitForTestSignal(t, serveDone, "backend receive loop to stop")
	waitForTestSignal(t, slowFrontend.frontendWriteDone, "slow frontend writer to stop")
	waitForTestSignal(t, healthyFrontend.frontendWriteDone, "healthy frontend writer to stop")
	waitForFullFrontendWriteChannels(t, 0)
	if got := slowConn.closeCalls.Load(); got != 1 {
		t.Fatalf("slow conn.Close called %d times, want 1", got)
	}
	if got := healthyConn.closeCalls.Load(); got != 1 {
		t.Fatalf("healthy conn.Close called %d times, want 1", got)
	}
}
