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
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/metadata"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
)

const writerTestSafetyTimeout = time.Second

type writerTestSentPacket struct {
	typeID    client.PacketType
	connectID int64
}

type writerTestAgentStream struct {
	ctx context.Context

	mu   sync.Mutex
	sent []writerTestSentPacket
}

func newWriterTestBackend(ctx context.Context, agentID string) (*Backend, *writerTestAgentStream) {
	if ctx == nil {
		ctx = context.Background()
	}
	stream := &writerTestAgentStream{ctx: ctx}
	return &Backend{id: agentID, conn: stream}, stream
}

func (s *writerTestAgentStream) Send(pkt *client.Packet) error {
	entry := writerTestSentPacket{typeID: pkt.Type}
	if closeReq := pkt.GetCloseRequest(); closeReq != nil {
		entry.connectID = closeReq.ConnectID
	}
	s.mu.Lock()
	s.sent = append(s.sent, entry)
	s.mu.Unlock()
	return nil
}

func (s *writerTestAgentStream) Recv() (*client.Packet, error) { return nil, io.EOF }
func (s *writerTestAgentStream) SetHeader(metadata.MD) error   { return nil }
func (s *writerTestAgentStream) SendHeader(metadata.MD) error  { return nil }
func (s *writerTestAgentStream) SetTrailer(metadata.MD)        {}
func (s *writerTestAgentStream) Context() context.Context      { return s.ctx }
func (s *writerTestAgentStream) SendMsg(any) error             { return nil }
func (s *writerTestAgentStream) RecvMsg(any) error             { return io.EOF }

func (s *writerTestAgentStream) count(packetType client.PacketType) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, pkt := range s.sent {
		if pkt.typeID == packetType {
			count++
		}
	}
	return count
}

func (s *writerTestAgentStream) closeRequestIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for _, pkt := range s.sent {
		if pkt.typeID == client.PacketType_CLOSE_REQ {
			ids = append(ids, pkt.connectID)
		}
	}
	return ids
}

func newWriterTestServer(queueDepth int) *ProxyServer {
	return NewProxyServer(
		"",
		[]proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault},
		1,
		nil,
		queueDepth,
	)
}

func writerTestEventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(writerTestSafetyTimeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

type writerTestImmediateHTTP struct {
	mu      sync.Mutex
	stream  []byte
	closed  bool
	closes  int
	updated chan struct{}
	closeCh chan struct{}

	closeOnce sync.Once
}

func newWriterTestImmediateHTTP() *writerTestImmediateHTTP {
	return &writerTestImmediateHTTP{
		updated: make(chan struct{}, 1),
		closeCh: make(chan struct{}),
	}
}

func (w *writerTestImmediateHTTP) Read([]byte) (int, error) { return 0, io.EOF }

func (w *writerTestImmediateHTTP) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	w.stream = append(w.stream, p...)
	select {
	case w.updated <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *writerTestImmediateHTTP) close() error {
	w.mu.Lock()
	w.closes++
	w.closed = true
	w.mu.Unlock()
	w.closeOnce.Do(func() { close(w.closeCh) })
	return nil
}

func (w *writerTestImmediateHTTP) snapshot() ([]byte, int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.stream...), w.closes, w.closed
}

type writerTestBlockingHTTP struct {
	writeStarted chan struct{}
	writeDone    chan struct{}
	releaseWrite chan struct{}
	closeCh      chan struct{}
	updated      chan struct{}

	mu     sync.Mutex
	stream []byte
	closed bool
	closes int

	startOnce   sync.Once
	doneOnce    sync.Once
	releaseOnce sync.Once
	closeOnce   sync.Once
}

func newWriterTestBlockingHTTP() *writerTestBlockingHTTP {
	return &writerTestBlockingHTTP{
		writeStarted: make(chan struct{}),
		writeDone:    make(chan struct{}),
		releaseWrite: make(chan struct{}),
		closeCh:      make(chan struct{}),
		updated:      make(chan struct{}, 1),
	}
}

func (w *writerTestBlockingHTTP) Read([]byte) (int, error) { return 0, io.EOF }

func (w *writerTestBlockingHTTP) Write(p []byte) (int, error) {
	w.startOnce.Do(func() { close(w.writeStarted) })
	<-w.releaseWrite
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.doneOnce.Do(func() { close(w.writeDone) })
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	w.stream = append(w.stream, p...)
	select {
	case w.updated <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *writerTestBlockingHTTP) release() {
	w.releaseOnce.Do(func() { close(w.releaseWrite) })
}

func (w *writerTestBlockingHTTP) close() error {
	w.mu.Lock()
	w.closes++
	w.closed = true
	w.mu.Unlock()
	w.closeOnce.Do(func() { close(w.closeCh) })
	w.release()
	return nil
}

func (w *writerTestBlockingHTTP) snapshot() ([]byte, int, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.stream...), w.closes, w.closed
}

