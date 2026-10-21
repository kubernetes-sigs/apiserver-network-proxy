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
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc/metadata"
	"k8s.io/apimachinery/pkg/util/wait"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
)

const dialTestTimeout = 5 * time.Second

// dialTestAgent is a fake agent connection for exercising
// ProxyServer.DialContext. Only the test goroutine writes packets for the
// server to recvCh; packets the server sends to the agent arrive on sent.
type dialTestAgent struct {
	t       *testing.T
	server  *ProxyServer
	backend *Backend
	// cancel ends the agent connection context.
	cancel context.CancelFunc

	recvCh    chan *client.Packet
	sent      chan *client.Packet
	done      chan struct{}
	serveDone chan struct{}
	stopped   bool
}

// newDialTestAgent registers a fake agent with server. If beforeSend is set,
// it is called for every packet sent to the agent, while the backend send lock
// is held, and may block.
func newDialTestAgent(t *testing.T, server *ProxyServer, beforeSend func(*client.Packet)) *dialTestAgent {
	t.Helper()
	ctrl := gomock.NewController(t)
	agentConn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(), metadata.MD{
		"agentid": []string{uuid.New().String()},
	}))
	agentConn.EXPECT().Context().Return(ctx).AnyTimes()

	a := &dialTestAgent{
		t:         t,
		server:    server,
		cancel:    cancel,
		recvCh:    make(chan *client.Packet, 10),
		sent:      make(chan *client.Packet, 100),
		done:      make(chan struct{}),
		serveDone: make(chan struct{}),
	}
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if beforeSend != nil {
			beforeSend(pkt)
		}
		select {
		case a.sent <- pkt:
		case <-a.done:
		}
		return nil
	}).AnyTimes()

	backend, err := NewBackend(agentConn)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	a.backend = backend
	server.addBackend(backend)
	go func() {
		defer close(a.serveDone)
		server.serveRecvBackend(backend, backend.GetAgentID(), a.recvCh)
	}()
	t.Cleanup(func() {
		close(a.done)
		a.stop()
	})
	return a
}

// stop ends the agent connection, as when the agent disconnects.
func (a *dialTestAgent) stop() {
	if a.stopped {
		return
	}
	a.stopped = true
	a.cancel()
	close(a.recvCh)
	<-a.serveDone
}

func (a *dialTestAgent) expectSent(packetType client.PacketType) *client.Packet {
	a.t.Helper()
	select {
	case pkt := <-a.sent:
		if pkt.Type != packetType {
			a.t.Fatalf("expected %v sent to agent, got %v", packetType, pkt)
		}
		return pkt
	case <-time.After(dialTestTimeout):
		a.t.Fatalf("timed out waiting for %v sent to agent", packetType)
		return nil
	}
}

func (a *dialTestAgent) expectNothingSent(wait time.Duration) {
	a.t.Helper()
	select {
	case pkt := <-a.sent:
		a.t.Fatalf("expected nothing sent to agent, got %v", pkt)
	case <-time.After(wait):
	}
}

func (a *dialTestAgent) dialResponse(dialID, connID int64, dialErr string) {
	a.recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{Random: dialID, ConnectID: connID, Error: dialErr},
		},
	}
}

func (a *dialTestAgent) data(connID int64, data string) {
	a.recvCh <- &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{
			Data: &client.Data{ConnectID: connID, Data: []byte(data)},
		},
	}
}

func (a *dialTestAgent) closeResponse(connID int64) {
	a.recvCh <- &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{ConnectID: connID},
		},
	}
}

type dialResult struct {
	conn net.Conn
	err  error
}

func startDial(ctx context.Context, server *ProxyServer, address string) <-chan dialResult {
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := server.DialContext(ctx, "tcp", address)
		resultCh <- dialResult{conn, err}
	}()
	return resultCh
}

