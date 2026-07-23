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
	"sync"
	"sync/atomic"
	"testing"
	"time"


	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
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
