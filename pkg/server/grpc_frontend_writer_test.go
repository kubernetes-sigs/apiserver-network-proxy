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
	"context"
	"fmt"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/grpc/metadata"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

// grpcTestFrontend is a gRPC frontend stream whose Send blocks until the test
// reads the packet.
type grpcTestFrontend struct {
	client.ProxyService_ProxyServer

	ctx    context.Context
	cancel context.CancelFunc
	recv   chan *client.Packet
	sent   chan *client.Packet
	// done is closed when ProxyServer.Proxy returns.
	done chan struct{}
}

func (f *grpcTestFrontend) Context() context.Context { return f.ctx }

func (f *grpcTestFrontend) Recv() (*client.Packet, error) {
	select {
	case pkt, ok := <-f.recv:
		if !ok {
			return nil, io.EOF
		}
		return pkt, nil
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	}
}

func (f *grpcTestFrontend) Send(pkt *client.Packet) error {
	select {
	case f.sent <- pkt:
		return nil
	case <-f.ctx.Done():
		return f.ctx.Err()
	}
}

func (f *grpcTestFrontend) expect(t *testing.T, typ client.PacketType, data string) {
	t.Helper()
	select {
	case pkt := <-f.sent:
		if pkt.Type != typ || string(pkt.GetData().GetData()) != data {
			t.Fatalf("frontend received %v %q, want %v %q", pkt.Type, pkt.GetData().GetData(), typ, data)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("frontend did not receive %v %q", typ, data)
	}
}

func newGRPCWriterTestServer(t *testing.T, queueSize int) *writerTestServer {
	t.Helper()
	metrics.Metrics.Reset()
	s := newWriterTestServer(t)
	s.proxy.SetFrontendWriteChannelSize(queueSize)
	s.proxy.addBackend(s.backend)
	return s
}

// establishGRPC serves a gRPC frontend with ProxyServer.Proxy and completes
// its dial with connection ID id.
func (s *writerTestServer) establishGRPC(t *testing.T, id int64) *grpcTestFrontend {
	t.Helper()
	f := s.establishGRPCPendingDial(t, id)
	s.send(t, dialRspPkt(id, id))
	f.expect(t, client.PacketType_DIAL_RSP, "")
	return f
}

// establishGRPCPendingDial serves a gRPC frontend with ProxyServer.Proxy and
// waits for its dial id to reach the agent, without answering it.
func (s *writerTestServer) establishGRPCPendingDial(t *testing.T, id int64) *grpcTestFrontend {
	t.Helper()
	ctx, cancel := context.WithCancel(metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(header.UserAgent, "test")))
	f := &grpcTestFrontend{
		ctx:    ctx,
		cancel: cancel,
		recv:   make(chan *client.Packet, 1),
		sent:   make(chan *client.Packet),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(f.done)
		_ = s.proxy.Proxy(f)
	}()
	t.Cleanup(func() {
		cancel()
		waitWriterSignal(t, f.done, "gRPC frontend handler shutdown")
	})

	f.recv <- dialReqPkt(id)
	for {
		select {
		case pkt := <-s.toAgent:
			if pkt.GetDialRequest().GetRandom() != id {
				continue
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("DIAL_REQ %d did not reach the agent", id)
		}
		break
	}
	return f
}

func (f *grpcTestFrontend) expectDialFailure(t *testing.T) {
	t.Helper()
	select {
	case pkt := <-f.sent:
		if pkt.Type != client.PacketType_DIAL_RSP || pkt.GetDialResponse().GetError() == "" {
			t.Fatalf("frontend received %v, want a failed DIAL_RSP", pkt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frontend did not receive a failed DIAL_RSP")
	}
}

func TestGRPCFrontendWriteChannelSize(t *testing.T) {
	for _, tc := range []struct {
		queueSize int
		wantCap   int // 0 means unqueued
	}{
		{queueSize: 0, wantCap: 0},
		{queueSize: -1, wantCap: defaultFrontendWriteChannelSize},
		{queueSize: 3, wantCap: 3},
	} {
		t.Run(fmt.Sprintf("queue=%d", tc.queueSize), func(t *testing.T) {
			s := newGRPCWriterTestServer(t, tc.queueSize)
			f := s.establishGRPC(t, 1)
			conn, err := s.proxy.getFrontend("agent", 1)
			if err != nil {
				t.Fatal(err)
			}
			queued, ok := conn.frontend.stream.(*grpcFrontendStream)
			switch {
			case tc.wantCap == 0 && ok:
				t.Fatal("zero queue size queued the gRPC frontend")
			case tc.wantCap == 0 && conn.frontend.stream != f:
				t.Fatalf("frontend stream = %T, want the gRPC stream", conn.frontend.stream)
			case tc.wantCap != 0 && !ok:
				t.Fatalf("frontend stream = %T, want queued", conn.frontend.stream)
			case tc.wantCap != 0 && cap(queued.writeCh) != tc.wantCap:
				t.Fatalf("capacity = %d, want %d", cap(queued.writeCh), tc.wantCap)
			}
		})
	}
}

func TestGRPCFrontendWriterDelaysHOLByQueueDepth(t *testing.T) {
	const depth = 3
	s := newGRPCWriterTestServer(t, depth)
	slow := s.establishGRPC(t, 1)
	healthy := s.establishGRPC(t, 2)

	// The writer takes slow-0 and blocks sending it. The rest fill the queue,
	// while the other frontend keeps receiving.
	var want []string
	for i := 0; i <= depth; i++ {
		payload := fmt.Sprintf("slow-%d", i)
		want = append(want, payload)
		s.send(t, dataPkt(1, []byte(payload)))
		s.send(t, dataPkt(2, []byte("healthy")))
		healthy.expect(t, client.PacketType_DATA, "healthy")
	}
	waitWriterMetrics(t, 1, 0)

	want = append(want, "overflow")
	s.send(t, dataPkt(1, []byte("overflow")))
	waitWriterMetrics(t, 1, 1)
	resumed := make(chan struct{})
	go func() {
		defer close(resumed)
		s.packets <- dataPkt(2, []byte("resumed"))
	}()
	for _, payload := range want {
		slow.expect(t, client.PacketType_DATA, payload)
	}
	waitWriterSignal(t, resumed, "dispatch after the slow frontend drained")
	healthy.expect(t, client.PacketType_DATA, "resumed")
	waitWriterMetrics(t, 0, 0)
}

func TestGRPCFrontendWriterDrainsOnHandlerExit(t *testing.T) {
	s := newGRPCWriterTestServer(t, 2)
	f := s.establishGRPC(t, 1)
	s.send(t, dataPkt(1, []byte("first")))
	s.send(t, dataPkt(1, []byte("second")))
	s.send(t, closeRspPkt(1, ""))
	s.send(t, &client.Packet{Type: client.PacketType_DRAIN}) // flush dispatch, not the writer

	close(f.recv) // Recv returns EOF, so Proxy returns with packets still queued.
	f.expect(t, client.PacketType_DATA, "first")
	f.expect(t, client.PacketType_DATA, "second")
	f.expect(t, client.PacketType_CLOSE_RSP, "")
	waitWriterSignal(t, f.done, "gRPC frontend handler shutdown")
}

func TestGRPCFrontendWriterCancelUnblocksDispatch(t *testing.T) {
	s := newGRPCWriterTestServer(t, 1)
	slow := s.establishGRPC(t, 1)
	healthy := s.establishGRPC(t, 2)
	for _, payload := range []string{"blocked", "queued", "overflow"} {
		s.send(t, dataPkt(1, []byte(payload)))
	}
	waitWriterMetrics(t, 1, 1)

	slow.cancel()
	waitWriterSignal(t, slow.done, "gRPC frontend handler shutdown")
	s.send(t, dataPkt(2, []byte("healthy")))
	healthy.expect(t, client.PacketType_DATA, "healthy")
	waitWriterMetrics(t, 0, 0)
}

// sendBlockingFrontend is a gRPC frontend stream whose Send ignores its
// context and blocks until release is closed.
type sendBlockingFrontend struct {
	client.ProxyService_ProxyServer

	ctx     context.Context
	release chan struct{}
}

func (f *sendBlockingFrontend) Context() context.Context { return f.ctx }

func (f *sendBlockingFrontend) Send(*client.Packet) error {
	<-f.release
	return nil
}

func TestGRPCFrontendDrainWaitsForSendInProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f := &sendBlockingFrontend{ctx: ctx, release: make(chan struct{})}
		g := newGRPCFrontendStream(f, 1)
		if err := g.Send(dataPkt(1, []byte("blocked"))); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the writer is in f.Send

		cancel()
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			g.drain()
		}()
		synctest.Wait()
		select {
		case <-drained:
			t.Fatal("drain returned while the writer was in Send")
		default:
		}
		close(f.release)
		<-drained
	})
}