func waitDial(t *testing.T, resultCh <-chan dialResult) dialResult {
	t.Helper()
	select {
	case r := <-resultCh:
		return r
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for DialContext to return")
		return dialResult{}
	}
}

// dialEstablished dials through the agent and completes the dial with connID.
func dialEstablished(t *testing.T, a *dialTestAgent, connID int64) *serverConn {
	t.Helper()
	resultCh := startDial(context.Background(), a.server, "127.0.0.1:8080")
	dialReq := a.expectSent(client.PacketType_DIAL_REQ)
	a.dialResponse(dialReq.GetDialRequest().Random, connID, "")
	r := waitDial(t, resultCh)
	if r.err != nil {
		t.Fatalf("DialContext: %v", r.err)
	}
	return r.conn.(*serverConn)
}

func newDialTestServer() *ProxyServer {
	metrics.Metrics.Reset()
	return NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
}

func assertServerDialFailure(t *testing.T, reason metrics.DialFailureReason, count int) {
	t.Helper()
	if err := metricstest.DefaultTester.ExpectServerDialFailure(reason, count); err != nil {
		t.Error(err)
	}
}

func assertNoPendingOrEstablished(t *testing.T, server *ProxyServer) {
	t.Helper()
	if n := pendingDialCount(server); n != 0 {
		t.Errorf("expected no pending dials, got %d", n)
	}
	server.fmu.RLock()
	defer server.fmu.RUnlock()
	if n := server.getCount(server.established); n != 0 {
		t.Errorf("expected no established connections, got %d", n)
	}
}

func readWithTimeout(t *testing.T, conn net.Conn, buf []byte) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		n, err := conn.Read(buf)
		resultCh <- result{n, err}
	}()
	select {
	case r := <-resultCh:
		return r.n, r.err
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for Read")
		return 0, nil
	}
}

func TestDialContextSuccess(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	resultCh := startDial(context.Background(), server, "127.0.0.1:8080")
	dialReq := a.expectSent(client.PacketType_DIAL_REQ).GetDialRequest()
	if dialReq.Address != "127.0.0.1:8080" || dialReq.Protocol != "tcp" {
		t.Errorf("unexpected DIAL_REQ %v", dialReq)
	}
	const connID int64 = 42
	a.dialResponse(dialReq.Random, connID, "")
	r := waitDial(t, resultCh)
	if r.err != nil {
		t.Fatalf("DialContext: %v", r.err)
	}
	conn := r.conn
	assertEstablishedConnsMetric(t, 1)
	if n := pendingDialCount(server); n != 0 {
		t.Errorf("expected no pending dials, got %d", n)
	}

	if n, err := conn.Write([]byte("ping")); err != nil || n != 4 {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	data := a.expectSent(client.PacketType_DATA).GetData()
	if data.ConnectID != connID || string(data.Data) != "ping" {
		t.Errorf("unexpected DATA %v", data)
	}

	a.data(connID, "pong")
	buf := make([]byte, 10)
	n, err := readWithTimeout(t, conn, buf)
	if err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("Read: data=%q err=%v", buf[:n], err)
	}

	if got := conn.LocalAddr().Network(); got != "tcp" {
		t.Errorf("unexpected LocalAddr network %q", got)
	}
	if got := conn.RemoteAddr().String(); got != "127.0.0.1:8080" {
		t.Errorf("unexpected RemoteAddr %q", got)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := a.expectSent(client.PacketType_CLOSE_REQ).GetCloseRequest().ConnectID; got != connID {
		t.Errorf("expected CLOSE_REQ for %d, got %d", connID, got)
	}
	assertNoPendingOrEstablished(t, server)
	assertEstablishedConnsMetric(t, 0)

	// A second Close is a no-op, and the closed connection is unusable.
	if err := conn.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("expected net.ErrClosed writing a closed conn, got %v", err)
	}
	if _, err := conn.Read(buf); !errors.Is(err, net.ErrClosed) {
		t.Errorf("expected net.ErrClosed reading a closed conn, got %v", err)
	}
	a.expectNothingSent(50 * time.Millisecond)

	// The agent's CLOSE_RSP for the connection is now unrecognized and dropped.
	a.closeResponse(connID)
	a.expectNothingSent(50 * time.Millisecond)
}

