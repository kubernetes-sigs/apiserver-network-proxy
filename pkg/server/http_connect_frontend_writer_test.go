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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type writerTestConn struct {
	net.Conn
	writes chan string
	closes atomic.Int32
}

func (c *writerTestConn) Write(data []byte) (int, error) {
	c.writes <- string(data)
	return c.Conn.Write(data)
}

func (c *writerTestConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

type writerTestFrontend struct {
	stream *httpConnectStream
	conn   *writerTestConn
	peer   net.Conn
	reader *bufio.Reader
}

func newWriterTestFrontend(t *testing.T, queueSize int) *writerTestFrontend {
	t.Helper()
	conn, peer := net.Pipe()
	traced := &writerTestConn{Conn: conn, writes: make(chan string, 32)}
	stream := newHTTPConnectStream(&http.Request{Host: "example.test:443"}, traced,
		bufio.NewReadWriter(bufio.NewReader(traced), bufio.NewWriter(traced)), queueSize)
	if err := peer.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stream.release()
		peer.Close()
	})
	return &writerTestFrontend{stream: stream, conn: traced, peer: peer, reader: bufio.NewReader(peer)}
}

func waitWriterSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func (f *writerTestFrontend) readHandshake(t *testing.T) {
	t.Helper()
	resp, err := http.ReadResponse(f.reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT returned %s", resp.Status)
	}
	select {
	case got := <-f.conn.writes:
		if got != connectEstablished {
			t.Fatalf("first socket write = %q, want HTTP handshake", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("missing HTTP handshake write")
	}
	waitWriterSignal(t, f.stream.connected, "HTTP handshake completion")
}

func (f *writerTestFrontend) waitWrite(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-f.conn.writes:
		if got != want {
			t.Fatalf("socket write = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no socket write for %q", want)
	}
}

func (f *writerTestFrontend) readData(t *testing.T, want string) {
	t.Helper()
	data := make([]byte, len(want))
	if _, err := io.ReadFull(f.reader, data); err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("received %q, want %q", data, want)
	}
}

type writerTestServer struct {
	proxy   *ProxyServer
	backend *Backend
	packets chan *client.Packet
	done    chan struct{}
	cancel  context.CancelFunc
}

func newWriterTestServer(t *testing.T) *writerTestServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := gomock.NewController(t)
	conn := agentmock.NewMockAgentService_ConnectServer(ctrl)
	conn.EXPECT().Context().Return(ctx).AnyTimes()
	conn.EXPECT().Send(gomock.Any()).Return(nil).AnyTimes()
	f := &writerTestServer{
		proxy:   NewProxyServer("test", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, nil, 10),
		backend: &Backend{id: "agent", conn: conn},
		packets: make(chan *client.Packet),
		done:    make(chan struct{}),
		cancel:  cancel,
	}
	go func() {
		defer close(f.done)
		f.proxy.serveRecvBackend(f.backend, f.backend.GetAgentID(), f.packets)
	}()
	t.Cleanup(func() {
		cancel()
		close(f.packets)
		waitWriterSignal(t, f.done, "backend dispatcher shutdown")
	})
	return f
}

func (s *writerTestServer) send(t *testing.T, pkt *client.Packet) {
	t.Helper()
	select {
	case s.packets <- pkt:
	case <-time.After(5 * time.Second):
		t.Fatalf("backend dispatcher blocked on %v", pkt.Type)
	}
}

func (s *writerTestServer) establish(t *testing.T, id int64, queueSize int) *writerTestFrontend {
	t.Helper()
	f := newWriterTestFrontend(t, queueSize)
	s.proxy.PendingDial.Add(id, &ProxyClientConnection{
		frontend: &Frontend{stream: f.stream, streamUID: fmt.Sprintf("stream-%d", id)},
		dialID:   id, backend: s.backend, start: time.Now(),
	})
	s.send(t, dialRspPkt(id, id))
	f.readHandshake(t)
	t.Cleanup(func() {
		f.stream.close()
		if f.stream.writerDone != nil {
			waitWriterSignal(t, f.stream.writerDone, "frontend writer shutdown")
		}
	})
	return f
}