func TestGRPCFrontendWriterBackendDisconnect(t *testing.T) {
	for _, cleanEOF := range []bool{false, true} {
		t.Run(fmt.Sprintf("clean-eof=%t", cleanEOF), func(t *testing.T) {
			s := newGRPCWriterTestServer(t, 1)
			slow := s.establishGRPC(t, 1)
			healthy := s.establishGRPC(t, 2)

			// A dial still waiting for its DIAL_RSP on this agent.
			pending := s.establishGRPCPendingDial(t, 3)

			s.send(t, dataPkt(1, []byte("blocked")))
			s.send(t, dataPkt(1, []byte("queued")))
			overflowSent := make(chan struct{})
			go func() {
				defer close(overflowSent)
				s.packets <- dataPkt(1, []byte("overflow"))
			}()
			waitWriterSignal(t, overflowSent, "dispatch of the overflow packet")
			waitWriterMetrics(t, 1, 1)

			// The agent goes away while the receive loop is blocked on the
			// slow frontend's full queue.
			if cleanEOF {
				s.backend.receivedEOF.Store(true)
			}
			s.cancel()
			packets := s.packets
			s.packets = make(chan *client.Packet)
			close(packets)

			want := []string{"blocked", "queued"}
			if cleanEOF {
				// After a clean EOF the agent's last packets are delivered,
				// so the loop waits for the slow frontend to read.
				want = append(want, "overflow")
			} else {
				// Otherwise the blocked packet is dropped and the cleanup
				// runs while the slow frontend is still not reading.
				waitWriterSignal(t, s.done, "receive loop exit on agent disconnect")
				healthy.expect(t, client.PacketType_CLOSE_RSP, "")
				pending.expectDialFailure(t)
			}
			for _, payload := range want {
				slow.expect(t, client.PacketType_DATA, payload)
			}
			slow.expect(t, client.PacketType_CLOSE_RSP, "")
			if cleanEOF {
				waitWriterSignal(t, s.done, "receive loop exit after clean EOF")
				healthy.expect(t, client.PacketType_CLOSE_RSP, "")
				pending.expectDialFailure(t)
			}
			waitWriterMetrics(t, 0, 0)
		})
	}
}