func TestDialContextNoBackend(t *testing.T) {
	server := newDialTestServer()
	conn, err := server.DialContext(context.Background(), "tcp", "127.0.0.1:8080")
	if conn != nil || err == nil {
		t.Fatalf("expected error with no backend, got conn=%v err=%v", conn, err)
	}
	assertServerDialFailure(t, metrics.DialFailureNoAgent, 1)
}

func TestDialContextDialError(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	resultCh := startDial(context.Background(), server, "127.0.0.1:8080")
	dialReq := a.expectSent(client.PacketType_DIAL_REQ)
	a.dialResponse(dialReq.GetDialRequest().Random, 0, "dial tcp 127.0.0.1:8080: connect: connection refused")
	r := waitDial(t, resultCh)
	if r.conn != nil || r.err == nil || !strings.Contains(r.err.Error(), "connection refused") {
		t.Fatalf("expected connection refused error, got conn=%v err=%v", r.conn, r.err)
	}
	// The agent already gave up on the dial, so it is not told to close it.
	a.expectNothingSent(50 * time.Millisecond)
	assertNoPendingOrEstablished(t, server)
	assertServerDialFailure(t, metrics.DialFailureErrorResponse, 1)
}

func TestDialContextDialClosedByAgent(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	resultCh := startDial(context.Background(), server, "127.0.0.1:8080")
	dialReq := a.expectSent(client.PacketType_DIAL_REQ)
	a.recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_CLS,
		Payload: &client.Packet_CloseDial{
			CloseDial: &client.CloseDial{Random: dialReq.GetDialRequest().Random},
		},
	}
	r := waitDial(t, resultCh)
	if r.conn != nil || !errors.Is(r.err, errDialClosed) {
		t.Fatalf("expected errDialClosed, got conn=%v err=%v", r.conn, r.err)
	}
	assertNoPendingOrEstablished(t, server)
	// The DIAL_CLS handler observes the failure after waking the dialer.
	if err := wait.PollUntilContextTimeout(context.Background(), 10*time.Millisecond, dialTestTimeout, true, func(context.Context) (bool, error) {
		return metricstest.DefaultTester.ExpectServerDialFailure(metrics.DialFailureBackendClose, 1) == nil, nil
	}); err != nil {
		assertServerDialFailure(t, metrics.DialFailureBackendClose, 1)
	}
}

func TestDialContextTimeout(t *testing.T) {
	server := newDialTestServer()
	server.SetBackendDialTimeout(100 * time.Millisecond)
	a := newDialTestAgent(t, server, nil)

	resultCh := startDial(context.Background(), server, "127.0.0.1:8080")
	dialID := a.expectSent(client.PacketType_DIAL_REQ).GetDialRequest().Random
	r := waitDial(t, resultCh)
	if r.conn != nil || !errors.Is(r.err, errBackendDialTimeout) {
		t.Fatalf("expected errBackendDialTimeout, got conn=%v err=%v", r.conn, r.err)
	}
	// The agent may still be dialing, so it is told to stop.
	if got := a.expectSent(client.PacketType_DIAL_CLS).GetCloseDial().Random; got != dialID {
		t.Errorf("expected DIAL_CLS for %d, got %d", dialID, got)
	}
	assertNoPendingOrEstablished(t, server)
	assertServerDialFailure(t, metrics.DialFailureBackendDialTimeout, 1)
}

func TestDialContextCancelledBeforeDial(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := server.DialContext(ctx, "tcp", "127.0.0.1:8080")
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got conn=%v err=%v", conn, err)
	}
	a.expectNothingSent(50 * time.Millisecond)
	assertNoPendingOrEstablished(t, server)
}

