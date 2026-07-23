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
