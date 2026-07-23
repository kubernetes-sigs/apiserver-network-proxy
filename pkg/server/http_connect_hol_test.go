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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

// holTestSafetyTimeout is a failure-safety bound for local test synchronization,
// not a production latency requirement or SLO.
const holTestSafetyTimeout = time.Second

type blockingHTTPReadWriter struct {
	writeStarted chan struct{}
	releaseWrite chan struct{}

	startOnce   sync.Once
	releaseOnce sync.Once
}

// closeUnblocksHTTPReadWriter models the relevant net.Conn close behavior for
// backend-shutdown tests: CloseHTTP terminates a blocked Write. A test-only
// release exists solely to make failure cleanup safe; it does not mark the
// frontend closed.
type closeUnblocksHTTPReadWriter struct {
	writeStarted  chan struct{}
	writeDone     chan struct{}
	closeObserved chan struct{}
	releaseWrite  chan struct{}

	mu       sync.Mutex
	closed   bool
	writeErr error

	startOnce   sync.Once
	doneOnce    sync.Once
	closeOnce   sync.Once
	releaseOnce sync.Once
}

func newCloseUnblocksHTTPReadWriter() *closeUnblocksHTTPReadWriter {
	return &closeUnblocksHTTPReadWriter{
		writeStarted:  make(chan struct{}),
		writeDone:     make(chan struct{}),
		closeObserved: make(chan struct{}),
		releaseWrite:  make(chan struct{}),
	}
}

func (w *closeUnblocksHTTPReadWriter) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (w *closeUnblocksHTTPReadWriter) Write(p []byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.releaseWrite

	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.doneOnce.Do(func() { close(w.writeDone) })
	if w.closed {
		w.writeErr = io.ErrClosedPipe
		return 0, w.writeErr
	}
	return len(p), nil
}

func (w *closeUnblocksHTTPReadWriter) close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.closeObserved)
		w.release()
	})
	return nil
}

func (w *closeUnblocksHTTPReadWriter) release() {
	w.releaseOnce.Do(func() { close(w.releaseWrite) })
}

func (w *closeUnblocksHTTPReadWriter) snapshot() (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed, w.writeErr
}

func newBlockingHTTPReadWriter() *blockingHTTPReadWriter {
	return &blockingHTTPReadWriter{
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (w *blockingHTTPReadWriter) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (w *blockingHTTPReadWriter) Write(p []byte) (int, error) {
	w.startOnce.Do(func() {
		close(w.writeStarted)
	})
	<-w.releaseWrite
	return len(p), nil
}

func (w *blockingHTTPReadWriter) release() {
	w.releaseOnce.Do(func() {
		close(w.releaseWrite)
	})
}

// released reports whether the blocking write has been released (e.g. by
// CloseHTTP overflow-closing the connection). It is a non-blocking check.
func (w *blockingHTTPReadWriter) released() bool {
	select {
	case <-w.releaseWrite:
		return true
	default:
		return false
	}
}

// recordingHTTPReadWriter intentionally buffers one write. It is suitable only
// for tests that send a single DATA packet before reading from writes.
type recordingHTTPReadWriter struct {
	writes chan []byte
}

func newRecordingHTTPReadWriter() *recordingHTTPReadWriter {
	return &recordingHTTPReadWriter{
		writes: make(chan []byte, 1),
	}
}

func (w *recordingHTTPReadWriter) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (w *recordingHTTPReadWriter) Write(p []byte) (int, error) {
	payload := append([]byte(nil), p...)
	w.writes <- payload
	return len(p), nil
}

// byteStreamHTTPReadWriter verifies the socket-level contract for one tunnel:
// writes and close are serialized, and writes preserve one continuous byte
// stream. It intentionally does not assume that protocol DATA packet boundaries
// correspond to calls to Write.
type byteStreamHTTPReadWriter struct {
	firstWriteStarted chan struct{}
	releaseFirstWrite chan struct{}
	streamUpdated     chan struct{}
	closeObserved     chan struct{}

	inFlight atomic.Int32

	mu          sync.Mutex
	stream      []byte
	closed      bool
	violations  []string
	firstOnce   sync.Once
	releaseOnce sync.Once
	closeOnce   sync.Once
}

func newByteStreamHTTPReadWriter() *byteStreamHTTPReadWriter {
	return &byteStreamHTTPReadWriter{
		firstWriteStarted: make(chan struct{}),
		releaseFirstWrite: make(chan struct{}),
		streamUpdated:     make(chan struct{}, 1),
		closeObserved:     make(chan struct{}),
	}
}

func (w *byteStreamHTTPReadWriter) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (w *byteStreamHTTPReadWriter) Write(p []byte) (int, error) {
	w.enter("Write")
	defer w.leave()

	isFirst := false
	w.firstOnce.Do(func() {
		isFirst = true
		close(w.firstWriteStarted)
	})
	if isFirst {
		<-w.releaseFirstWrite
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.recordViolationLocked("Write completed after CloseHTTP")
		return 0, io.ErrClosedPipe
	}
	// This test exercises ordering only. Short writes have a separate contract
	// and must be covered by a dedicated test.
	w.stream = append(w.stream, p...)
	select {
	case w.streamUpdated <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *byteStreamHTTPReadWriter) enter(operation string) {
	if inFlight := w.inFlight.Add(1); inFlight > 1 {
		w.recordViolation(fmt.Sprintf("%s overlapped another socket operation", operation))
	}
}

func (w *byteStreamHTTPReadWriter) leave() {
	w.inFlight.Add(-1)
}

func (w *byteStreamHTTPReadWriter) release() {
	w.releaseOnce.Do(func() { close(w.releaseFirstWrite) })
}

func (w *byteStreamHTTPReadWriter) close() error {
	w.enter("CloseHTTP")

	w.mu.Lock()
	if w.closed {
		w.recordViolationLocked("CloseHTTP called more than once")
	} else {
		w.closed = true
	}
	w.mu.Unlock()
	w.leave()
	w.closeOnce.Do(func() { close(w.closeObserved) })
	return nil
}

func (w *byteStreamHTTPReadWriter) recordViolation(violation string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordViolationLocked(violation)
}

func (w *byteStreamHTTPReadWriter) recordViolationLocked(violation string) {
	w.violations = append(w.violations, violation)
}

func (w *byteStreamHTTPReadWriter) snapshot() ([]byte, []string, bool, int32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.stream...), append([]string(nil), w.violations...), w.closed, w.inFlight.Load()
}

// observedHTTPConn adapts byteStreamHTTPReadWriter to the net.Conn returned by
// an HTTP hijack. Reads block on a pipe until Close, while writes remain fully
// controlled and observable by the test.
type observedHTTPConn struct {
	net.Conn
	peer net.Conn
	sink *byteStreamHTTPReadWriter

	closeOnce sync.Once
	closeErr  error
}

func newObservedHTTPConn() *observedHTTPConn {
	conn, peer := net.Pipe()
	return &observedHTTPConn{
		Conn: conn,
		peer: peer,
		sink: newByteStreamHTTPReadWriter(),
	}
}

func (c *observedHTTPConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return n, io.EOF
	}
	return n, err
}

func (c *observedHTTPConn) Write(p []byte) (int, error) {
	return c.sink.Write(p)
}

func (c *observedHTTPConn) Close() error {
	c.closeOnce.Do(func() {
		if err := c.sink.close(); err != nil {
			c.closeErr = err
		}
		if err := c.Conn.Close(); c.closeErr == nil {
			c.closeErr = err
		}
		// The peer is test-side plumbing. It may already be closed to simulate
		// frontend EOF, so its cleanup error is not a server-socket close error.
		_ = c.peer.Close()
	})
	return c.closeErr
}

type hijackingResponseWriter struct {
	header http.Header
	conn   net.Conn
	bufrw  *bufio.ReadWriter
}

func newHijackingResponseWriter(conn net.Conn) *hijackingResponseWriter {
	return &hijackingResponseWriter{
		header: make(http.Header),
		conn:   conn,
		bufrw:  bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)),
	}
}