func TestHTTPConnectWriterPreservesHandshakeDataAndCloseOrder(t *testing.T) {
	s := newWriterTestServer(t)
	f := s.establish(t, 1, 3)
	s.send(t, dataPkt(1, []byte("first")))
	f.waitWrite(t, "first") // net.Pipe blocks this write until the peer reads.
	s.send(t, dataPkt(1, []byte("second")))
	s.send(t, closeRspPkt(1, ""))
	s.send(t, &client.Packet{Type: client.PacketType_DRAIN}) // flush dispatch, not the writer

	if _, err := s.proxy.getFrontend("agent", 1); err == nil {
		t.Fatal("connection with a queued CLOSE_RSP remains routable")
	}
	if f.stream.isClosed() {
		t.Fatal("CLOSE_RSP discarded the queued DATA")
	}
	// Duplicate close must not enqueue another packet or abort the ordered close.
	s.send(t, closeRspPkt(1, ""))
	got, err := io.ReadAll(f.reader)
	if err != nil || string(got) != "firstsecond" {
		t.Fatalf("received %q, %v; want all DATA before EOF", got, err)
	}
	waitWriterSignal(t, f.stream.writerDone, "ordered close")
	if got := f.conn.closes.Load(); got != 1 {
		t.Fatalf("socket closed %d times, want once", got)
	}
}

func TestHTTPConnectWriterDelaysHOLByQueueDepth(t *testing.T) {
	const depth = 3
	s := newWriterTestServer(t)
	slow := s.establish(t, 1, depth)
	s.send(t, dataPkt(1, []byte("slow-0")))
	slow.waitWrite(t, "slow-0")

	// A new dial and its DATA must progress while the first socket is blocked.
	healthy := s.establish(t, 2, depth)
	var want strings.Builder
	want.WriteString("slow-0")
	for i := 1; i <= depth; i++ {
		payload := fmt.Sprintf("slow-%d", i)
		want.WriteString(payload)
		s.send(t, dataPkt(1, []byte(payload)))
		s.send(t, dataPkt(2, []byte("healthy")))
		healthy.readData(t, "healthy")
	}
	if got := len(slow.stream.writeCh); got != depth {
		t.Fatalf("queue length = %d, want %d", got, depth)
	}

	want.WriteString("overflow")
	s.send(t, dataPkt(1, []byte("overflow")))
	select {
	case s.packets <- dataPkt(2, []byte("too-early")):
		t.Fatal("dispatch did not block when the slow queue filled")
	case <-time.After(50 * time.Millisecond):
	}
	drained := make(chan error, 1)
	go func() {
		data := make([]byte, want.Len())
		_, err := io.ReadFull(slow.reader, data)
		if err == nil && string(data) != want.String() {
			err = fmt.Errorf("received %q, want %q", data, want.String())
		}
		drained <- err
	}()
	s.send(t, dataPkt(2, []byte("resumed")))
	healthy.readData(t, "resumed")
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow frontend did not drain after resuming")
	}
}

func TestHTTPConnectWriterBackendDisconnectInterruptsWrites(t *testing.T) {
	for _, queuedClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued-close=%t", queuedClose), func(t *testing.T) {
			s := newWriterTestServer(t)
			slow := s.establish(t, 1, 1)
			healthy := s.establish(t, 2, 1)
			s.send(t, dataPkt(1, []byte("blocked")))
			slow.waitWrite(t, "blocked")
			if queuedClose {
				s.send(t, closeRspPkt(1, ""))
				s.send(t, &client.Packet{Type: client.PacketType_DRAIN})
				if _, err := s.proxy.getFrontend("agent", 1); err == nil {
					t.Fatal("queued-close frontend is still routable")
				}
			} else {
				s.send(t, dataPkt(1, []byte("queued")))
				s.send(t, dataPkt(1, []byte("overflow")))
			}
			// Cancellation must bypass Frontend.sendLock and interrupt both
			// a blocked socket write and a Send waiting for queue capacity.
			s.cancel()
			waitWriterSignal(t, slow.stream.writerDone, "blocked writer on backend disconnect")
			waitWriterSignal(t, healthy.stream.writerDone, "other writer on backend disconnect")
			s.send(t, &client.Packet{Type: client.PacketType_DRAIN})
			for _, f := range []*writerTestFrontend{slow, healthy} {
				if got := f.conn.closes.Load(); got != 1 {
					t.Fatalf("socket closed %d times, want once", got)
				}
			}
		})
	}
}