type writerTestGateFailHTTP struct {
	started chan struct{}
	release chan struct{}
	err     error

	startOnce   sync.Once
	releaseOnce sync.Once
}

func newWriterTestGateFailHTTP(err error) *writerTestGateFailHTTP {
	return &writerTestGateFailHTTP{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     err,
	}
}

func (w *writerTestGateFailHTTP) Read([]byte) (int, error) { return 0, io.EOF }

func (w *writerTestGateFailHTTP) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.release
	return 0, w.err
}

func (w *writerTestGateFailHTTP) unblock() {
	w.releaseOnce.Do(func() { close(w.release) })
}

type writerTestOneByteHTTP struct {
	mu     sync.Mutex
	stream []byte
}

func (w *writerTestOneByteHTTP) Read([]byte) (int, error) { return 0, io.EOF }

func (w *writerTestOneByteHTTP) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	w.stream = append(w.stream, p[0])
	w.mu.Unlock()
	return 1, nil
}

func (w *writerTestOneByteHTTP) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.stream...)
}

type writerTestZeroWriter struct {
	calls atomic.Int32
}

func (w *writerTestZeroWriter) Write([]byte) (int, error) {
	w.calls.Add(1)
	return 0, nil
}

// This test protects the writer's load-bearing synchronization contract. It is
// intentionally run under the package race job: enqueue publication and both
// terminal transitions must remain panic-free and idempotent under overlap.
func TestHTTPConnectWriterConcurrentTerminalTransitions(t *testing.T) {
	const iterations = 300
	for i := 0; i < iterations; i++ {
		server := newWriterTestServer(8)
		var closeCalls atomic.Int32
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      newWriterTestImmediateHTTP(),
			CloseHTTP: func() error { closeCalls.Add(1); return nil },
			closed:    make(chan struct{}),
			agentID:   "race-agent",
			connectID: int64(i + 1),
		}
		writer, attached := connection.attachHTTPWriter(server, nil, false)
		if !attached {
			t.Fatal("failed to attach writer to a live connection")
		}

		start := make(chan struct{})
		var workers sync.WaitGroup
		for worker := 0; worker < 8; worker++ {
			workers.Add(1)
			go func(worker int) {
				defer workers.Done()
				<-start
				for packet := 0; packet < 8; packet++ {
					writer.enqueueData([]byte{byte(worker), byte(packet)})
				}
			}(worker)
		}
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			writer.beginGracefulClose()
		}()
		go func() {
			defer workers.Done()
			<-start
			writer.abort(httpConnectAbortFrontendClose)
		}()

		close(start)
		workers.Wait()
		// Replay both transitions after the raced calls return to verify that the
		// terminal APIs remain idempotent.
		writer.abort(httpConnectAbortWriteFailure)
		writer.beginGracefulClose()

		writerTestEventually(t, "exactly one terminal HTTP close", func() bool {
			return closeCalls.Load() == 1
		})
		writer.mu.Lock()
		accepting, aborted, closeSeen := writer.accepting, writer.aborted, writer.closeResponseSeen
		writer.mu.Unlock()
		if accepting || !aborted || !closeSeen {
			t.Fatalf("terminal writer state = accepting:%t aborted:%t closeResponseSeen:%t", accepting, aborted, closeSeen)
		}
		if got := closeCalls.Load(); got != 1 {
			t.Fatalf("CloseHTTP calls = %d, want 1", got)
		}
	}
}