func TestDialContextCancelledWaitingForDialResponse(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := startDial(ctx, server, "127.0.0.1:8080")
	dialID := a.expectSent(client.PacketType_DIAL_REQ).GetDialRequest().Random
	cancel()
	r := waitDial(t, resultCh)
	if r.conn != nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got conn=%v err=%v", r.conn, r.err)
	}
	if got := a.expectSent(client.PacketType_DIAL_CLS).GetCloseDial().Random; got != dialID {
		t.Errorf("expected DIAL_CLS for %d, got %d", dialID, got)
	}
	assertServerDialFailure(t, metrics.DialFailureFrontendClose, 1)

	// If the agent completes the dial anyway, the connection is closed.
	const connID int64 = 7
	a.dialResponse(dialID, connID, "")
	if got := a.expectSent(client.PacketType_CLOSE_REQ).GetCloseRequest().ConnectID; got != connID {
		t.Errorf("expected CLOSE_REQ for %d, got %d", connID, got)
	}
	assertNoPendingOrEstablished(t, server)
}

// A canceled dial must not wait for a DIAL_REQ send that is blocked, even
// without a backend dial timeout.
func TestDialContextCancelledWhileDialRequestSendBlocked(t *testing.T) {
	server := newDialTestServer()
	sendBlocked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	newDialTestAgent(t, server, func(pkt *client.Packet) {
		if pkt.Type == client.PacketType_DIAL_REQ {
			close(sendBlocked)
			<-release
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := startDial(ctx, server, "127.0.0.1:8080")
	select {
	case <-sendBlocked:
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for the DIAL_REQ send")
	}
	cancel()
	r := waitDial(t, resultCh)
	if r.conn != nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got conn=%v err=%v", r.conn, r.err)
	}
	if n := pendingDialCount(server); n != 0 {
		t.Errorf("expected no pending dials, got %d", n)
	}
	assertServerDialFailure(t, metrics.DialFailureFrontendClose, 1)
}

func TestDialContextAgentConnectionClosedWhileDialing(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	resultCh := startDial(context.Background(), server, "127.0.0.1:8080")
	a.expectSent(client.PacketType_DIAL_REQ)
	a.stop()
	r := waitDial(t, resultCh)
	if r.conn != nil || !errors.Is(r.err, errBackendClosedWhileDialing) {
		t.Fatalf("expected errBackendClosedWhileDialing, got conn=%v err=%v", r.conn, r.err)
	}
	assertNoPendingOrEstablished(t, server)
	assertServerDialFailure(t, metrics.DialFailureBackendClose, 1)
}

// The DIAL_RSP handler removes the pending dial before it hands the DIAL_RSP
// to the connection. If DialContext gives up in between, the connection must
// still be closed on the agent rather than leaked.
func TestDialResponseAfterDialerGaveUp(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)

	const dialID, connID int64 = 1234, 99
	conn := newServerConn(server, a.backend, dialID, "127.0.0.1:8080")
	// DialContext gave up and closed the connection after the DIAL_RSP
	// handler had already claimed the pending dial.
	_ = conn.Close()
	server.PendingDial.Add(dialID, &ProxyClientConnection{
		frontend:    &Frontend{stream: conn, streamUID: uuid.New().String()},
		dialID:      dialID,
		start:       time.Now(),
		backend:     a.backend,
		dialAddress: "127.0.0.1:8080",
	})

	a.dialResponse(dialID, connID, "")
	if got := a.expectSent(client.PacketType_CLOSE_REQ).GetCloseRequest().ConnectID; got != connID {
		t.Errorf("expected CLOSE_REQ for %d, got %d", connID, got)
	}
	assertNoPendingOrEstablished(t, server)
	assertEstablishedConnsMetric(t, 0)
	assertServerDialFailure(t, metrics.DialFailureSendResponse, 1)
}

// Close must return promptly while a Write is blocked sending to the agent,
// and must end that Write as well as one waiting to send.
func TestServerConnCloseDoesNotWaitForBlockedWrite(t *testing.T) {
	server := newDialTestServer()
	writeBlocked := make(chan struct{})
	release := make(chan struct{})
	a := newDialTestAgent(t, server, func(pkt *client.Packet) {
		if pkt.Type == client.PacketType_DATA && string(pkt.GetData().Data) == "stuck" {
			close(writeBlocked)
			<-release
		}
	})
	const connID int64 = 5
	conn := dialEstablished(t, a, connID)

	stuck := []byte("stuck")
	firstWrite := make(chan error, 1)
	go func() {
		_, err := conn.Write(stuck)
		firstWrite <- err
	}()
	select {
	case <-writeBlocked:
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for the blocked write")
	}
	// This write waits for the backend send lock.
	secondWrite := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("queued"))
		secondWrite <- err
	}()

	closed := make(chan struct{})
	go func() {
		_ = conn.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(dialTestTimeout):
		t.Fatal("Close blocked behind a blocked Write")
	}
	for name, writeCh := range map[string]chan error{"blocked": firstWrite, "waiting": secondWrite} {
		select {
		case err := <-writeCh:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("expected net.ErrClosed for the %s write, got %v", name, err)
			}
		case <-time.After(dialTestTimeout):
			t.Fatalf("the %s write was not ended by Close", name)
		}
	}
	// The abandoned send must not see the caller reuse its buffer.
	copy(stuck, "xxxxx")

	close(release)
	if got := string(a.expectSent(client.PacketType_DATA).GetData().Data); got != "stuck" {
		t.Errorf("expected the abandoned DATA %q, got %q", "stuck", got)
	}
	if got := a.expectSent(client.PacketType_CLOSE_REQ).GetCloseRequest().ConnectID; got != connID {
		t.Errorf("expected CLOSE_REQ for %d, got %d", connID, got)
	}
	assertNoPendingOrEstablished(t, server)
}