func TestHTTPConnectWriterDrainsAfterBackendEOF(t *testing.T) {
	for _, tc := range []struct {
		queueSize     int
		closeResponse bool
	}{
		{queueSize: 1, closeResponse: false},
		{queueSize: 3, closeResponse: true},
		{queueSize: 0, closeResponse: false},
		{queueSize: 0, closeResponse: true},
	} {
		t.Run(fmt.Sprintf("queue=%d/close-response=%t", tc.queueSize, tc.closeResponse), func(t *testing.T) {
			s := NewProxyServer("test", []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, 10)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			grpcServer := grpc.NewServer()
			agent.RegisterAgentServiceServer(grpcServer, s)
			go func() { _ = grpcServer.Serve(listener) }()
			t.Cleanup(grpcServer.Stop)
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs(header.AgentID, "agent")), 5*time.Second)
			defer cancel()
			stream, err := agent.NewAgentServiceClient(conn).Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Header(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for s.BackendManagers[0].NumBackends() == 0 {
				if time.Now().After(deadline) {
					t.Fatal("agent did not register")
				}
				time.Sleep(time.Millisecond)
			}

			f := newWriterTestFrontend(t, tc.queueSize)
			proxyDone := make(chan struct{})
			go func() {
				defer close(proxyDone)
				_ = s.proxy(f.stream)
			}()
			t.Cleanup(func() {
				f.stream.close()
				waitWriterSignal(t, proxyDone, "frontend handler shutdown")
			})
			req, err := stream.Recv()
			if err != nil || req.GetDialRequest() == nil {
				t.Fatalf("DIAL_REQ = %v, %v", req, err)
			}
			if err := stream.Send(dialRspPkt(req.GetDialRequest().Random, 1)); err != nil {
				t.Fatal(err)
			}
			f.readHandshake(t)
			t.Cleanup(func() {
				f.stream.close()
				if f.stream.writerDone != nil {
					waitWriterSignal(t, f.stream.writerDone, "frontend writer shutdown")
				}
			})
			if err := stream.Send(dataPkt(1, []byte("first"))); err != nil {
				t.Fatal(err)
			}
			f.waitWrite(t, "first")
			// A small or disabled queue leaves DATA in the shared receive path;
			// a larger queue also admits CLOSE_RSP. EOF must preserve both cases.
			for _, payload := range []string{"second", "third"} {
				if err := stream.Send(dataPkt(1, []byte(payload))); err != nil {
					t.Fatal(err)
				}
			}
			if tc.closeResponse {
				if err := stream.Send(closeRspPkt(1, "")); err != nil {
					t.Fatal(err)
				}
			}
			// Only the buffered writer can accept CLOSE_RSP before the peer
			// resumes reading. The synchronous dispatcher is still in Write.
			if tc.closeResponse && tc.queueSize > 0 {
				deadline = time.Now().Add(time.Second)
				for {
					if _, err := s.getFrontend("agent", 1); err != nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("connection with queued CLOSE_RSP remains established")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Recv(); err != io.EOF {
				t.Fatalf("agent stream returned %v, want EOF", err)
			}
			// A normal gRPC handler return cancels its context too. Let its
			// callbacks run while the frontend is still temporarily slow.
			select {
			case <-f.stream.closed:
				t.Fatal("clean backend EOF aborted frontend writes")
			case <-time.After(50 * time.Millisecond):
			}
			got, err := io.ReadAll(f.reader)
			if err != nil || string(got) != "firstsecondthird" {
				t.Fatalf("received %q, %v; want all DATA before EOF", got, err)
			}
			if f.stream.writerDone != nil {
				waitWriterSignal(t, f.stream.writerDone, "writer drain")
			}
			waitWriterSignal(t, proxyDone, "frontend handler shutdown")
		})
	}
}

func TestHTTPConnectWriterFrontendCloseUnblocksFullQueue(t *testing.T) {
	s := newWriterTestServer(t)
	slow := s.establish(t, 1, 1)
	healthy := s.establish(t, 2, 1)
	s.send(t, dataPkt(1, []byte("blocked")))
	slow.waitWrite(t, "blocked")
	s.send(t, dataPkt(1, []byte("queued")))
	s.send(t, dataPkt(1, []byte("overflow")))
	if err := slow.stream.Close(); err != nil {
		t.Fatal(err)
	}
	waitWriterSignal(t, slow.stream.writerDone, "closed frontend writer")
	s.send(t, dataPkt(2, []byte("healthy")))
	healthy.readData(t, "healthy")
	if healthy.stream.isClosed() {
		t.Fatal("closing one frontend closed another")
	}
}

func TestHTTPConnectWriterSocketErrorClosesStream(t *testing.T) {
	s := newWriterTestServer(t)
	f := s.establish(t, 1, 1)
	f.peer.Close()
	s.send(t, dataPkt(1, []byte("write-fails")))
	waitWriterSignal(t, f.stream.writerDone, "writer with failed socket")
	if err := f.stream.Context().Err(); err != context.Canceled {
		t.Fatalf("stream context error = %v, want canceled", err)
	}
	if err := f.stream.Send(dataPkt(1, []byte("late"))); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("late Send returned %v, want closed", err)
	}
}