func (w *hijackingResponseWriter) Header() http.Header {
	return w.header
}

func (w *hijackingResponseWriter) Write(p []byte) (int, error) {
	return w.conn.Write(p)
}

func (w *hijackingResponseWriter) WriteHeader(int) {}

func (w *hijackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.bufrw, nil
}

// backendConnWithContext keeps the mock stream behavior while allowing a test
// to cancel Backend.Context independently of the mock's metadata-bearing
// context. Cancellation unblocks a Tunnel still waiting for DIAL_RSP.
type backendConnWithContext struct {
	agent.AgentService_ConnectServer
	ctx context.Context
}

func (c *backendConnWithContext) Context() context.Context {
	return c.ctx
}

// TestSlowHTTPFrontendDoesNotDelayDialResponse targets the incident's control-
// packet head-of-line blocking in serveRecvBackend.
//
// The test:
//  1. Registers established HTTP-CONNECT connection A and pending connection B
//     on the same agent stream.
//  2. Blocks A inside its hijacked-socket Write.
//  3. Delivers B's DIAL_RSP while A remains blocked.
//  4. Requires B to establish without releasing or closing A.
//
// This proves that lack of socket progress for one frontend cannot park the
// shared backend consumer or delay an unrelated connection's establishment.
func TestSlowHTTPFrontendDoesNotDelayDialResponse(t *testing.T) {
	const (
		agentID    = "agent-1"
		connectIDA = int64(1001)
		connectIDB = int64(1002)
		dialIDB    = int64(2002)
	)

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		1,
	)
	backend := &Backend{id: agentID}

	slowHTTP := newBlockingHTTPReadWriter()
	connectionA := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      slowHTTP,
		CloseHTTP: func() error { slowHTTP.release(); return nil },
		connected: make(chan struct{}),
		connectID: connectIDA,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectIDA, connectionA)

	connectionB := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		CloseHTTP: func() error { return nil },
		connected: make(chan struct{}),
		dialID:    dialIDB,
		agentID:   agentID,
		start:     time.Now(),
		backend:   backend,
	}
	proxyServer.PendingDial.Add(dialIDB, connectionB)

	recvCh := make(chan *client.Packet, 1)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	t.Cleanup(func() {
		slowHTTP.release()
		close(recvCh)
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during test cleanup")
		}
	})

	recvCh <- dataPkt(connectIDA, []byte("response for connection A"))

	select {
	case <-slowHTTP.writeStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("connection A did not enter the blocking HTTP Write")
	}

	recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{
				Random:    dialIDB,
				ConnectID: connectIDB,
			},
		},
	}

	select {
	case <-connectionB.connected:
		// Desired behavior: B establishes while A remains blocked. There are no
		// packets after B's DIAL_RSP, so A cannot be legitimately overflow-closed
		// here; A's write must still be blocked. This deterministically proves B
		// was rescued by isolating the consumer from A's write, not by killing A.
		if slowHTTP.released() {
			t.Fatal("connection A was released/closed before B established; B must establish via isolation, not by killing A")
		}
	case <-time.After(holTestSafetyTimeout):
		// Release A only after the required progress timed out, both to clean up
		// and to prove that A's Write was the operation preventing B's progress.
		slowHTTP.release()
		select {
		case <-connectionB.connected:
			t.Fatal("connection B established only after connection A's blocked HTTP Write was released")
		case <-time.After(holTestSafetyTimeout):
			t.Fatal("connection B did not establish even after connection A was released")
		}
	}
}

// TestSlowHTTPFrontendDoesNotDelayOtherConnectionData targets DATA-path head-of-
// line blocking between established HTTP-CONNECT connections.
//
// The test:
//  1. Registers established connections A and B on the same agent stream.
//  2. Blocks A inside its hijacked-socket Write.
//  3. Delivers DATA for B while A remains blocked.
//  4. Requires B to receive its exact payload without releasing or closing A.
//
// This prevents a control-packet-only fix that prioritizes DIAL_RSP while
// leaving unrelated DATA on the same blocking shared-consumer path.
func TestSlowHTTPFrontendDoesNotDelayOtherConnectionData(t *testing.T) {
	const (
		agentID    = "agent-1"
		connectIDA = int64(1001)
		connectIDB = int64(1002)
		payloadB   = "response for connection B"
	)

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		1,
	)
	backend := &Backend{id: agentID}

	slowHTTP := newBlockingHTTPReadWriter()
	connectionA := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      slowHTTP,
		CloseHTTP: func() error { slowHTTP.release(); return nil },
		connected: make(chan struct{}),
		connectID: connectIDA,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectIDA, connectionA)

	recordingHTTP := newRecordingHTTPReadWriter()
	connectionB := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      recordingHTTP,
		CloseHTTP: func() error { return nil },
		connected: make(chan struct{}),
		connectID: connectIDB,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectIDB, connectionB)

	recvCh := make(chan *client.Packet, 1)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	t.Cleanup(func() {
		slowHTTP.release()
		close(recvCh)
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during test cleanup")
		}
	})

	recvCh <- dataPkt(connectIDA, []byte("response for connection A"))
	select {
	case <-slowHTTP.writeStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("connection A did not enter the blocking HTTP Write")
	}

	recvCh <- dataPkt(connectIDB, []byte(payloadB))

	select {
	case got := <-recordingHTTP.writes:
		if string(got) != payloadB {
			t.Fatalf("connection B received payload %q, want %q", got, payloadB)
		}
		if slowHTTP.released() {
			t.Fatal("connection A was released/closed before B received DATA; B must progress via isolation, not by killing A")
		}
	case <-time.After(holTestSafetyTimeout):
		// Release A only to prove that its blocked write was preventing B's DATA
		// from being processed and to allow deterministic cleanup.
		slowHTTP.release()
		select {
		case got := <-recordingHTTP.writes:
			if string(got) != payloadB {
				t.Fatalf("connection B received payload %q after A was released, want %q", got, payloadB)
			}
			t.Fatal("connection B received DATA only after connection A's blocked HTTP Write was released")
		case <-time.After(holTestSafetyTimeout):
			t.Fatal("connection B did not receive DATA even after connection A was released")
		}
	}
}