// A write deadline set while a Write is blocked ends that Write. Since the
// abandoned DATA may still reach the agent, later Writes fail too.
func TestServerConnWriteDeadlineEndsBlockedWrite(t *testing.T) {
	server := newDialTestServer()
	writeBlocked := make(chan struct{})
	release := make(chan struct{})
	a := newDialTestAgent(t, server, func(pkt *client.Packet) {
		if pkt.Type == client.PacketType_DATA && string(pkt.GetData().Data) == "stuck" {
			close(writeBlocked)
			<-release
		}
	})
	conn := dialEstablished(t, a, 5)
	defer conn.Close()

	writeDone := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("stuck"))
		writeDone <- err
	}()
	select {
	case <-writeBlocked:
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for the blocked write")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("expected os.ErrDeadlineExceeded, got %v", err)
		}
	case <-time.After(dialTestTimeout):
		t.Fatal("the write deadline did not end the blocked write")
	}

	close(release)
	a.expectSent(client.PacketType_DATA)
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("after timeout")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("expected os.ErrDeadlineExceeded after a timed out write, got %v", err)
	}
	a.expectNothingSent(50 * time.Millisecond)
}

// Dials abandoned while the agent stream is stalled must not each leave a
// goroutine blocked on the backend, and the agent is not told to stop dials
// it never received.
func TestDialContextTimeoutsOnStalledBackendDoNotAccumulateGoroutines(t *testing.T) {
	server := newDialTestServer()
	server.SetBackendDialTimeout(20 * time.Millisecond)
	stalled := make(chan struct{})
	release := make(chan struct{})
	var stallOnce sync.Once
	a := newDialTestAgent(t, server, func(*client.Packet) {
		stallOnce.Do(func() {
			close(stalled)
			<-release
		})
	})

	// The first DIAL_REQ stalls the agent stream.
	first := startDial(context.Background(), server, "127.0.0.1:8080")
	select {
	case <-stalled:
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for the agent stream to stall")
	}
	if r := waitDial(t, first); !errors.Is(r.err, errBackendDialTimeout) {
		t.Fatalf("expected errBackendDialTimeout, got conn=%v err=%v", r.conn, r.err)
	}

	before := runtime.NumGoroutine()
	const dials = 50
	for i := 0; i < dials; i++ {
		if r := waitDial(t, startDial(context.Background(), server, "127.0.0.1:8080")); !errors.Is(r.err, errBackendDialTimeout) {
			t.Fatalf("dial %d: expected errBackendDialTimeout, got conn=%v err=%v", i, r.conn, r.err)
		}
	}
	if err := wait.PollUntilContextTimeout(context.Background(), 10*time.Millisecond, dialTestTimeout, true, func(context.Context) (bool, error) {
		return runtime.NumGoroutine()-before < dials/2, nil
	}); err != nil {
		t.Errorf("goroutines grew from %d to %d over %d abandoned dials", before, runtime.NumGoroutine(), dials)
	}

	close(release)
	a.expectSent(client.PacketType_DIAL_REQ)
	a.expectNothingSent(100 * time.Millisecond)
	assertNoPendingOrEstablished(t, server)
	assertServerDialFailure(t, metrics.DialFailureBackendDialTimeout, dials+1)
}