// A stale asynchronous graceful completion must not evict a replacement that
// reused the same protocol IDs.
func TestHTTPConnectStaleWriterCannotRemoveReplacement(t *testing.T) {
	const (
		agentID   = "reuse-agent"
		connectID = int64(73)
	)
	server := newWriterTestServer(2)
	oldHTTP := newWriterTestBlockingHTTP()
	oldConnection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      oldHTTP,
		CloseHTTP: oldHTTP.close,
		closed:    make(chan struct{}),
		agentID:   agentID,
		connectID: connectID,
	}
	server.addEstablished(agentID, connectID, oldConnection)
	oldWriter, _ := oldConnection.attachHTTPWriter(server, nil, false)
	oldWriter.start()
	if got := oldWriter.enqueueData([]byte("old payload")); got != httpConnectEnqueueAccepted {
		t.Fatalf("enqueue result = %v, want accepted", got)
	}
	select {
	case <-oldHTTP.writeStarted:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("old writer did not enter its blocked write")
	}
	oldWriter.beginGracefulClose()

	replacement := &ProxyClientConnection{Mode: ModeHTTPConnect, agentID: agentID, connectID: connectID}
	server.addEstablished(agentID, connectID, replacement)
	oldHTTP.release()
	select {
	case <-oldHTTP.closeCh:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("stale writer did not finish graceful cleanup")
	}

	got, err := server.getFrontend(agentID, connectID)
	if err != nil {
		t.Fatalf("replacement was removed by stale completion: %v", err)
	}
	if got != replacement {
		t.Fatalf("established pointer = %p, want replacement %p", got, replacement)
	}
}

func TestHTTPConnectWriterPreservesBytesAcrossPartialWrites(t *testing.T) {
	const (
		agentID   = "partial-agent"
		connectID = int64(91)
	)
	payloads := [][]byte{[]byte("alpha"), []byte("-"), []byte("beta"), []byte("-gamma")}
	want := bytes.Join(payloads, nil)
	server := newWriterTestServer(len(payloads))
	httpWriter := &writerTestOneByteHTTP{}
	connection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      httpWriter,
		closed:    make(chan struct{}),
		agentID:   agentID,
		connectID: connectID,
	}
	server.addEstablished(agentID, connectID, connection)
	writer, _ := connection.attachHTTPWriter(server, nil, false)
	writer.start()
	for _, payload := range payloads {
		if got := writer.enqueueData(payload); got != httpConnectEnqueueAccepted {
			t.Fatalf("enqueue result = %v, want accepted", got)
		}
	}
	writer.beginGracefulClose()
	select {
	case <-connection.closed:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("partial-write stream did not close after draining")
	}
	if got := httpWriter.bytes(); !bytes.Equal(got, want) {
		t.Fatalf("reassembled stream = %q, want %q", got, want)
	}

	zeroWriter := &writerTestZeroWriter{}
	if err := writeAll(zeroWriter, []byte("no progress")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-progress write error = %v, want %v", err, io.ErrShortWrite)
	}
	if got := zeroWriter.calls.Load(); got != 1 {
		t.Fatalf("zero-progress writer calls = %d, want 1", got)
	}
}