// TestTemporarilySlowHTTPFrontendRecovers targets the risk that isolation turns
// a temporary socket slowdown into an unnecessary connection reset.
//
// The test:
//  1. Registers established connections A and B on the same agent stream.
//  2. Blocks A's first DATA write and places one additional DATA payload behind
//     it.
//  3. Delivers DATA to B and requires B to progress while A remains blocked.
//  4. Requires A to remain established and open during the temporary slowdown.
//  5. Makes A writable and requires both accepted payloads in byte-stream order.
//  6. Delivers normal CLOSE_RSP and requires close only after A's bytes drain.
//
// This deliberately freezes the minimum nonzero recovery guarantee: one small
// pending DATA payload must not overflow-close A. It does not choose a larger
// queue depth or sustained-overflow threshold. A time-based policy must not
// preempt the prompt recovery exercised here, but the test does not decide
// whether a genuinely prolonged stall may be timed out. No queue, writer
// goroutine, or dispatcher design is required.
func TestTemporarilySlowHTTPFrontendRecovers(t *testing.T) {
	const (
		agentID    = "agent-1"
		connectIDA = int64(1001)
		connectIDB = int64(1002)
		payloadB   = "response for healthy connection B"
	)
	payloadsA := [][]byte{[]byte("first response for A"), []byte("second response for A")}
	wantStreamA := bytes.Join(payloadsA, nil)

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		2,
	)
	backend := &Backend{id: agentID}

	frontendA := newByteStreamHTTPReadWriter()
	connectionA := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      frontendA,
		CloseHTTP: frontendA.close,
		connected: make(chan struct{}),
		connectID: connectIDA,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectIDA, connectionA)

	frontendB := newRecordingHTTPReadWriter()
	connectionB := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      frontendB,
		CloseHTTP: func() error { return nil },
		connected: make(chan struct{}),
		connectID: connectIDB,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectIDB, connectionB)

	// Once A's first DATA is dequeued and blocked, the channel can hold exactly
	// one waiting DATA for A and the healthy DATA for B. Reaching B therefore
	// proves A's second payload was accepted by the frontend delivery path.
	recvCh := make(chan *client.Packet, 2)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	var closeRecvOnce sync.Once
	closeRecv := func() { closeRecvOnce.Do(func() { close(recvCh) }) }
	t.Cleanup(func() {
		frontendA.release()
		closeRecv()
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during cleanup")
		}
	})

	recvCh <- dataPkt(connectIDA, payloadsA[0])
	select {
	case <-frontendA.firstWriteStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("connection A did not enter the blocking HTTP Write")
	}

	recvCh <- dataPkt(connectIDA, payloadsA[1])
	recvCh <- dataPkt(connectIDB, []byte(payloadB))

	select {
	case got := <-frontendB.writes:
		if string(got) != payloadB {
			t.Fatalf("connection B received payload %q, want %q", got, payloadB)
		}
	case <-time.After(holTestSafetyTimeout):
		// Release A only after B's required progress timed out. If B then
		// progresses, the failure is specifically the shared-consumer HOL stall.
		frontendA.release()
		select {
		case got := <-frontendB.writes:
			if string(got) != payloadB {
				t.Fatalf("connection B received payload %q after A recovered, want %q", got, payloadB)
			}
			t.Fatal("connection B received DATA only after connection A recovered")
		case <-time.After(holTestSafetyTimeout):
			t.Fatal("connection B did not receive DATA even after connection A recovered")
		}
	}

	if _, err := proxyServer.getFrontend(agentID, connectIDA); err != nil {
		t.Fatalf("connection A was removed while temporarily slow: %v", err)
	}
	_, _, closedBeforeRecovery, _ := frontendA.snapshot()
	if closedBeforeRecovery {
		t.Fatal("connection A was closed instead of isolated while temporarily slow")
	}

	frontendA.release()
	deadline := time.NewTimer(holTestSafetyTimeout)
	defer deadline.Stop()
	for {
		gotStream, _, _, _ := frontendA.snapshot()
		if len(gotStream) >= len(wantStreamA) {
			break
		}
		select {
		case <-frontendA.streamUpdated:
		case <-deadline.C:
			t.Fatalf("connection A received %d bytes after recovery, want %d", len(gotStream), len(wantStreamA))
		}
	}

	if _, err := proxyServer.getFrontend(agentID, connectIDA); err != nil {
		t.Fatalf("connection A was removed during recovery: %v", err)
	}
	gotStreamA, _, closedAfterRecovery, _ := frontendA.snapshot()
	if closedAfterRecovery {
		t.Fatal("connection A was closed after recovering socket progress")
	}
	if !bytes.Equal(gotStreamA, wantStreamA) {
		t.Fatalf("connection A byte stream after recovery = %q, want %q", gotStreamA, wantStreamA)
	}

	// Normal backend close is the terminal barrier: it must occur only after A's
	// accepted bytes have drained and must remove A from established state.
	recvCh <- &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{ConnectID: connectIDA},
		},
	}
	select {
	case <-frontendA.closeObserved:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("connection A did not close normally after recovery")
	}
	gotStreamA, violations, closed, inFlight := frontendA.snapshot()
	if len(violations) > 0 {
		t.Fatalf("connection A socket operations were not serialized through close: %v", violations)
	}
	if !closed {
		t.Fatal("connection A did not reach a terminal closed state")
	}
	if inFlight != 0 {
		t.Fatalf("connection A closed with %d socket operations still in flight", inFlight)
	}
	if !bytes.Equal(gotStreamA, wantStreamA) {
		t.Fatalf("connection A final byte stream = %q, want %q", gotStreamA, wantStreamA)
	}
	if _, err := proxyServer.getFrontend(agentID, connectIDA); err == nil {
		t.Fatal("connection A remained established after normal close")
	}

	closeRecv()
	select {
	case <-consumerDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("serveRecvBackend did not exit after recovery test")
	}
}