// queueSend sends packets in order from a single goroutine, however many are
// queued while the agent stream is stalled.
func TestBackendQueueSend(t *testing.T) {
	server := newDialTestServer()
	stalled := make(chan struct{})
	release := make(chan struct{})
	var stallOnce sync.Once
	a := newDialTestAgent(t, server, func(*client.Packet) {
		stallOnce.Do(func() {
			close(stalled)
			<-release
		})
	})

	before := runtime.NumGoroutine()
	const packets = 50
	for i := int64(1); i <= packets; i++ {
		a.backend.queueSend(newCloseRequestPacket(i))
	}
	select {
	case <-stalled:
	case <-time.After(dialTestTimeout):
		t.Fatal("timed out waiting for the queued send")
	}
	if grown := runtime.NumGoroutine() - before; grown > 2 {
		t.Errorf("queueing %d packets started %d goroutines", packets, grown)
	}

	close(release)
	for i := int64(1); i <= packets; i++ {
		if got := a.expectSent(client.PacketType_CLOSE_REQ).GetCloseRequest().ConnectID; got != i {
			t.Fatalf("expected CLOSE_REQ for %d, got %d", i, got)
		}
	}
}

// Data from the agent is buffered up to a bound; beyond it, delivery waits
// for the reader instead of growing the buffer without limit.
func TestServerConnReceiveBufferIsBounded(t *testing.T) {
	server := newDialTestServer()
	server.SetFrontendWriteChannelSize(2)
	a := newDialTestAgent(t, server, nil)
	conn := dialEstablished(t, a, 5)
	defer conn.Close()

	dataPkt := func(s string) *client.Packet {
		return &client.Packet{
			Type:    client.PacketType_DATA,
			Payload: &client.Packet_Data{Data: &client.Data{ConnectID: 5, Data: []byte(s)}},
		}
	}
	for _, s := range []string{"a", "b"} {
		if err := conn.Send(dataPkt(s)); err != nil {
			t.Fatalf("Send(%q): %v", s, err)
		}
	}
	sendDone := make(chan error, 1)
	go func() { sendDone <- conn.Send(dataPkt("c")) }()
	select {
	case err := <-sendDone:
		t.Fatalf("expected Send to block on a full buffer, returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	buf := make([]byte, 10)
	if n, err := readWithTimeout(t, conn, buf); err != nil || string(buf[:n]) != "a" {
		t.Fatalf("Read: data=%q err=%v", buf[:n], err)
	}
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("Send after Read: %v", err)
		}
	case <-time.After(dialTestTimeout):
		t.Fatal("Send stayed blocked after Read made room")
	}

	// Close releases a Send blocked on the full buffer.
	go func() { sendDone <- conn.Send(dataPkt("d")) }()
	select {
	case err := <-sendDone:
		t.Fatalf("expected Send to block on a full buffer, returned %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	_ = conn.Close()
	select {
	case err := <-sendDone:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("expected net.ErrClosed, got %v", err)
		}
	case <-time.After(dialTestTimeout):
		t.Fatal("Send stayed blocked after Close")
	}
}

func TestServerConnWriteSplitsLargeWrites(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)
	conn := dialEstablished(t, a, 5)
	defer conn.Close()

	payload := bytes.Repeat([]byte("0123456789"), 10000) // 100,000 bytes
	if n, err := conn.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	var got []byte
	for len(got) < len(payload) {
		data := a.expectSent(client.PacketType_DATA).GetData()
		if len(data.Data) > maxConnDataPacketSize {
			t.Fatalf("DATA packet of %d bytes exceeds %d", len(data.Data), maxConnDataPacketSize)
		}
		got = append(got, data.Data...)
	}
	if !bytes.Equal(got, payload) {
		t.Error("DATA packets do not reassemble to the written payload")
	}
}