func TestHTTPConnectInitialResponseFailures(t *testing.T) {
	t.Run("successful dial response", func(t *testing.T) {
		const (
			agentID   = "response-agent"
			connectID = int64(101)
		)
		server := newWriterTestServer(1)
		backend, stream := newWriterTestBackend(context.Background(), agentID)
		writeErr := errors.New("response write failed")
		httpWriter := newWriterTestGateFailHTTP(writeErr)
		connected := make(chan struct{})
		var closeCalls atomic.Int32
		connection := &ProxyClientConnection{
			Mode:                ModeHTTPConnect,
			HTTP:                httpWriter,
			CloseHTTP:           func() error { closeCalls.Add(1); return nil },
			closed:              make(chan struct{}),
			connected:           connected,
			httpInitialResponse: []byte(httpConnectSuccessResponse),
			agentID:             agentID,
			connectID:           connectID,
			backend:             backend,
		}
		server.addEstablished(agentID, connectID, connection)
		writer, attached := connection.configuredHTTPWriter(server, true)
		if !attached {
			t.Fatal("successful response writer was not attached")
		}
		writer.start()
		select {
		case <-httpWriter.started:
		case <-time.After(writerTestSafetyTimeout):
			t.Fatal("successful response write did not start")
		}
		httpWriter.unblock()
		writerTestEventually(t, "failed response cleanup", func() bool {
			return closeCalls.Load() == 1 && stream.count(client.PacketType_CLOSE_REQ) == 1
		})
		select {
		case <-connected:
			t.Fatal("connected was signaled after the successful response write failed")
		default:
		}
		if got, err := server.getFrontend(agentID, connectID); err != nil || got != connection {
			t.Fatalf("failed response terminal entry = %p, %v; want retained %p", got, err, connection)
		}
		if writer.beginGracefulClose() != httpConnectCloseAlreadyForced {
			t.Fatal("backend acknowledgement did not observe the forced response failure")
		}
		server.removeEstablishedIf(agentID, connectID, connection)
	})

	t.Run("failed dial response with zero connection ID", func(t *testing.T) {
		const agentID = "failed-dial-agent"
		server := newWriterTestServer(1)
		backend, stream := newWriterTestBackend(context.Background(), agentID)
		replacement := &ProxyClientConnection{Mode: ModeHTTPConnect, agentID: agentID}
		server.addEstablished(agentID, 0, replacement)

		writeErr := errors.New("error response write failed")
		httpWriter := newWriterTestGateFailHTTP(writeErr)
		var closeCalls atomic.Int32
		connected := make(chan struct{})
		failedDial := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      httpWriter,
			CloseHTTP: func() error { closeCalls.Add(1); return nil },
			closed:    make(chan struct{}),
			connected: connected,
			agentID:   agentID,
			backend:   backend,
		}
		writer, attached := failedDial.attachHTTPWriter(server, serializeHTTPConnectDialError("dial failed"), false)
		if !attached {
			t.Fatal("failed-dial writer was not attached")
		}
		writer.start()
		select {
		case <-httpWriter.started:
		case <-time.After(writerTestSafetyTimeout):
			t.Fatal("failed-dial error response write did not start")
		}
		writer.beginGracefulClose()
		httpWriter.unblock()
		writerTestEventually(t, "failed-dial response cleanup", func() bool {
			return closeCalls.Load() == 1
		})
		select {
		case <-connected:
			t.Fatal("failed dial signaled connected")
		default:
		}
		failedDial.sendBackendCloseRequest(server, "test completion")
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 0 {
			t.Fatalf("failed dial sent %d CLOSE_REQ packets with connection ID zero", got)
		}
		got, err := server.getFrontend(agentID, 0)
		if err != nil || got != replacement {
			t.Fatalf("zero-ID replacement = %p, %v; want %p", got, err, replacement)
		}

		serialized := serializeHTTPConnectDialError(strings.Repeat("x", maxHTTPConnectErrorBodyBytes+512))
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(serialized)), nil)
		if err != nil {
			t.Fatalf("parse serialized error response: %v", err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("read serialized error body: %v", err)
		}
		if len(body) != maxHTTPConnectErrorBodyBytes {
			t.Fatalf("error response body length = %d, want %d", len(body), maxHTTPConnectErrorBodyBytes)
		}
		if !bytes.HasSuffix(body, []byte(httpConnectTruncationMarker)) {
			t.Fatalf("truncated error body does not end with marker: %q", body[len(body)-64:])
		}
	})
}

func TestHTTPConnectDeadBackendSendGuards(t *testing.T) {
	t.Run("overflow cleanup outlives backend shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server := newWriterTestServer(1)
		backend, stream := newWriterTestBackend(ctx, "dead-agent")
		closeStarted := make(chan struct{})
		closeRelease := make(chan struct{})
		closeReturned := make(chan struct{})
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      newWriterTestImmediateHTTP(),
			closed:    make(chan struct{}),
			agentID:   "dead-agent",
			connectID: 201,
			backend:   backend,
			CloseHTTP: func() error {
				close(closeStarted)
				<-closeRelease
				close(closeReturned)
				return nil
			},
		}
		writer, _ := connection.attachHTTPWriter(server, nil, false)
		if got := writer.enqueueData([]byte("queued")); got != httpConnectEnqueueAccepted {
			t.Fatalf("first enqueue = %v, want accepted", got)
		}
		if got := writer.enqueueData([]byte("overflow")); got != httpConnectEnqueueOverflow {
			t.Fatalf("second enqueue = %v, want overflow", got)
		}
		select {
		case <-closeStarted:
		case <-time.After(writerTestSafetyTimeout):
			t.Fatal("overflow cleanup did not enter CloseHTTP")
		}
		connection.suppressBackendCloseRequest()
		cancel()
		close(closeRelease)
		select {
		case <-closeReturned:
		case <-time.After(writerTestSafetyTimeout):
			t.Fatal("overflow CloseHTTP did not return")
		}
		// Settle the once guard synchronously if the cleanup goroutine has not yet
		// reached it. Either caller must observe suppression.
		connection.sendBackendCloseRequest(server, "backend shutdown")
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 0 {
			t.Fatalf("dead backend received %d CLOSE_REQ packets", got)
		}
	})

	t.Run("nonzero pre-publication ID with canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		server := newWriterTestServer(1)
		backend, stream := newWriterTestBackend(ctx, "canceled-agent")
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			agentID:   "canceled-agent",
			connectID: 202,
			backend:   backend,
		}
		connection.sendBackendCloseRequest(server, "setup race")
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 0 {
			t.Fatalf("canceled pre-publication backend received %d CLOSE_REQ packets", got)
		}
	})
}