func TestHTTPConnectWriterDefaultsNegativeQueueSize(t *testing.T) {
	f := newWriterTestFrontend(t, -1)
	if got := cap(f.stream.writeCh); got != defaultFrontendWriteChannelSize {
		t.Fatalf("capacity = %d, want %d", got, defaultFrontendWriteChannelSize)
	}
}

func TestHTTPConnectZeroWriteQueueUsesSynchronousWrites(t *testing.T) {
	metrics.Metrics.Reset()
	s := newWriterTestServer(t)
	f := s.establish(t, 1, 0)
	if f.stream.writeCh != nil || f.stream.writerDone != nil {
		t.Fatal("zero queue size allocated writer channels")
	}

	sent := make(chan error, 1)
	go func() { sent <- f.stream.Send(dataPkt(1, []byte("inline"))) }()
	f.waitWrite(t, "inline")
	select {
	case err := <-sent:
		t.Fatalf("Send returned before the socket write completed: %v", err)
	default:
	}
	waitWriterMetrics(t, 0, 0)
	f.readData(t, "inline")
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return after the socket write completed")
	}

	s.send(t, closeRspPkt(1, ""))
	s.send(t, &client.Packet{Type: client.PacketType_DRAIN}) // Wait for close dispatch.
	if !f.stream.isClosed() || f.conn.closes.Load() != 1 {
		t.Fatal("CLOSE_RSP did not close the synchronous stream exactly once")
	}
	waitWriterMetrics(t, 0, 0)
}

func TestHTTPConnectZeroWriteQueueCloseInterruptsWrite(t *testing.T) {
	for _, backendClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("backend-close=%t", backendClose), func(t *testing.T) {
			metrics.Metrics.Reset()
			s := newWriterTestServer(t)
			f := s.establish(t, 1, 0)
			s.send(t, dataPkt(1, []byte("blocked")))
			f.waitWrite(t, "blocked")
			if backendClose {
				s.cancel()
			} else {
				f.stream.close()
			}
			s.send(t, &client.Packet{Type: client.PacketType_DRAIN}) // The dispatcher must be unblocked.
			if !f.stream.isClosed() || f.conn.closes.Load() != 1 {
				t.Fatal("stream was not closed exactly once")
			}
			waitWriterMetrics(t, 0, 0)
		})
	}
}