func TestServerConnClosedByAgent(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)
	const connID int64 = 5
	conn := dialEstablished(t, a, connID)

	a.data(connID, "last words")
	a.closeResponse(connID)

	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "last words" {
		t.Fatalf("ReadAll: data=%q err=%v", got, err)
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("expected net.ErrClosed writing a closed conn, got %v", err)
	}
	// The agent already closed the connection; it is not asked to again.
	_ = conn.Close()
	a.expectNothingSent(50 * time.Millisecond)
	assertNoPendingOrEstablished(t, server)
}

func TestServerConnAgentConnectionClosed(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)
	conn := dialEstablished(t, a, 5)
	defer conn.Close()

	a.stop()
	buf := make([]byte, 10)
	if _, err := readWithTimeout(t, conn, buf); err == nil {
		t.Fatal("expected Read to fail after the agent connection closed")
	}
	assertNoPendingOrEstablished(t, server)
}

func TestServerConnDeadlines(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)
	conn := dialEstablished(t, a, 5)
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	_, err := readWithTimeout(t, conn, buf)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("expected os.ErrDeadlineExceeded, got %v", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("expected a timeout net.Error, got %v", err)
	}

	// Clearing the deadline lets a pending Read complete.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	a.data(5, "hello")
	if n, err := readWithTimeout(t, conn, buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read: data=%q err=%v", buf[:n], err)
	}

	if err := conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("late")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("expected os.ErrDeadlineExceeded, got %v", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("on time")); err != nil {
		t.Errorf("Write: %v", err)
	}
	a.expectSent(client.PacketType_DATA)
}

// Dial failures the server reports itself keep their identity.
func TestDialResponseError(t *testing.T) {
	for _, want := range []error{errBackendClosedWhileDialing, errBackendDialTimeout} {
		if got := dialResponseError(want.Error()); !errors.Is(got, want) {
			t.Errorf("dialResponseError(%q) = %v, want %v", want.Error(), got, want)
		}
	}
	if got := dialResponseError("connection refused"); got.Error() != "connection refused" {
		t.Errorf("got %v, want connection refused", got)
	}
}

// A locally closed connection discards unread data.
func TestServerConnReadAfterClose(t *testing.T) {
	server := newDialTestServer()
	a := newDialTestAgent(t, server, nil)
	conn := dialEstablished(t, a, 5)
	if err := conn.Send(&client.Packet{
		Type:    client.PacketType_DATA,
		Payload: &client.Packet_Data{Data: &client.Data{ConnectID: 5, Data: []byte("unread")}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, err := conn.Read(make([]byte, 10)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("expected net.ErrClosed, got %v", err)
	}
}