func TestHTTPConnectTerminalSideEffectsConvergeExactlyOnce(t *testing.T) {
	const iterations = 100
	for i := 0; i < iterations; i++ {
		server := newWriterTestServer(1)
		backend, stream := newWriterTestBackend(context.Background(), "terminal-agent")
		var closeCalls atomic.Int32
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      newWriterTestImmediateHTTP(),
			CloseHTTP: func() error { closeCalls.Add(1); return nil },
			closed:    make(chan struct{}),
			agentID:   "terminal-agent",
			connectID: int64(300 + i),
			backend:   backend,
		}
		server.addEstablished(connection.agentID, connection.connectID, connection)
		writer, _ := connection.attachHTTPWriter(server, nil, false)
		if got := writer.enqueueData([]byte("queued")); got != httpConnectEnqueueAccepted {
			t.Fatalf("initial enqueue = %v, want accepted", got)
		}

		start := make(chan struct{})
		result := make(chan httpConnectCloseDisposition, 1)
		var workers sync.WaitGroup
		workers.Add(4)
		go func() {
			defer workers.Done()
			<-start
			result <- writer.beginGracefulClose()
		}()
		go func() {
			defer workers.Done()
			<-start
			writer.abort(httpConnectAbortWriteFailure)
		}()
		go func() {
			defer workers.Done()
			<-start
			connection.abortHTTP(server, httpConnectAbortFrontendClose)
		}()
		go func() {
			defer workers.Done()
			<-start
			writer.enqueueData([]byte("racing enqueue"))
		}()
		close(start)
		workers.Wait()
		// Mirror CLOSE_RSP routing: if forced cleanup won first, the response
		// path owns removal of the terminal map entry.
		if <-result == httpConnectCloseAlreadyForced {
			server.removeEstablishedIf(connection.agentID, connection.connectID, connection)
		}

		writerTestEventually(t, "converged terminal side effects", func() bool {
			return closeCalls.Load() == 1 && stream.count(client.PacketType_CLOSE_REQ) == 1
		})
		// With convergence observed, replay every terminal API to prove that none
		// can duplicate a side effect or resurrect map state.
		connection.abortHTTP(server, httpConnectAbortFrontendClose)
		writer.abort(httpConnectAbortWriteFailure)
		_ = connection.closeHTTP()
		connection.sendBackendCloseRequest(server, "duplicate terminal call")
		if got := closeCalls.Load(); got != 1 {
			t.Fatalf("CloseHTTP calls = %d, want 1", got)
		}
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 1 {
			t.Fatalf("connection-owned CLOSE_REQ count = %d, want 1", got)
		}
		if _, err := server.getFrontend(connection.agentID, connection.connectID); err == nil {
			t.Fatal("terminal connection remained or was resurrected in established state")
		}
	}
}

func TestHTTPConnectQueueOverflowMetricCardinalityAndReset(t *testing.T) {
	metrics.Metrics.Reset()
	t.Cleanup(metrics.Metrics.Reset)

	server := newWriterTestServer(1)
	connection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      newWriterTestImmediateHTTP(),
		closed:    make(chan struct{}),
		agentID:   "metric-agent",
		connectID: 401,
	}
	writer, _ := connection.attachHTTPWriter(server, nil, false)
	if got := writer.enqueueData([]byte("queued")); got != httpConnectEnqueueAccepted {
		t.Fatalf("first enqueue = %v, want accepted", got)
	}
	if got := writer.enqueueData([]byte("overflow")); got != httpConnectEnqueueOverflow {
		t.Fatalf("second enqueue = %v, want overflow", got)
	}

	const metricName = metrics.Namespace + "_" + metrics.Subsystem + "_http_connect_disconnect_count"
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	found := false
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		found = true
		if len(family.Metric) != 1 {
			t.Fatalf("overflow metric series = %d, want 1", len(family.Metric))
		}
		metric := family.Metric[0]
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "reason" || metric.Label[0].GetValue() != "queue_overflow" {
			t.Fatalf("overflow metric labels = %v, want only reason=queue_overflow", metric.Label)
		}
		if got := metric.GetCounter().GetValue(); got != 1 {
			t.Fatalf("overflow metric value = %v, want 1", got)
		}
	}
	if !found {
		t.Fatalf("metric family %q was not gathered", metricName)
	}

	metrics.Metrics.Reset()
	metrics.Metrics.Reset()
	families, err = prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather reset metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() == metricName && len(family.Metric) != 0 {
			t.Fatalf("overflow metric retained %d series after reset", len(family.Metric))
		}
	}
}
