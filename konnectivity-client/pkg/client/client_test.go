/*
Copyright 2019 The Kubernetes Authors.

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

package client

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics/testing"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

func TestMain(m *testing.M) {
	fs := flag.NewFlagSet("test", flag.PanicOnError)
	klog.InitFlags(fs)
	fs.Set("v", "9")
	metrics.Metrics.RegisterMetrics(prometheus.DefaultRegisterer)

	m.Run()
}

func TestDial(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	s, ps := pipe()
	ts := testServer(ps, 100)

	defer ps.Close()
	defer s.Close()

	tunnel := newUnstartedTunnel(s, s.conn())

	if err := metricstest.ExpectClientConnection(metrics.ClientConnectionStatusCreated, 1); err != nil {
		t.Error(err)
	}

	go tunnel.serve(ctx)
	go ts.serve()

	_, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("expect nil; got %v", err)
	}

	if ts.packets[0].Type != client.PacketType_DIAL_REQ {
		t.Fatalf("expect packet.type %v; got %v", client.PacketType_DIAL_REQ, ts.packets[0].Type)
	}

	if ts.packets[0].GetDialRequest().Address != "127.0.0.1:80" {
		t.Errorf("expect packet.address %v; got %v", "127.0.0.1:80", ts.packets[0].GetDialRequest().Address)
	}

	if err := metricstest.ExpectClientConnections(map[metrics.ClientConnectionStatus]int{
		metrics.ClientConnectionStatusCreated: 0,
		metrics.ClientConnectionStatusDialing: 0,
		metrics.ClientConnectionStatusOk:      1,
	}); err != nil {
		t.Error(err)
	}
	if err := metricstest.ExpectClientDialFailures(nil); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset() // For clean shutdown.
}

// TestDialRace exercises the scenario where serve() observes and handles DIAL_RSP
// before DialContext() does any work after sending the DIAL_REQ.
func TestDialRace(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	s, ps := pipe()
	ts := testServer(ps, 100)

	defer ps.Close()
	defer s.Close()

	// artificially delay after calling Send, ensure handoff of result from serve to DialContext still works
	slowStream := fakeSlowSend{s}
	tunnel := newUnstartedTunnel(slowStream, &fakeConn{})

	go tunnel.serve(ctx)
	go ts.serve()

	_, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("expect nil; got %v", err)
	}

	if ts.packets[0].Type != client.PacketType_DIAL_REQ {
		t.Fatalf("expect packet.type %v; got %v", client.PacketType_DIAL_REQ, ts.packets[0].Type)
	}

	if ts.packets[0].GetDialRequest().Address != "127.0.0.1:80" {
		t.Errorf("expect packet.address %v; got %v", "127.0.0.1:80", ts.packets[0].GetDialRequest().Address)
	}

	if err := metricstest.ExpectClientDialFailures(nil); err != nil {
		t.Error(err)
	}
}

// fakeSlowSend wraps ProxyService_ProxyClient and adds an artificial 1 second delay after calling Send
type fakeSlowSend struct {
	client.ProxyService_ProxyClient
}

func (s fakeSlowSend) Send(p *client.Packet) error {
	// send the request so it can start being processed immediately
	err := s.ProxyService_ProxyClient.Send(p)
	// delay returning to simulate slowness on the client side,
	// to exercise serve() observing/handling the DIAL_RSP before
	// the client does any post-Send() work
	time.Sleep(time.Second)
	return err
}

func TestAlreadyDialed(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	s, ps := pipe()
	ts := testServer(ps, 100)

	defer ps.Close()
	defer s.Close()

	// artificially delay after calling Send, ensure handoff of result from serve to DialContext still works
	slowStream := fakeSlowSend{s}
	tunnel := newUnstartedTunnel(slowStream, &fakeConn{})

	go tunnel.serve(ctx)
	go ts.serve()

	_, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("expect nil; got %v", err)
	}

	// Duplicate
	_, err = tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatalf("expect duplicate dial error, got nil")
	}

	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureAlreadyStarted, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset() // For clean shutdown.
}

func TestData(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	s, ps := pipe()
	ts := testServer(ps, 100)

	defer ps.Close()
	defer s.Close()

	tunnel := newUnstartedTunnel(s, s.conn())

	go tunnel.serve(ctx)
	go ts.serve()

	conn, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("expect nil; got %v", err)
	}

	datas := [][]byte{
		[]byte("hello"),
		[]byte(", "),
		[]byte("world."),
	}

	// send data using conn.Write
	for _, data := range datas {
		n, err := conn.Write(data)
		if err != nil {
			t.Error(err)
		}
		if n != len(data) {
			t.Errorf("expect n=%d len(%q); got %d", len(data), string(data), n)
		}
	}

	// test server should echo data back
	var buf [64]byte
	for _, data := range datas {
		n, err := conn.Read(buf[:])
		if err != nil {
			t.Error(err)
		}

		if string(buf[:n]) != "echo: "+string(data) {
			t.Errorf("expect 'echo: %s'; got %s", string(data), string(buf[:n]))
		}
	}

	// verify test server received data
	if ts.data.String() != "hello, world." {
		t.Errorf("expect server received %v; got %v", "hello, world.", ts.data.String())
	}
}

func TestClose(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	s, ps := pipe()
	ts := testServer(ps, 100)

	defer ps.Close()
	defer s.Close()

	tunnel := newUnstartedTunnel(s, s.conn())

	go tunnel.serve(ctx)
	go ts.serve()

	conn, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("expect nil; got %v", err)
	}

	if err := metricstest.ExpectClientConnections(map[metrics.ClientConnectionStatus]int{
		metrics.ClientConnectionStatusCreated: 0,
		metrics.ClientConnectionStatusDialing: 0,
		metrics.ClientConnectionStatusOk:      1,
	}); err != nil {
		t.Error(err)
	}

	if err := conn.Close(); err != nil {
		t.Error(err)
	}

	if ts.packets[1].Type != client.PacketType_CLOSE_REQ {
		t.Fatalf("expect packet.type %v; got %v", client.PacketType_CLOSE_REQ, ts.packets[1].Type)
	}
	if ts.packets[1].GetCloseRequest().ConnectID != 100 {
		t.Errorf("expect connectID=100; got %d", ts.packets[1].GetCloseRequest().ConnectID)
	}

	<-tunnel.Done()
	if err := metricstest.ExpectClientConnections(map[metrics.ClientConnectionStatus]int{
		metrics.ClientConnectionStatusCreated: 0,
		metrics.ClientConnectionStatusDialing: 0,
		metrics.ClientConnectionStatusOk:      0,
	}); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset() // For clean shutdown.
}

func TestCloseTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	expectCleanShutdown(t)

	ctx := context.Background()
	s, ps := pipe()
	ts := testServer(ps, 100)

	// sending a nil response for close handler should trigger the timeout
	// since we never receive CLOSE_RSP
	ts.handlers[client.PacketType_CLOSE_REQ] = func(pkt *client.Packet) *client.Packet {
		return nil
	}

	defer ps.Close()
	defer s.Close()

	tunnel := newUnstartedTunnel(s, s.conn())

	go tunnel.serve(ctx)
	go ts.serve()

	conn, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err != nil {
		t.Fatalf("expect nil; got %v", err)
	}

	go func() {
		buf := make([]byte, 10)
		_, err = conn.Read(buf)
		if err != io.EOF {
			t.Errorf("expected %v: got %v", io.EOF, err)
		}
	}()

	if err := conn.Close(); err != errConnCloseTimeout {
		t.Errorf("expected %v but got %v", errConnCloseTimeout, err)
	}

	<-tunnel.Done()
	if err := metricstest.ExpectClientConnections(map[metrics.ClientConnectionStatus]int{
		metrics.ClientConnectionStatusCreated: 0,
		metrics.ClientConnectionStatusDialing: 0,
		metrics.ClientConnectionStatusOk:      0,
	}); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset() // For clean shutdown.
}

func TestCreateSingleUseGrpcTunnel_NoLeakOnFailure(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	tunnel, err := CreateSingleUseGrpcTunnel(context.Background(), "127.0.0.1:12345", grpc.WithInsecure())
	if tunnel != nil {
		t.Fatal("expected nil tunnel when calling CreateSingleUseGrpcTunnel")
	}
	if err == nil {
		t.Fatal("expected error when calling CreateSingleUseGrpcTunnel")
	}
}

func TestCreateSingleUseGrpcTunnelWithContext_NoLeakOnFailure(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	tunnel, err := CreateSingleUseGrpcTunnelWithContext(context.Background(), context.Background(), "127.0.0.1:12345", grpc.WithInsecure())
	if tunnel != nil {
		t.Fatal("expected nil tunnel when calling CreateSingleUseGrpcTunnelWithContext")
	}
	if err == nil {
		t.Fatal("expected error when calling CreateSingleUseGrpcTunnelWithContext")
	}
}

func TestDialAfterTunnelCancelled(t *testing.T) {
	expectCleanShutdown(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, ps := pipeWithContext(ctx)
	ts := testServer(ps, 100)

	defer ps.Close()
	defer s.Close()

	tunnel := newUnstartedTunnel(s, s.conn())

	go tunnel.serve(ctx)
	go ts.serve()

	_, err := tunnel.DialContext(ctx, "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatalf("expect err when dialing after tunnel closed")
	}

	// TODO(jkh52): verify dial failure metric.
	metrics.Metrics.Reset() // For clean shutdown.

	select {
	case <-tunnel.Done():
	case <-time.After(30 * time.Second):
		t.Errorf("Timed out waiting for tunnel to close")
	}
}

func TestDial_RequestContextCancelled(t *testing.T) {
	expectCleanShutdown(t)

	s, ps := pipe()
	defer ps.Close()
	defer s.Close()

	ts := testServer(ps, 100)
	reqCtx, reqCancel := context.WithCancel(context.Background())
	ts.handlers[client.PacketType_DIAL_REQ] = func(*client.Packet) *client.Packet {
		reqCancel()
		return nil // don't respond
	}
	closeCh := make(chan struct{})
	ts.handlers[client.PacketType_DIAL_CLS] = func(*client.Packet) *client.Packet {
		close(closeCh)
		return nil // don't respond
	}
	go ts.serve()

	func() {
		tunnel := newUnstartedTunnel(s, s.conn())
		go tunnel.serve(context.Background())

		_, err := tunnel.DialContext(reqCtx, "tcp", "127.0.0.1:80")
		if err == nil {
			t.Fatalf("Expected dial error, got none")
		}

		isDialFailure, reason := GetDialFailureReason(err)
		if !isDialFailure {
			t.Errorf("Unexpected non-dial failure error: %v", err)
		} else if reason != metrics.DialFailureContext {
			t.Errorf("Expected DialFailureContext, got %v", reason)
		}

		if err := metricstest.ExpectClientDialFailure(metrics.DialFailureContext, 1); err != nil {
			t.Error(err)
		}
		metrics.Metrics.Reset() // For clean shutdown.

		ts.assertPacketType(0, client.PacketType_DIAL_REQ)
		waitForDialClsStart := time.Now()
		select {
		case <-closeCh:
			t.Logf("Dial closed after %#v", time.Since(waitForDialClsStart).String())
			ts.assertPacketType(1, client.PacketType_DIAL_CLS)
		case <-time.After(30 * time.Second):
			t.Fatal("Timed out waiting for DIAL_CLS packet")
		}

		waitForTunnelCloseStart := time.Now()
		select {
		case <-tunnel.Done():
			t.Logf("Tunnel closed after %#v", time.Since(waitForTunnelCloseStart).String())
		case <-time.After(30 * time.Second):
			t.Errorf("Timed out waiting for tunnel to close")
		}
	}()
}

func TestDial_BackendError(t *testing.T) {
	expectCleanShutdown(t)

	s, ps := pipe()
	ts := testServer(ps, 100)
	ts.handlers[client.PacketType_DIAL_REQ] = func(pkt *client.Packet) *client.Packet {
		return &client.Packet{
			Type: client.PacketType_DIAL_RSP,
			Payload: &client.Packet_DialResponse{
				DialResponse: &client.DialResponse{
					Random: pkt.GetDialRequest().Random,
					Error:  "fake backend error",
				},
			},
		}
	}

	defer ps.Close()
	defer s.Close()

	tunnel := newUnstartedTunnel(s, s.conn())

	go tunnel.serve(context.Background())
	go ts.serve()

	_, err := tunnel.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatalf("Expected dial error, got none")
	}

	isDialFailure, reason := GetDialFailureReason(err)
	if !isDialFailure {
		t.Errorf("Unexpected non-dial failure error: %v", err)
	} else if reason != metrics.DialFailureEndpoint {
		t.Errorf("Expected DialFailureEndpoint, got %v", reason)
	}

	ts.assertPacketType(0, client.PacketType_DIAL_REQ)

	if err := metricstest.ExpectClientDialFailure(metrics.DialFailureEndpoint, 1); err != nil {
		t.Error(err)
	}
	metrics.Metrics.Reset() // For clean shutdown.
}

func TestDial_Closed(t *testing.T) {
	expectCleanShutdown(t)

	s, ps := pipe()
	defer ps.Close()
	defer s.Close()

	ts := testServer(ps, 100)
	ts.handlers[client.PacketType_DIAL_REQ] = func(pkt *client.Packet) *client.Packet {
		return &client.Packet{
			Type: client.PacketType_DIAL_CLS,
			Payload: &client.Packet_CloseDial{
				CloseDial: &client.CloseDial{
					Random: pkt.GetDialRequest().Random,
				},
			},
		}
	}
	go ts.serve()

	func() {
		// Verify that the tunnel goroutines are not leaked before cleaning up the test server.
		goleak.VerifyNone(t, goleak.IgnoreCurrent())

		tunnel := newUnstartedTunnel(s, s.conn())
		go tunnel.serve(context.Background())

		_, err := tunnel.DialContext(context.Background(), "tcp", "127.0.0.1:80")
		if err == nil {
			t.Fatalf("Expected dial error, got none")
		}

		isDialFailure, reason := GetDialFailureReason(err)
		if !isDialFailure {
			t.Errorf("Unexpected non-dial failure error: %v", err)
		} else if reason != metrics.DialFailureDialClosed {
			t.Errorf("Expected DialFailureDialClosed, got %v", reason)
		}

		ts.assertPacketType(0, client.PacketType_DIAL_REQ)

		if err := metricstest.ExpectClientDialFailure(metrics.DialFailureDialClosed, 1); err != nil {
			t.Error(err)
		}
		metrics.Metrics.Reset() // For clean shutdown.

		select {
		case <-tunnel.Done():
		case <-time.After(30 * time.Second):
			t.Errorf("Timed out waiting for tunnel to close")
		}
	}()
}

func TestRegisterMetrics(t *testing.T) {
	Metrics.RegisterMetrics(prometheus.DefaultRegisterer)
}

func TestLegacyRegisterMetrics(t *testing.T) {
	Metrics.LegacyRegisterMetrics(prometheus.MustRegister)
}

// TODO: Move to common testing library

// fakeStream implements ProxyService_ProxyClient
type fakeStream struct {
	grpc.ClientStream
	r      <-chan *client.Packet
	w      chan<- *client.Packet
	done   <-chan struct{}
	closed chan struct{}
}

type fakeConn struct {
	stream *fakeStream
}

func (f *fakeConn) Close() error {
	if f.stream != nil {
		f.stream.Close()
	}
	return nil
}

var _ clientConn = &fakeConn{}

var _ client.ProxyService_ProxyClient = &fakeStream{}

func pipe() (*fakeStream, *fakeStream) {
	return pipeWithContext(context.Background())
}

func pipeWithContext(context context.Context) (*fakeStream, *fakeStream) {
	r, w := make(chan *client.Packet, 2), make(chan *client.Packet, 2)
	s1 := &fakeStream{done: context.Done(), closed: make(chan struct{})}
	s2 := &fakeStream{done: context.Done(), closed: make(chan struct{})}
	s1.r, s1.w = r, w
	s2.r, s2.w = w, r
	return s1, s2
}

func (s *fakeStream) Send(packet *client.Packet) error {
	klog.V(4).InfoS("[DEBUG] send", "packet", packet)
	if packet == nil {
		return nil
	}
	select {
	case <-s.done:
		return errors.New("Send on cancelled stream")
	case <-s.closed:
		return errors.New("Send on closed stream")
	case s.w <- packet:
		return nil
	}
}

func (s *fakeStream) Recv() (*client.Packet, error) {
	select {
	case <-s.done:
		return nil, errors.New("Recv on cancelled stream")
	case <-s.closed:
		return nil, errors.New("Recv on closed stream")
	case pkt := <-s.r:
		klog.V(4).InfoS("[DEBUG] recv", "packet", pkt)
		return pkt, nil
	case <-time.After(30 * time.Second):
		return nil, errors.New("timeout recv")
	}
}

func (s *fakeStream) Close() {
	select {
	case <-s.closed: // Avoid double-closing
	default:
		close(s.closed)
	}
}

func (s *fakeStream) conn() *fakeConn {
	return &fakeConn{s}
}

type proxyServer struct {
	t        testing.T
	s        client.ProxyService_ProxyClient
	handlers map[client.PacketType]handler
	connid   int64
	data     bytes.Buffer

	packets     []*client.Packet
	packetsLock sync.Mutex
}

func testServer(s client.ProxyService_ProxyClient, connid int64) *proxyServer {
	server := &proxyServer{
		s:        s,
		connid:   connid,
		handlers: make(map[client.PacketType]handler),
		packets:  []*client.Packet{},
	}

	server.handlers[client.PacketType_CLOSE_REQ] = server.handleClose
	server.handlers[client.PacketType_DIAL_REQ] = server.handleDial
	server.handlers[client.PacketType_DATA] = server.handleData

	return server
}

func (s *proxyServer) serve() {
	for {
		pkt, err := s.s.Recv()
		if err != nil {
			s.t.Error(err)
			return
		}

		if pkt == nil {
			return
		}

		func() {
			s.packetsLock.Lock()
			defer s.packetsLock.Unlock()
			s.packets = append(s.packets, pkt)
		}()

		if handler, ok := s.handlers[pkt.Type]; ok {
			req := handler(pkt)
			if req != nil {
				if err := s.s.Send(req); err != nil {
					s.t.Error(err)
				}
			}
		}
	}
}

func (s *proxyServer) assertPacketType(index int, expectedType client.PacketType) {
	s.packetsLock.Lock()
	defer s.packetsLock.Unlock()

	if index >= len(s.packets) {
		s.t.Fatalf("Expected %v packet[%d], but have only received %d packets", expectedType, index, len(s.packets))
	}
	actual := s.packets[index].Type
	if actual != expectedType {
		s.t.Errorf("Unexpected packet[%d].type: got %v, expected %v", index, actual, expectedType)
	}
}

type handler func(pkt *client.Packet) *client.Packet

func (s *proxyServer) handleDial(pkt *client.Packet) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{
				Random:    pkt.GetDialRequest().Random,
				ConnectID: s.connid,
			},
		},
	}
}

func (s *proxyServer) handleClose(pkt *client.Packet) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{
				ConnectID: pkt.GetCloseRequest().ConnectID,
			},
		},
	}
}

func (s *proxyServer) handleData(pkt *client.Packet) *client.Packet {
	s.data.Write(pkt.GetData().Data)

	return &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: pkt.GetData().ConnectID,
				Data:      append([]byte("echo: "), pkt.GetData().Data...),
			},
		},
	}
}

func assertNoClientDialFailures(t testing.TB) {
	t.Helper()
	if err := metricstest.ExpectClientDialFailures(nil); err != nil {
		t.Errorf("Unexpected %s metric: %v", "dial_failure_total", err)
	}
}

func expectCleanShutdown(t testing.TB) {
	metrics.Metrics.Reset()
	currentGoRoutines := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		goleak.VerifyNone(t, currentGoRoutines)
		assertNoClientDialFailures(t)
		// The tunnels gauge metric is intentionally not included here, to avoid
		// brittle gauge assertions. Tests should directly verify it.
		metrics.Metrics.Reset()
	})
}