// TestBackendShutdownUnblocksSlowHTTPFrontend targets cleanup after the agent
// stream ends while an HTTP-CONNECT frontend socket Write is blocked.
// ProxyServer.Connect closes recvCh when the agent stream terminates.
//
// The test:
//  1. Registers established HTTP-CONNECT connection A.
//  2. Blocks A inside its frontend socket Write.
//  3. Closes recvCh, representing agent-stream termination.
//  4. Does not release the write on the success path.
//  5. Requires production cleanup to call CloseHTTP, unblock the pending Write
//     with io.ErrClosedPipe, remove A from established state, and exit
//     serveRecvBackend.
//
// The required outcome is independent of any writer goroutine, queue, or
// cancellation implementation.
func TestBackendShutdownUnblocksSlowHTTPFrontend(t *testing.T) {
	const (
		agentID   = "agent-1"
		connectID = int64(1001)
	)

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		1,
	)
	backend := &Backend{id: agentID}
	frontend := newCloseUnblocksHTTPReadWriter()
	connection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      frontend,
		CloseHTTP: frontend.close,
		connected: make(chan struct{}),
		connectID: connectID,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectID, connection)

	recvCh := make(chan *client.Packet)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	var closeRecvOnce sync.Once
	closeRecv := func() { closeRecvOnce.Do(func() { close(recvCh) }) }
	t.Cleanup(func() {
		// A release here is failure cleanup only. The success path must be
		// released by production calling CloseHTTP after backend shutdown.
		frontend.release()
		closeRecv()
		select {
		case <-frontend.writeDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("blocked frontend Write did not exit during cleanup")
		}
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during cleanup")
		}
	})

	recvCh <- dataPkt(connectID, []byte("response blocked on frontend socket"))
	select {
	case <-frontend.writeStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("frontend DATA did not enter the blocking HTTP Write")
	}

	// Closing recvCh models readBackendToChannel ending when the agent stream is
	// lost. No test-side release follows: backend cleanup must close the frontend
	// and thereby unblock its Write.
	closeRecv()
	select {
	case <-frontend.closeObserved:
	case <-time.After(holTestSafetyTimeout):
		frontend.release()
		select {
		case <-frontend.closeObserved:
			t.Fatal("backend shutdown closed the frontend only after its blocked Write was released by the test")
		case <-time.After(holTestSafetyTimeout):
			t.Fatal("backend shutdown did not close the blocked HTTP frontend")
		}
	}

	select {
	case <-frontend.writeDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("closing the frontend did not unblock its pending Write")
	}
	select {
	case <-consumerDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("serveRecvBackend did not exit after backend shutdown")
	}

	closed, writeErr := frontend.snapshot()
	if !closed {
		t.Fatal("frontend did not reach a terminal closed state")
	}
	if !errors.Is(writeErr, io.ErrClosedPipe) {
		t.Fatalf("blocked frontend Write error = %v, want %v", writeErr, io.ErrClosedPipe)
	}
	if _, err := proxyServer.getFrontend(agentID, connectID); err == nil {
		t.Fatal("connection remained established after backend shutdown")
	}
}

// TestPerConnectionDataOrdering targets reordering, omission, duplication, and
// close overtaking within one isolated HTTP-CONNECT connection.
//
// The test:
//  1. Registers one established connection and blocks its first socket Write.
//  2. Delivers three more DATA packets followed by CLOSE_RSP.
//  3. Releases the first Write.
//  4. Requires the concatenated DATA byte stream exactly once and in source
//     order, followed by terminal close with no socket operation in flight.
//
// DATA packet boundaries need not map one-to-one to socket Write calls; the
// observable contract is the byte stream and close ordering.
func TestPerConnectionDataOrdering(t *testing.T) {
	const (
		agentID   = "agent-1"
		connectID = int64(1001)
	)
	payloads := [][]byte{[]byte("first"), []byte("second"), []byte("third"), []byte("fourth")}
	wantStream := bytes.Join(payloads, nil)

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		len(payloads),
	)
	backend := &Backend{id: agentID}

	frontendHTTP := newByteStreamHTTPReadWriter()
	connection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      frontendHTTP,
		CloseHTTP: frontendHTTP.close,
		connected: make(chan struct{}),
		connectID: connectID,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectID, connection)

	// Once the first DATA is dequeued, the channel has exactly enough capacity
	// for the remaining DATA packets and the terminal CLOSE_RSP. Test-side sends
	// therefore cannot become the source of the stall.
	recvCh := make(chan *client.Packet, len(payloads))
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	t.Cleanup(func() {
		frontendHTTP.release()
		close(recvCh)
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during test cleanup")
		}
	})

	recvCh <- dataPkt(connectID, payloads[0])
	select {
	case <-frontendHTTP.firstWriteStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("first DATA did not enter the blocking HTTP Write")
	}

	for _, payload := range payloads[1:] {
		recvCh <- dataPkt(connectID, payload)
	}
	recvCh <- &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{ConnectID: connectID},
		},
	}

	frontendHTTP.release()
	select {
	case <-frontendHTTP.closeObserved:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("timed out waiting for frontend close after ordered DATA")
	}

	gotStream, violations, closed, inFlight := frontendHTTP.snapshot()
	if len(violations) > 0 {
		t.Fatalf("frontend socket operations were not serialized: %v", violations)
	}
	if inFlight != 0 {
		t.Fatalf("frontend close was observed with %d socket operations still in flight", inFlight)
	}
	if !closed {
		t.Fatal("frontend did not observe the terminal close")
	}
	if !bytes.Equal(gotStream, wantStream) {
		t.Fatalf("frontend byte stream = %q, want %q", gotStream, wantStream)
	}
}

