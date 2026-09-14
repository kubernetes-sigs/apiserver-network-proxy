/*
Copyright 2025 The Kubernetes Authors.

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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
	agentmock "sigs.k8s.io/apiserver-network-proxy/proto/agent/mocks"
)

// httpConnectFixture drives a real HTTP CONNECT request through the Tunnel
// handler, so that the adapter is exercised by ProxyServer.Proxy exactly as it
// is in production.
type httpConnectFixture struct {
	proxyServer *ProxyServer
	agentConn   *agentmock.MockAgentService_ConnectServer
	backend     *Backend
	// toAgent receives every packet the server sends to the agent.
	toAgent chan *client.Packet
	// fromAgent is the agent's send queue, consumed by the mocked Recv.
	fromAgent chan *client.Packet
	conn      net.Conn
}

func newHTTPConnectFixture(t *testing.T, ctrl *gomock.Controller, host string) *httpConnectFixture {
	t.Helper()

	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	agentConn, backend := prepareAgentConnMD(t, ctrl, proxyServer, nil)

	f := &httpConnectFixture{
		proxyServer: proxyServer,
		agentConn:   agentConn,
		backend:     backend,
		toAgent:     make(chan *client.Packet, 16),
		fromAgent:   make(chan *client.Packet, 16),
	}

	agentConn.EXPECT().SendHeader(gomock.Any()).Return(nil).AnyTimes()
	agentConn.EXPECT().Send(gomock.Any()).DoAndReturn(func(pkt *client.Packet) error {
		f.toAgent <- pkt
		return nil
	}).AnyTimes()
	agentConn.EXPECT().Recv().DoAndReturn(func() (*client.Packet, error) {
		pkt, ok := <-f.fromAgent
		if !ok {
			return nil, io.EOF
		}
		return pkt, nil
	}).AnyTimes()

	front := httptest.NewServer(&Tunnel{Server: proxyServer})
	t.Cleanup(front.Close)

	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("failed to parse front URL: %v", err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatalf("failed to connect to HTTP CONNECT front: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	f.conn = conn

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host); err != nil {
		t.Fatalf("failed to write CONNECT request: %v", err)
	}
	return f
}

// serveAgent runs the agent side of the connection for the duration of the test.
func (f *httpConnectFixture) serveAgent(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.proxyServer.Connect(f.agentConn)
	}()
	t.Cleanup(func() {
		close(f.fromAgent)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for Connect to return")
		}
	})
}

func (f *httpConnectFixture) nextAgentPacket(t *testing.T) *client.Packet {
	t.Helper()
	select {
	case pkt := <-f.toAgent:
		return pkt
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a packet to the agent")
		return nil
	}
}

// TestHTTPConnectTunnelUsesGrpcPacketHandling verifies that an HTTP CONNECT
// frontend is served by the same DIAL_REQ/DIAL_RSP/DATA/CLOSE_RSP packet
// handling as a gRPC frontend.
func TestHTTPConnectTunnelUsesGrpcPacketHandling(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const host = "127.0.0.1:8080"
	const connectID = 4242

	f := newHTTPConnectFixture(t, ctrl, host)
	f.serveAgent(t)

	// The CONNECT request is presented to the server as a DIAL_REQ.
	dialReq := f.nextAgentPacket(t)
	if dialReq.Type != client.PacketType_DIAL_REQ {
		t.Fatalf("expected DIAL_REQ to agent, got %v", dialReq.Type)
	}
	if got := dialReq.GetDialRequest().Address; got != host {
		t.Errorf("expected dial address %q, got %q", host, got)
	}
	if got := dialReq.GetDialRequest().Protocol; got != "tcp" {
		t.Errorf("expected dial protocol %q, got %q", "tcp", got)
	}
	dialID := dialReq.GetDialRequest().Random

	// The agent answers, which becomes the CONNECT response.
	f.fromAgent <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{Random: dialID, ConnectID: connectID},
		},
	}

	br := bufio.NewReader(f.conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("failed to read CONNECT response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for established tunnel, got %s", resp.Status)
	}

	// Bytes written by the client become DATA packets carrying the connection
	// ID the agent assigned.
	if _, err := f.conn.Write([]byte("hello agent")); err != nil {
		t.Fatalf("failed to write tunnel bytes: %v", err)
	}
	data := f.nextAgentPacket(t)
	if data.Type != client.PacketType_DATA {
		t.Fatalf("expected DATA to agent, got %v", data)
	}
	if got := data.GetData().ConnectID; got != connectID {
		t.Errorf("expected connectID %d on tunnel data, got %d", connectID, got)
	}
	if got := string(data.GetData().Data); got != "hello agent" {
		t.Errorf("expected tunnel data %q, got %q", "hello agent", got)
	}

	// DATA from the agent is written back to the client as raw bytes.
	f.fromAgent <- dataPkt(connectID, []byte("hello client"))
	readBuf := make([]byte, len("hello client"))
	if err := f.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	if _, err := io.ReadFull(br, readBuf); err != nil {
		t.Fatalf("failed to read tunnel bytes from agent: %v", err)
	}
	if got := string(readBuf); got != "hello client" {
		t.Errorf("expected %q back from agent, got %q", "hello client", got)
	}

	// CLOSE_RSP tears the hijacked connection down.
	f.fromAgent <- &client.Packet{
		Type: client.PacketType_CLOSE_RSP,
		Payload: &client.Packet_CloseResponse{
			CloseResponse: &client.CloseResponse{ConnectID: connectID},
		},
	}
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("unexpected error draining closed tunnel: %v", err)
	}
}

// TestHTTPConnectTunnelFrontendCloseClosesBackend verifies that a client
// hanging up produces the CLOSE_REQ the gRPC path sends on frontend shutdown.
func TestHTTPConnectTunnelFrontendCloseClosesBackend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const connectID = 77

	f := newHTTPConnectFixture(t, ctrl, "127.0.0.1:8080")
	f.serveAgent(t)

	dialReq := f.nextAgentPacket(t)
	if dialReq.Type != client.PacketType_DIAL_REQ {
		t.Fatalf("expected DIAL_REQ to agent, got %v", dialReq.Type)
	}
	f.fromAgent <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{Random: dialReq.GetDialRequest().Random, ConnectID: connectID},
		},
	}

	br := bufio.NewReader(f.conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("failed to read CONNECT response: %v", err)
	}
	resp.Body.Close()

	f.conn.Close()

	closeReq := f.nextAgentPacket(t)
	if closeReq.Type != client.PacketType_CLOSE_REQ {
		t.Fatalf("expected CLOSE_REQ to agent after frontend close, got %v", closeReq.Type)
	}
	if got := closeReq.GetCloseRequest().ConnectID; got != connectID {
		t.Errorf("expected CLOSE_REQ for connectID %d, got %d", connectID, got)
	}
}

// TestHTTPConnectTunnelDialErrorBecomesHTTPResponse verifies that a failed dial
// reaches the client as an HTTP error rather than an established tunnel.
func TestHTTPConnectTunnelDialErrorBecomesHTTPResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	f := newHTTPConnectFixture(t, ctrl, "127.0.0.1:8080")
	f.serveAgent(t)

	dialReq := f.nextAgentPacket(t)
	if dialReq.Type != client.PacketType_DIAL_REQ {
		t.Fatalf("expected DIAL_REQ to agent, got %v", dialReq.Type)
	}
	f.fromAgent <- &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{
				Random: dialReq.GetDialRequest().Random,
				Error:  "dial tcp 127.0.0.1:8080: connect: connection refused",
			},
		},
	}

	resp, err := http.ReadResponse(bufio.NewReader(f.conn), nil)
	if err != nil {
		t.Fatalf("failed to read CONNECT response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected %d for refused dial, got %s", http.StatusBadGateway, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read error body: %v", err)
	}
	if got := string(body); got != "dial tcp 127.0.0.1:8080: connect: connection refused" {
		t.Errorf("expected dial error in body, got %q", got)
	}
}

// TestHTTPConnectTunnelNoBackend verifies the dial failure the frontend packet
// handling raises when no agent is registered at all.
func TestHTTPConnectTunnelNoBackend(t *testing.T) {
	proxyServer := NewProxyServer(uuid.New().String(), []proxystrategies.ProxyStrategy{proxystrategies.ProxyStrategyDefault}, 1, &AgentTokenAuthenticationOptions{}, xfrChannelSize)
	front := httptest.NewServer(&Tunnel{Server: proxyServer})
	defer front.Close()

	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("failed to parse front URL: %v", err)
	}
	conn, err := net.Dial("tcp", frontURL.Host)
	if err != nil {
		t.Fatalf("failed to connect to HTTP CONNECT front: %v", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "CONNECT 127.0.0.1:8080 HTTP/1.1\r\nHost: 127.0.0.1:8080\r\n\r\n"); err != nil {
		t.Fatalf("failed to write CONNECT request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("failed to read CONNECT response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected %d when no agent is available, got %s", http.StatusServiceUnavailable, resp.Status)
	}
}

func TestMapDialErrorToHTTPStatus(t *testing.T) {
	testCases := []struct {
		dialErr string
		want    int
	}{
		{dialErr: (&ErrNotFound{}).Error(), want: http.StatusServiceUnavailable},
		{dialErr: errBackendDialTimeout.Error(), want: http.StatusGatewayTimeout},
		{dialErr: "dial tcp 10.0.0.1:443: i/o timeout", want: http.StatusGatewayTimeout},
		{dialErr: "socket: too many open files", want: http.StatusServiceUnavailable},
		{dialErr: "dial tcp 10.0.0.1:443: connect: connection refused", want: http.StatusBadGateway},
		{dialErr: "lookup nosuchhost: no such host", want: http.StatusBadGateway},
		{dialErr: "something unexpected", want: http.StatusBadGateway},
	}
	for _, tc := range testCases {
		t.Run(tc.dialErr, func(t *testing.T) {
			if got := mapDialErrorToHTTPStatus(tc.dialErr); got != tc.want {
				t.Errorf("mapDialErrorToHTTPStatus(%q) = %d, want %d", tc.dialErr, got, tc.want)
			}
		})
	}
}