func TestHTTPConnectZeroWriteQueueReturnsSocketError(t *testing.T) {
	s := newWriterTestServer(t)
	f := s.establish(t, 1, 0)
	f.peer.Close()
	if err := f.stream.Send(dataPkt(1, []byte("write-fails"))); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Send returned %v, want socket write error", err)
	}
}

func TestHTTPConnectStreamCloseIsIdempotent(t *testing.T) {
	f := newWriterTestFrontend(t, 1)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { _ = f.stream.Close() })
	}
	wg.Wait()
	if got := f.conn.closes.Load(); got != 1 {
		t.Fatalf("socket closed %d times, want once", got)
	}
}

func waitWriterMetrics(t *testing.T, full, blocked float64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if promtest.ToFloat64(metrics.Metrics.FullFrontendWriteQueues()) == full &&
			promtest.ToFloat64(metrics.Metrics.BlockedFrontendWriteChannels()) == blocked {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("full queues = %v, blocked dispatchers = %v; want %v, %v",
		promtest.ToFloat64(metrics.Metrics.FullFrontendWriteQueues()),
		promtest.ToFloat64(metrics.Metrics.BlockedFrontendWriteChannels()), full, blocked)
}

func TestHTTPConnectWriterFullQueueMetricCountsConnections(t *testing.T) {
	metrics.Metrics.Reset()
	s := newWriterTestServer(t)
	first := s.establish(t, 1, 1)
	second := s.establish(t, 2, 1)
	for i, f := range []*writerTestFrontend{first, second} {
		id := int64(i + 1)
		s.send(t, dataPkt(id, []byte("blocked")))
		f.waitWrite(t, "blocked")
		s.send(t, dataPkt(id, []byte("queued")))
	}
	waitWriterMetrics(t, 2, 0)
	first.stream.close()
	waitWriterMetrics(t, 1, 0)
	second.readData(t, "blockedqueued")
	waitWriterMetrics(t, 0, 0)
}

func TestHTTPConnectWriterPressureMetricsClearOnDisconnect(t *testing.T) {
	metrics.Metrics.Reset()
	s := newWriterTestServer(t)
	f := s.establish(t, 1, 1)
	s.send(t, dataPkt(1, []byte("blocked")))
	f.waitWrite(t, "blocked")
	s.send(t, dataPkt(1, []byte("queued")))
	s.send(t, dataPkt(1, []byte("overflow")))
	waitWriterMetrics(t, 1, 1)
	s.cancel()
	waitWriterSignal(t, f.stream.writerDone, "writer cancellation")
	waitWriterMetrics(t, 0, 0)
}

func TestHTTPConnectWriterFullQueueMetricAfterConcurrentUpdates(t *testing.T) {
	metrics.Metrics.Reset()
	h := &httpConnectStream{writeCh: make(chan *client.Packet, 1)}
	pkt := dataPkt(1, []byte("data"))
	h.writeCh <- pkt
	h.updateFrontendWriteQueueMetric()
	defer h.stopFrontendWriteQueueMetric()
	consume, produce := make(chan struct{}), make(chan struct{})
	done := make(chan struct{}, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range consume {
			<-h.writeCh
			h.updateFrontendWriteQueueMetric()
			done <- struct{}{}
		}
	})
	wg.Go(func() {
		for range produce {
			h.writeCh <- pkt
			h.updateFrontendWriteQueueMetric()
			done <- struct{}{}
		}
	})
	t.Cleanup(func() { close(consume); close(produce); wg.Wait() })
	for i := range 100000 {
		consume <- struct{}{}
		produce <- struct{}{}
		<-done
		<-done
		// Both updates have finished, so this is not a transient sample:
		// the queue is full and must contribute one to the gauge.
		if got := promtest.ToFloat64(metrics.Metrics.FullFrontendWriteQueues()); got != 1 {
			t.Fatalf("iteration %d: full-queue gauge = %v, want 1", i, got)
		}
	}
}