// TestHTTPConnectResponsePrecedesTunnelData targets ordering at the boundary
// between Tunnel establishment and backend DATA delivery.
//
// The test:
//  1. Starts a real Tunnel.ServeHTTP path with a hijacked test connection.
//  2. Completes DIAL_REQ/DIAL_RSP and blocks the successful HTTP 200 Write.
//  3. Delivers backend DATA, then uses DRAIN as a deterministic shared-consumer
//     progress marker while the HTTP response remains blocked.
//  4. Releases the HTTP response and completes CLOSE_RSP/CLOSE_REQ shutdown.
//  5. Requires the client-visible bytes to be the complete HTTP 200 response
//     followed by the exact tunneled DATA.
//
// Only wire order is asserted; no dispatch or buffering design is required.
func TestHTTPConnectResponsePrecedesTunnelData(t *testing.T) {
	const (
		agentID   = "agent-1"
		connectID = int64(1001)
		target    = "istiod-stable.istio-system.svc:443"
	)
	connectResponse := []byte("HTTP/1.1 200 Connection Established\r\n\r\n")
	payload := []byte("tunneled response bytes")
	wantStream := append(append([]byte(nil), connectResponse...), payload...)

	ctrl := gomock.NewController(t)
	backendConn := mockAgentConn(ctrl, agentID, []string{})
	dialRequests := make(chan *client.Packet, 1)
	closeRequests := make(chan *client.Packet, 1)
	backendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		switch pkt.Type {
		case client.PacketType_DIAL_REQ:
			select {
			case dialRequests <- pkt:
			default:
				t.Errorf("received duplicate backend DIAL_REQ")
			}
		case client.PacketType_CLOSE_REQ:
			select {
			case closeRequests <- pkt:
			default:
				t.Errorf("received duplicate backend CLOSE_REQ")
			}
		default:
			t.Errorf("backend Send packet type = %v, want DIAL_REQ or CLOSE_REQ", pkt.Type)
		}
		return nil
	}).AnyTimes()

	backend, err := NewBackend(backendConn)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	backendCtx, cancelBackend := context.WithCancel(backend.Context())
	backend.conn = &backendConnWithContext{
		AgentService_ConnectServer: backendConn,
		ctx:                        backendCtx,
	}
	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		1,
	)
	proxyServer.addBackend(backend)

	recvCh := make(chan *client.Packet)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	frontendConn := newObservedHTTPConn()
	responseWriter := newHijackingResponseWriter(frontendConn)
	request := httptest.NewRequest(http.MethodConnect, "http://example.invalid", nil)
	request.Host = target
	tunnelDone := make(chan struct{})
	go func() {
		defer close(tunnelDone)
		(&Tunnel{Server: proxyServer}).ServeHTTP(responseWriter, request)
	}()

	var (
		closeRecvOnce sync.Once
		feederDone    chan struct{}
		dialID        int64
		dialCaptured  bool
	)
	closeRecv := func() { closeRecvOnce.Do(func() { close(recvCh) }) }
	t.Cleanup(func() {
		// Teardown order is load-bearing:
		//  1. Release the socket write and stop the feeder before closing recvCh,
		//     otherwise the feeder could panic by sending to a closed channel.
		//  2. Close the frontend and cancel the backend context so Tunnel exits
		//     whether it is blocked in the established read loop or still waiting
		//     for DIAL_RSP.
		//  3. Only then close recvCh and wait for the consumer and Tunnel.
		frontendConn.sink.release()
		safeToCloseRecv := true
		if feederDone != nil {
			select {
			case <-feederDone:
			case <-time.After(holTestSafetyTimeout):
				t.Errorf("backend packet feeder did not exit during cleanup")
				safeToCloseRecv = false
			}
		}
		if dialCaptured {
			if pending := proxyServer.PendingDial.Remove(dialID); pending != nil {
				_ = pending.CloseHTTP()
			}
		}
		_ = frontendConn.Close()
		cancelBackend()
		if safeToCloseRecv {
			closeRecv()
			select {
			case <-consumerDone:
			case <-time.After(holTestSafetyTimeout):
				t.Errorf("serveRecvBackend did not exit during cleanup")
			}
		}
		select {
		case <-tunnelDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("HTTP tunnel did not exit during cleanup")
		}
	})

	var dialRequest *client.Packet
	select {
	case dialRequest = <-dialRequests:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("HTTP tunnel did not send DIAL_REQ to the backend")
	}
	dial := dialRequest.GetDialRequest()
	if dial == nil {
		t.Fatal("backend DIAL_REQ did not contain a dial request payload")
	}
	dialID = dial.Random
	dialCaptured = true
	if dial.Address != target {
		t.Fatalf("backend DIAL_REQ = %v, want address %q", dial, target)
	}

	recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{Random: dialID, ConnectID: connectID},
		},
	}
	select {
	case <-frontendConn.sink.firstWriteStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("HTTP tunnel did not begin writing the successful CONNECT response")
	}

	// DRAIN is a test-side backend-consumer progress marker. Handing it to the
	// consumer proves the preceding DATA left the shared consumer and was accepted
	// by the frontend delivery path while the CONNECT response remained blocked.
	// This makes the ordering check deterministic without sleeps.
	feederDone = make(chan struct{})
	go func() {
		defer close(feederDone)
		recvCh <- dataPkt(connectID, payload)
		recvCh <- &client.Packet{Type: client.PacketType_DRAIN}
	}()
	select {
	case <-feederDone:
	case <-time.After(holTestSafetyTimeout):
		frontendConn.sink.release()
		t.Fatal("backend consumer did not accept DATA while the CONNECT response write was blocked")
	}

	frontendConn.sink.release()
	deadline := time.NewTimer(holTestSafetyTimeout)
	defer deadline.Stop()
	for {
		gotStream, _, _, _ := frontendConn.sink.snapshot()
		if len(gotStream) >= len(wantStream) {
			break
		}
		select {
		case <-frontendConn.sink.streamUpdated:
		case <-deadline.C:
			t.Fatalf("frontend received %d bytes, want %d before close", len(gotStream), len(wantStream))
		}
	}

	recvCh <- &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{ConnectID: connectID},
		},
	}
	select {
	case <-frontendConn.sink.closeObserved:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("frontend connection did not close after CLOSE_RSP")
	}
	closeRecv()
	select {
	case <-consumerDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("serveRecvBackend did not exit")
	}
	select {
	case <-tunnelDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("HTTP tunnel did not exit after frontend close")
	}
	select {
	case closeRequest := <-closeRequests:
		closePayload := closeRequest.GetCloseRequest()
		if closePayload == nil {
			t.Fatal("backend CLOSE_REQ did not contain a close request payload")
		}
		if closePayload.ConnectID != connectID {
			t.Fatalf("backend CLOSE_REQ connectID = %d, want %d", closePayload.ConnectID, connectID)
		}
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("HTTP tunnel did not send CLOSE_REQ after frontend close")
	}

	gotStream, _, _, _ := frontendConn.sink.snapshot()
	if !bytes.Equal(gotStream, wantStream) {
		t.Fatalf("frontend byte stream = %q, want CONNECT response followed by DATA %q", gotStream, wantStream)
	}
}

// TestConcurrentHTTPFrontendAndBackendClose targets overlapping frontend EOF
// and backend CLOSE_RSP for the same established HTTP-CONNECT tunnel.
//
// The test:
//  1. Establishes a real Tunnel and verifies the successful HTTP 200 response.
//  2. Closes the frontend peer so Tunnel initiates backend CLOSE_REQ.
//  3. Pauses that CLOSE_REQ in flight and delivers backend CLOSE_RSP.
//  4. Requires CLOSE_RSP to close the frontend, then releases CLOSE_REQ.
//  5. Requires exactly one correctly addressed CLOSE_REQ, both serving paths to
//     exit, no socket operation in flight, and no established or pending state.
//
// Close ownership is not assigned to any goroutine, queue, or state-machine
// implementation; only protocol-visible shutdown and final state are asserted.
func TestConcurrentHTTPFrontendAndBackendClose(t *testing.T) {
	const (
		agentID   = "agent-1"
		connectID = int64(1001)
		target    = "istiod-stable.istio-system.svc:443"
	)
	connectResponse := []byte("HTTP/1.1 200 Connection Established\r\n\r\n")

	ctrl := gomock.NewController(t)
	backendConn := mockAgentConn(ctrl, agentID, []string{})
	dialRequests := make(chan *client.Packet, 1)
	closeRequestStarted := make(chan struct{})
	releaseCloseRequest := make(chan struct{})
	var (
		closeRequestCount atomic.Int32
		closeStartedOnce  sync.Once
		closeReleaseOnce  sync.Once
	)
	releaseBackendClose := func() {
		closeReleaseOnce.Do(func() { close(releaseCloseRequest) })
	}
	backendConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		switch pkt.Type {
		case client.PacketType_DIAL_REQ:
			select {
			case dialRequests <- pkt:
			default:
				t.Errorf("received duplicate backend DIAL_REQ")
			}
		case client.PacketType_CLOSE_REQ:
			if count := closeRequestCount.Add(1); count != 1 {
				t.Errorf("backend received %d CLOSE_REQ packets, want exactly one", count)
			}
			closePayload := pkt.GetCloseRequest()
			if closePayload == nil {
				t.Errorf("backend CLOSE_REQ did not contain a close request payload")
			} else if closePayload.ConnectID != connectID {
				t.Errorf("backend CLOSE_REQ connectID = %d, want %d", closePayload.ConnectID, connectID)
			}
			closeStartedOnce.Do(func() { close(closeRequestStarted) })
			<-releaseCloseRequest
		default:
			t.Errorf("backend Send packet type = %v, want DIAL_REQ or CLOSE_REQ", pkt.Type)
		}
		return nil
	}).AnyTimes()

	backend, err := NewBackend(backendConn)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	backendCtx, cancelBackend := context.WithCancel(backend.Context())
	backend.conn = &backendConnWithContext{
		AgentService_ConnectServer: backendConn,
		ctx:                        backendCtx,
	}
	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		1,
	)
	proxyServer.addBackend(backend)

	recvCh := make(chan *client.Packet)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	frontendConn := newObservedHTTPConn()
	// This test does not exercise a slow socket. Let the CONNECT response write
	// complete normally before initiating either close path.
	frontendConn.sink.release()
	responseWriter := newHijackingResponseWriter(frontendConn)
	request := httptest.NewRequest(http.MethodConnect, "http://example.invalid", nil)
	request.Host = target
	tunnelDone := make(chan struct{})
	go func() {
		defer close(tunnelDone)
		(&Tunnel{Server: proxyServer}).ServeHTTP(responseWriter, request)
	}()

	var (
		closeRecvOnce sync.Once
		dialID        int64
		dialCaptured  bool
	)
	closeRecv := func() { closeRecvOnce.Do(func() { close(recvCh) }) }
	t.Cleanup(func() {
		releaseBackendClose()
		frontendConn.sink.release()
		if dialCaptured {
			if pending := proxyServer.PendingDial.Remove(dialID); pending != nil {
				_ = pending.CloseHTTP()
			}
		}
		_ = frontendConn.peer.Close()
		_ = frontendConn.Close()
		cancelBackend()
		closeRecv()
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during cleanup")
		}
		select {
		case <-tunnelDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("HTTP tunnel did not exit during cleanup")
		}
	})

	var dialRequest *client.Packet
	select {
	case dialRequest = <-dialRequests:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("HTTP tunnel did not send DIAL_REQ to the backend")
	}
	dial := dialRequest.GetDialRequest()
	if dial == nil {
		t.Fatal("backend DIAL_REQ did not contain a dial request payload")
	}
	dialID = dial.Random
	dialCaptured = true
	if dial.Address != target {
		t.Fatalf("backend DIAL_REQ = %v, want address %q", dial, target)
	}

	recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{Random: dialID, ConnectID: connectID},
		},
	}
	select {
	case <-frontendConn.sink.streamUpdated:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("frontend did not receive the successful CONNECT response")
	}
	gotResponse, _, _, _ := frontendConn.sink.snapshot()
	if !bytes.Equal(gotResponse, connectResponse) {
		t.Fatalf("frontend byte stream before close = %q, want %q", gotResponse, connectResponse)
	}

	// Closing the peer supplies frontend EOF. Tunnel begins CLOSE_REQ and the
	// mock pauses it in flight, creating a deterministic overlap window for the
	// backend CLOSE_RSP path.
	if err := frontendConn.peer.Close(); err != nil {
		t.Fatalf("closing frontend peer: %v", err)
	}
	select {
	case <-closeRequestStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("frontend EOF did not initiate backend CLOSE_REQ")
	}

	recvCh <- &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{ConnectID: connectID},
		},
	}
	select {
	case <-frontendConn.sink.closeObserved:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("backend CLOSE_RSP did not close the frontend socket")
	}

	releaseBackendClose()
	select {
	case <-tunnelDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("HTTP tunnel did not exit after concurrent frontend/backend close")
	}
	closeRecv()
	select {
	case <-consumerDone:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("serveRecvBackend did not exit after concurrent close")
	}

	if got := closeRequestCount.Load(); got != 1 {
		t.Fatalf("backend CLOSE_REQ count = %d, want 1", got)
	}
	_, _, closed, inFlight := frontendConn.sink.snapshot()
	if !closed {
		t.Fatal("frontend socket did not reach a terminal closed state")
	}
	if inFlight != 0 {
		t.Fatalf("frontend closed with %d socket operations still in flight", inFlight)
	}
	if _, err := proxyServer.getFrontend(agentID, connectID); err == nil {
		t.Fatal("connection remained or was recreated in established state after concurrent close")
	}
	proxyServer.PendingDial.mu.RLock()
	_, stillPending := proxyServer.PendingDial.pendingDial[dialID]
	proxyServer.PendingDial.mu.RUnlock()
	if stillPending {
		t.Fatal("connection reappeared in pending state after concurrent close")
	}
}

// TestManySlowHTTPFrontendsDoNotDelayDialResponse targets a fix that isolates
// only one slow frontend but still stalls behind several slow connections.
//
// For slow-frontend counts 2 and 10, the test:
//  1. Registers each slow connection on one agent stream.
//  2. Blocks the connections in deterministic stream order.
//  3. Delivers DIAL_RSP and DATA for healthy connection B after all slow DATA.
//  4. Requires B to establish and receive its exact DATA before any slow
//     frontend is released or closed.
//  5. Requires all slow connections to remain alive through B's progress.
//
// The contract is observable multi-connection isolation; it does not require a
// per-connection worker, queue, or other dispatch architecture.
func TestManySlowHTTPFrontendsDoNotDelayDialResponse(t *testing.T) {
	for _, slowFrontendCount := range []int{2, 10} {
		slowFrontendCount := slowFrontendCount
		t.Run(fmt.Sprintf("slow_frontends=%d", slowFrontendCount), func(t *testing.T) {
			runManySlowHTTPFrontendsDialResponseCase(t, slowFrontendCount)
		})
	}
}

func runManySlowHTTPFrontendsDialResponseCase(t *testing.T, slowFrontendCount int) {
	const (
		agentID    = "agent-1"
		dialIDB    = int64(2001)
		connectIDB = int64(3001)
		payloadB   = "response for healthy connection B"
	)

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		slowFrontendCount,
	)
	backend := &Backend{id: agentID}

	slowHTTPs := make([]*blockingHTTPReadWriter, 0, slowFrontendCount)
	connectIDs := make([]int64, 0, slowFrontendCount)
	for i := 0; i < slowFrontendCount; i++ {
		slowHTTP := newBlockingHTTPReadWriter()
		connectID := int64(1001 + i)
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      slowHTTP,
			CloseHTTP: func() error { slowHTTP.release(); return nil },
			connected: make(chan struct{}),
			connectID: connectID,
			agentID:   agentID,
			backend:   backend,
		}
		proxyServer.addEstablished(agentID, connectID, connection)
		slowHTTPs = append(slowHTTPs, slowHTTP)
		connectIDs = append(connectIDs, connectID)
	}

	recordingHTTP := newRecordingHTTPReadWriter()
	connectionB := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      recordingHTTP,
		CloseHTTP: func() error { return nil },
		connected: make(chan struct{}),
		dialID:    dialIDB,
		agentID:   agentID,
		start:     time.Now(),
		backend:   backend,
	}
	proxyServer.PendingDial.Add(dialIDB, connectionB)

	// Writer 0's DATA is dequeued before the remaining packets are staged.
	// Capacity N therefore holds exactly the remaining N-1 DATA packets plus
	// B's final DIAL_RSP, so the producer cannot become the source of the stall.
	recvCh := make(chan *client.Packet, slowFrontendCount)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()

	releaseAll := func() {
		for _, slowHTTP := range slowHTTPs {
			slowHTTP.release()
		}
	}
	t.Cleanup(func() {
		releaseAll()
		close(recvCh)
		select {
		case <-consumerDone:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("serveRecvBackend did not exit during test cleanup")
		}
	})

	// Establish one actually blocked frontend before staging the remaining slow
	// connections and B. The test does not require the implementation to start a
	// Write for every other slow connection; only healthy B's progress matters.
	recvCh <- dataPkt(connectIDs[0], []byte("response for slow connection 0"))
	select {
	case <-slowHTTPs[0].writeStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("first slow connection did not enter the blocking HTTP Write")
	}

	for i := 1; i < slowFrontendCount; i++ {
		recvCh <- dataPkt(connectIDs[i], []byte(fmt.Sprintf("response for slow connection %d", i)))
	}
	recvCh <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{
				Random:    dialIDB,
				ConnectID: connectIDB,
			},
		},
	}

	select {
	case <-connectionB.connected:
		// A DIAL_RSP-only priority mechanism is insufficient: after B establishes,
		// exact DATA for B must also traverse the same loaded backend path.
		recvCh <- dataPkt(connectIDB, []byte(payloadB))
		select {
		case got := <-recordingHTTP.writes:
			if string(got) != payloadB {
				t.Fatalf("connection B received payload %q, want %q", got, payloadB)
			}
		case <-time.After(holTestSafetyTimeout):
			t.Fatal("connection B established but its DATA was delayed by the slow HTTP frontends")
		}

		// Each slow connection has only one DATA packet, so closing one is not a
		// legitimate overflow response. The monotonic release signal catches a
		// connection killed at any point before healthy B's DATA was delivered.
		for i, slowHTTP := range slowHTTPs {
			if slowHTTP.released() {
				t.Fatalf("slow connection %d was released/closed to make healthy B progress", i)
			}
		}
	case <-time.After(holTestSafetyTimeout):
		// Release the slow frontends only after B failed to progress. If B then
		// establishes, their blocked writes caused the failure without constraining
		// how a correct implementation must dispatch writes.
		releaseAll()
		select {
		case <-connectionB.connected:
			t.Fatal("connection B established only after the slow HTTP frontends were released")
		case <-time.After(holTestSafetyTimeout):
			t.Fatal("connection B did not establish even after the slow HTTP frontends were released")
		}
	}
}

// TestSlowHTTPFrontendDoesNotSaturateBackendReceiveChannel targets the incident's
// propagated ingress symptom: "Receive channel from agent is full" and a stuck
// FullRecvChannel("Connect") gauge. It drives readBackendToChannel -> recvCh ->
// serveRecvBackend, but not the outer Connect RPC wrapper.
//
// For capacities 1 and 10, the test:
//  1. Blocks established connection A in its HTTP socket Write.
//  2. Delivers B's DIAL_RSP, then N+1 filler DATA packets for A so the backlog
//     exceeds a capacity-N backend receive channel.
//  3. Delivers terminal DATA for healthy B after the filler traffic.
//  4. Confirms the FullRecvChannel gauge rises while current code is stalled.
//  5. Requires B to establish and receive its exact DATA without test-releasing
//     A, then requires the shared receive-channel gauge to settle to zero.
//
// Capacity 1 is the minimal reproduction; capacity 10 matches the default
// konnectivity-server backend Connect receive channel (agent -> server), not the
// HTTP-CONNECT frontend path. A transient gauge increase is acceptable. A may
// also be overflow-closed under the frozen sustained-backpressure policy; the
// required outcome is that healthy traffic and shared ingress do not remain
// stuck. No frontend dispatch architecture is prescribed.
func TestSlowHTTPFrontendDoesNotSaturateBackendReceiveChannel(t *testing.T) {
	for _, capacity := range []int{1, 10} {
		capacity := capacity
		t.Run(fmt.Sprintf("capacity=%d", capacity), func(t *testing.T) {
			runSlowHTTPFrontendSaturationCase(t, capacity)
		})
	}
}

func runSlowHTTPFrontendSaturationCase(t *testing.T, capacity int) {
	const (
		agentID    = "agent-1"
		connectIDA = int64(1001)
		connectIDB = int64(1002)
		dialIDB    = int64(2002)
		payloadB   = "terminal response for healthy connection B"
	)

	metrics.Metrics.Reset()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	proxyServer := NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		capacity,
	)

	dialRSPFor := func(random, connID int64) *client.Packet {
		return &client.Packet{
			Type: client.PacketType_DIAL_RSP,
			Payload: &client.Packet_DialResponse{
				DialResponse: &client.DialResponse{
					Random:    random,
					ConnectID: connID,
				},
			},
		}
	}

	// Packets are released to the mock reader in stages, not preloaded, so the
	// sustained saturation is provably caused by A's parked write rather than by
	// the reader outrunning the consumer. The gate is opened only after A is
	// confirmed blocked. gateOnce lets cleanup reopen it safely if the test
	// fails before the main path does.
	gateOpen := make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gateOpen) }) }

	packets := make(chan *client.Packet)
	fillerCount := capacity + 1

	conn := mockAgentConn(ctrl, agentID, []string{})
	// A bounded backpressure policy may legitimately close A after sustained
	// overload. Later DATA for the now-missing frontend may repeat the close
	// request, so accept any number of CLOSE_REQ packets for A while rejecting
	// every other send.
	conn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		if pkt.Type != client.PacketType_CLOSE_REQ {
			t.Errorf("backend Send packet type = %v, want CLOSE_REQ", pkt.Type)
			return nil
		}
		closeReq := pkt.GetCloseRequest()
		if closeReq == nil || closeReq.ConnectID != connectIDA {
			t.Errorf("backend CLOSE_REQ = %v, want connectID %d", closeReq, connectIDA)
		}
		return nil
	}).AnyTimes()
	conn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
		if pkt, ok := <-packets; ok {
			return pkt, nil
		}
		return nil, io.EOF
	}).AnyTimes()
	backend, err := NewBackend(conn)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	slowHTTP := newBlockingHTTPReadWriter()
	connectionA := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      slowHTTP,
		CloseHTTP: func() error { slowHTTP.release(); return nil },
		connected: make(chan struct{}),
		connectID: connectIDA,
		agentID:   agentID,
		backend:   backend,
	}
	proxyServer.addEstablished(agentID, connectIDA, connectionA)

	recordingHTTP := newRecordingHTTPReadWriter()
	connectionB := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      recordingHTTP,
		CloseHTTP: func() error { return nil },
		connected: make(chan struct{}),
		dialID:    dialIDB,
		agentID:   agentID,
		start:     time.Now(),
		backend:   backend,
	}
	proxyServer.PendingDial.Add(dialIDB, connectionB)

	recvCh := make(chan *client.Packet, capacity)
	stopCh := make(chan error, 1)
	consumerDone := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		proxyServer.serveRecvBackend(backend, agentID, recvCh)
	}()
	go func() {
		defer close(readerDone)
		proxyServer.readBackendToChannel(backend, recvCh, stopCh)
	}()

	// The feeder owns the packets channel: it delivers A's DATA, then (after the
	// gate opens) B's DIAL_RSP, N+1 A fillers, and finally DATA for healthy B, then
	// closes packets so the reader sees EOF. allPacketsPulled reports that the
	// mock stream yielded every packet; it does NOT by itself prove those
	// packets were inserted into recvCh or consumed. Delivery to B is the
	// end-to-end progress proof.
	allPacketsPulled := make(chan struct{})
	go func() {
		defer close(allPacketsPulled)
		packets <- dataPkt(connectIDA, []byte("response for connection A"))
		<-gateOpen
		packets <- dialRSPFor(dialIDB, connectIDB)
		for i := 0; i < fillerCount; i++ {
			packets <- dataPkt(connectIDA, []byte("filler to saturate recvCh"))
		}
		packets <- dataPkt(connectIDB, []byte(payloadB))
		close(packets)
	}()

	t.Cleanup(func() {
		openGate()         // unblock the feeder if we failed before opening it.
		slowHTTP.release() // unblock A so the consumer and reader can drain.
		select {
		case <-allPacketsPulled:
		case <-time.After(holTestSafetyTimeout):
			t.Errorf("feeder did not finish delivering packets during cleanup")
		}
		select {
		case <-readerDone:
			// Only safe to close recvCh once the reader has stopped sending.
			close(recvCh) // mirrors production defer; lets the consumer loop end.
			select {
			case <-consumerDone:
			case <-time.After(holTestSafetyTimeout):
				t.Errorf("serveRecvBackend did not exit during test cleanup")
			}
		case <-time.After(holTestSafetyTimeout):
			// Do not close recvCh while readBackendToChannel may still send to
			// it; that would panic and mask the real failure.
			t.Errorf("readBackendToChannel did not exit during test cleanup; leaving recvCh open to avoid send-on-closed")
		}
	})

	// Wait until A is provably blocked inside its HTTP Write, then open the gate
	// so any saturation is attributable to A.
	select {
	case <-slowHTTP.writeStarted:
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("connection A did not enter the blocking HTTP Write")
	}
	openGate()

	// Desired behavior: B establishes without the test releasing A.
	select {
	case <-connectionB.connected:
	case <-time.After(holTestSafetyTimeout):
		// Failure diagnosis: prove that the receive channel remained saturated.
		// Poll rather than sampling once so scheduling does not hide the symptom;
		// an implementation that keeps draining may never raise the gauge.
		saturated := false
		for i := 0; i < 100; i++ {
			if promtest.ToFloat64(metrics.Metrics.FullRecvChannel(metrics.Connect)) >= 1 {
				saturated = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !saturated {
			t.Errorf("expected FullRecvChannel gauge >= 1 while consumer is stalled")
		}
		t.Fatal("connection B did not establish while connection A's HTTP Write was blocked; receive channel saturated")
	}

	// Note: we deliberately do NOT assert here that A is still blocked. A valid
	// bounded backpressure policy may close A while handling the fillers, and the
	// implementation can reach that point before this goroutine runs. The "B was not
	// rescued by killing A" invariant is proven deterministically in
	// TestSlowHTTPFrontendDoesNotDelayDialResponse, which feeds no packets after
	// B and therefore cannot legitimately close A at that point.

	// Terminal end-to-end progress proof: healthy B receives its exact DATA even
	// though A's filler traffic was staged first. This rejects control-only
	// prioritization and a shared writer pool that remains blocked behind A,
	// without requiring A's writes to use any particular dispatch mechanism.
	select {
	case got := <-recordingHTTP.writes:
		if string(got) != payloadB {
			t.Fatalf("connection B received payload %q, want %q", got, payloadB)
		}
	case <-time.After(holTestSafetyTimeout):
		t.Fatal("connection B established but terminal DATA did not progress through the saturated backend receive path")
	}

	// A transient gauge blip is acceptable; require it to settle to zero once
	// the consumer has drained the backlog. The shared backend receive channel
	// must no longer be stuck.
	settled := false
	for i := 0; i < 100; i++ {
		if promtest.ToFloat64(metrics.Metrics.FullRecvChannel(metrics.Connect)) == 0 {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Errorf("FullRecvChannel gauge did not return to 0; backend ingress is still stalled")
	}
}
