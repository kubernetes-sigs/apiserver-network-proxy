/*
Copyright 2022 The Kubernetes Authors.

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

package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

func TestServeData_HTTP(t *testing.T) {
	var err error
	var stream agent.AgentService_ConnectClient
	stopCh := make(chan struct{})
	cs := &ClientSet{
		clients: make(map[string]*Client),
		stopCh:  stopCh,
	}
	testClient := &Client{
		connManager: newConnectionManager(),
		stopCh:      stopCh,
		cs:          cs,
	}
	testClient.stream, stream = pipe()

	// Start agent
	go testClient.Serve()
	defer close(stopCh)

	// Start test http server as remote service
	expectedBody := "Hello, client"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, expectedBody)
	}))
	defer ts.Close()

	// Simulate sending KAS DIAL_REQ to (Agent) Client
	dialPacket := newDialPacket("tcp", ts.URL[len("http://"):], 111)
	err = stream.Send(dialPacket)
	if err != nil {
		t.Fatal(err.Error())
	}

	// Expect receiving DIAL_RSP packet from (Agent) Client
	pkt, err := stream.Recv()
	if err != nil {
		t.Fatal(err.Error())
	}
	if pkt == nil {
		t.Fatal("unexpected nil packet")
	}
	if pkt.Type != client.PacketType_DIAL_RSP {
		t.Errorf("expect PacketType_DIAL_RSP; got %v", pkt.Type)
	}
	dialRsp := pkt.Payload.(*client.Packet_DialResponse)
	connID := dialRsp.DialResponse.ConnectID
	if dialRsp.DialResponse.Random != 111 {
		t.Errorf("expect random=111; got %v", dialRsp.DialResponse.Random)
	}

	// Send Data (HTTP Request) via (Agent) Client to the test http server
	dataPacket := newDataPacket(connID, []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	err = stream.Send(dataPacket)
	if err != nil {
		t.Error(err.Error())
	}

	// Expect receiving http response via (Agent) Client
	pkt, _ = stream.Recv()
	if pkt == nil {
		t.Fatal("unexpected nil packet")
	}
	if pkt.Type != client.PacketType_DATA {
		t.Errorf("expect PacketType_DATA; got %v", pkt.Type)
	}
	data := pkt.Payload.(*client.Packet_Data).Data.Data

	// Verify response data
	//
	// HTTP/1.1 200 OK\r\n
	// Date: Tue, 07 May 2019 06:44:57 GMT\r\n
	// Content-Length: 14\r\n
	// Content-Type: text/plain; charset=utf-8\r\n
	// \r\n
	// Hello, client
	headAndBody := strings.Split(string(data), "\r\n")
	if body := headAndBody[len(headAndBody)-1]; body != expectedBody {
		t.Errorf("expect body %v; got %v", expectedBody, body)
	}

	// Force close the test server which will cause remote connection gets droped
	ts.Close()

	// Verify receiving CLOSE_RSP
	pkt, _ = stream.Recv()
	if pkt == nil {
		t.Fatal("unexpected nil packet")
	}
	if pkt.Type != client.PacketType_CLOSE_RSP {
		t.Errorf("expect PacketType_CLOSE_RSP; got %v", pkt.Type)
	}
	closeErr := pkt.Payload.(*client.Packet_CloseResponse).CloseResponse.Error
	if closeErr != "" {
		t.Errorf("expect nil closeErr; got %v", closeErr)
	}

	// Verify internal state is consistent
	waitForConnectionDeletion(t, testClient, connID)
}

func TestClose_Client(t *testing.T) {
	var stream agent.AgentService_ConnectClient
	stopCh := make(chan struct{})
	cs := &ClientSet{
		clients: make(map[string]*Client),
		stopCh:  stopCh,
	}
	testClient := &Client{
		connManager: newConnectionManager(),
		stopCh:      stopCh,
		cs:          cs,
	}
	testClient.stream, stream = pipe()

	// Start agent
	go testClient.Serve()
	defer close(stopCh)

	// Start test http server as remote service
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "hello, world")
	}))
	defer ts.Close()

	// Simulate sending KAS DIAL_REQ to (Agent) Client
	dialPacket := newDialPacket("tcp", ts.URL[len("http://"):], 111)
	err := stream.Send(dialPacket)
	if err != nil {
		t.Fatal(err)
	}

	// Expect receiving DIAL_RSP packet from (Agent) Client
	pkt, _ := stream.Recv()
	if pkt == nil {
		t.Fatal("unexpected nil packet")
	}
	if pkt.Type != client.PacketType_DIAL_RSP {
		t.Errorf("expect PacketType_DIAL_RSP; got %v", pkt.Type)
	}
	dialRsp := pkt.Payload.(*client.Packet_DialResponse)
	connID := dialRsp.DialResponse.ConnectID
	if dialRsp.DialResponse.Random != 111 {
		t.Errorf("expect random=111; got %v", dialRsp.DialResponse.Random)
	}

	closePacket := newClosePacket(connID)
	if err := stream.Send(closePacket); err != nil {
		t.Fatal(err)
	}

	// Expect receiving close response via (Agent) Client
	pkt, _ = stream.Recv()
	if pkt == nil {
		t.Error("unexpected nil packet")
	}
	if pkt.Type != client.PacketType_CLOSE_RSP {
		t.Errorf("expect PacketType_CLOSE_RSP; got %v", pkt.Type)
	}
	closeErr := pkt.Payload.(*client.Packet_CloseResponse).CloseResponse.Error
	if closeErr != "" {
		t.Errorf("expect nil closeErr; got %v", closeErr)
	}

	// Verify internal state is consistent
	// connID will be removed when remoteToProxy on another goroutine exits.
	// Wait a short period to see if the connection gets cleaned up.
	waitForConnectionDeletion(t, testClient, connID)

	// Verify remote conn is closed
	if err := stream.Send(closePacket); err != nil {
		t.Fatal(err)
	}

	// Expect receiving close response via (Agent) Client
	pkt, _ = stream.Recv()
	if pkt == nil {
		t.Error("unexpected nil packet")
	}
	if pkt.Type != client.PacketType_CLOSE_RSP {
		t.Errorf("expect PacketType_CLOSE_RSP; got %+v", pkt)
	}
	closeErr = pkt.Payload.(*client.Packet_CloseResponse).CloseResponse.Error
	if closeErr != "Unknown connectID" {
		t.Errorf("expect Unknown connectID; got %v", closeErr)
	}

}

func TestConnectionMismatch(t *testing.T) {
	var stream agent.AgentService_ConnectClient
	stopCh := make(chan struct{})
	cs := &ClientSet{
		clients: make(map[string]*Client),
		stopCh:  stopCh,
	}
	testClient := &Client{
		connManager: newConnectionManager(),
		stopCh:      stopCh,
		cs:          cs,
	}
	testClient.stream, stream = pipe()

	// Start agent
	go testClient.Serve()
	defer close(stopCh)

	// Simulate sending a DATA packet to (Agent) Client
	const connID = 12345
	pkt := newDataPacket(connID, []byte("hello world"))
	if err := stream.Send(pkt); err != nil {
		t.Fatal(err)
	}

	// Expect to receive CLOSE_RSP packet from (Agent) Client
	pkt, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Type != client.PacketType_CLOSE_RSP {
		t.Errorf("expect PacketType_CLOSE_RSP; got %v", pkt.Type)
	}
	closeRsp := pkt.Payload.(*client.Packet_CloseResponse).CloseResponse
	if closeRsp.ConnectID != connID {
		t.Errorf("expect connID=%d; got %v", connID, closeRsp.ConnectID)
	}
}

// brokenStream wraps a ConnectClient and returns an error on Send and/or Recv if the respective
// error is non-nil.
type brokenStream struct {
	agent.AgentService_ConnectClient

	sendErr, recvErr error
}

func (bs *brokenStream) Send(packet *client.Packet) error {
	if bs.sendErr != nil {
		return bs.sendErr
	}
	return bs.AgentService_ConnectClient.Send(packet)
}

func (bs *brokenStream) Recv() (*client.Packet, error) {
	if bs.recvErr != nil {
		return nil, bs.recvErr
	}
	return bs.AgentService_ConnectClient.Recv()
}

func TestFailedSend_DialResp_GRPC(t *testing.T) {
	defer goleakVerifyNone(t, goleak.IgnoreCurrent())

	stopCh := make(chan struct{})
	cs := &ClientSet{
		clients: make(map[string]*Client),
		stopCh:  stopCh,
	}
	testClient := &Client{
		connManager: newConnectionManager(),
		stopCh:      stopCh,
		cs:          cs,
	}
	defer func() {
		close(stopCh)
		// Give a.probe() time to clean up
		// This is terrible and racy
		time.Sleep(time.Second)
	}()

	clientStream, stream := pipe()
	testClient.stream = &brokenStream{
		AgentService_ConnectClient: clientStream,
		sendErr:                    errors.New("expected send error"),
	}

	// Start agent
	go testClient.Serve()

	// Start test http server as remote service
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Log("Request Received", "url", r.URL, "userAgent", r.UserAgent())
		fmt.Fprint(w, "hello, world")
	}))
	defer ts.Close()

	func() {
		// goleak.IgnoreCurrent() needs to include all the goroutines which were started by testClient.Serve() above.
		// That includes things like pkg/agent/client.go:614 +0x88. Sleep does not guarantee they will have started.
		time.Sleep(time.Second)
		defer goleakVerifyNone(t, goleak.IgnoreCurrent())

		// Simulate sending KAS DIAL_REQ to (Agent) Client
		dialPacket := newDialPacket("tcp", strings.TrimPrefix(ts.URL, "http://"), 111)
		err := stream.Send(dialPacket)
		if err != nil {
			t.Fatal(err)
		}

		// Expect receiving DIAL_RSP packet from (Agent) Client
		_, err = stream.Recv()
		if err == nil {
			t.Error("expected timeout error, got none")
		}

		if conns := len(testClient.connManager.List()); conns > 0 {
			t.Errorf("Leaked %d connections", conns)
		}
	}()
}

func TestDrain(t *testing.T) {
	drainCh := make(chan struct{})
	stopCh := make(chan struct{})
	cs := &ClientSet{
		clients: make(map[string]*Client),
		drainCh: drainCh,
		stopCh:  stopCh,
	}
	testClient := &Client{
		connManager: newConnectionManager(),
		drainCh:     drainCh,
		stopCh:      stopCh,
		cs:          cs,
	}
	agentStream, stream := pipe()
	recvStarted := make(chan struct{}, 1)
	agentStream.recvStarted = recvStarted
	testClient.stream = agentStream

	// Start agent
	go testClient.Serve()
	defer close(stopCh)

	// Ensure Serve is blocked in Recv before requesting drain.
	select {
	case <-recvStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Serve to call Recv")
	}

	// Simulate pod first shutdown signal
	close(drainCh)

	// Expect to receive DRAIN packet from (Agent) Client
	pkt, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if pkt.Type != client.PacketType_DRAIN {
		t.Errorf("expect PacketType_DRAIN; got %v", pkt.Type)
	}
}

// fakeStream implements AgentService_ConnectClient
type fakeStream struct {
	grpc.ClientStream
	r           <-chan *client.Packet
	w           chan<- *client.Packet
	recvStarted chan struct{}
}

func pipe() (*fakeStream, *fakeStream) {
	r, w := make(chan *client.Packet, 2), make(chan *client.Packet, 2)
	s1, s2 := &fakeStream{}, &fakeStream{}
	s1.r, s1.w = r, w
	s2.r, s2.w = w, r
	return s1, s2
}

func (s *fakeStream) Send(packet *client.Packet) error {
	klog.V(4).InfoS("[DEBUG] send", "packet", packet)
	s.w <- packet
	return nil
}

func (s *fakeStream) Recv() (*client.Packet, error) {
	select {
	case s.recvStarted <- struct{}{}:
	default:
	}
	select {
	case pkt := <-s.r:
		klog.V(4).InfoS("[DEBUG] recv", "packet", pkt)
		return pkt, nil
	case <-time.After(5 * time.Second):
		return nil, errors.New("timeout recv")
	}
}

func newDialPacket(protocol, address string, random int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_REQ,
		Payload: &client.Packet_DialRequest{
			DialRequest: &client.DialRequest{
				Protocol: protocol,
				Address:  address,
				Random:   random,
			},
		},
	}
}

func newDataPacket(connID int64, data []byte) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: connID,
				Data:      data,
			},
		},
	}
}

func newClosePacket(connID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_CLOSE_REQ,
		Payload: &client.Packet_CloseRequest{
			CloseRequest: &client.CloseRequest{
				ConnectID: connID,
			},
		},
	}
}

func waitForConnectionDeletion(t *testing.T, testClient *Client, connID int64) {
	t.Helper()
	err := wait.PollImmediate(100*time.Millisecond, 3*time.Second, func() (bool, error) {
		_, ok := testClient.connManager.Get(connID)
		return !ok, nil
	})
	if err != nil {
		t.Error("client.endpointConn not released")
	}
}

func goleakVerifyNone(t *testing.T, options ...goleak.Option) {
	t.Helper()
	var err error
	waitErr := wait.PollImmediate(100*time.Millisecond, 3*time.Second, func() (bool, error) {
		err = goleak.Find(options...)
		if err == nil {
			return true, nil
		}
		return false, nil
	})
	if waitErr != nil {
		t.Error(err)
	}
}

func TestClose_Client_NilConn(t *testing.T) {
	stopCh := make(chan struct{})
	testClient := &Client{
		stopCh: stopCh,
	}
	testClient.Close()
	select {
	case <-stopCh:
	default:
		t.Fatal("stopCh not closed by Client.Close()")
	}
}

// TestServe_ExitDoesNotRemoveReplacementClient covers the race between a
// client whose stream died and the sync loop reconnecting to the same server:
// if the old client is already gone from the ClientSet when its Serve loop
// exits, the exit must not remove (and close) the client that replaced it.
func TestServe_ExitDoesNotRemoveReplacementClient(t *testing.T) {
	defer goleakVerifyNone(t, goleak.IgnoreCurrent())

	const serverID = "server-1"
	cs := &ClientSet{clients: make(map[string]*Client)}
	newClient := func() *Client {
		c := &Client{
			connManager:   newConnectionManager(),
			stopCh:        make(chan struct{}),
			probeInterval: time.Hour,
			serverID:      serverID,
			cs:            cs,
		}
		clientStream, _ := pipe()
		c.stream = &brokenStream{AgentService_ConnectClient: clientStream, recvErr: io.EOF}
		return c
	}

	old := newClient()
	cs.clients[serverID] = old
	// The stream to serverID died and was removed once already (for example by
	// the probe); the sync loop then reconnected to the same server.
	cs.RemoveClient(serverID)
	replacement := newClient()
	if err := cs.AddClient(serverID, replacement); err != nil {
		t.Fatalf("AddClient: %v", err)
	}

	// The old client's Serve loop now exits on its dead stream.
	old.Serve()

	if !cs.HasID(serverID) {
		t.Fatal("replacement client was removed by the old client's Serve exit")
	}
	select {
	case <-replacement.stopCh:
		t.Fatal("replacement client was closed by the old client's Serve exit")
	default:
	}
	cs.RemoveClient(serverID)
}

func TestConnect_Client_InvalidTokenPath(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal("failed to open port:", err)
	}
	defer l.Close()

	gs := grpc.NewServer()
	gs.RegisterService(&agent.AgentService_ServiceDesc, fakeServer{})

	var wg sync.WaitGroup
	defer func() {
		gs.Stop()
		wg.Wait()
	}()
	wg.Go(func() {
		gs.Serve(l)
	})

	// validate the setup actually works and allows the grpc dial to go through
	cl, _, err := newAgentClient(l.Addr().String(), "", "",
		&ClientSet{},
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal("failed to create agent client:", err)
	}
	if cl.conn == nil {
		t.Fatal("agent client conn nil")
	}
	cl.Close()

	// check error path when invalid SA token
	cl, _, err = newAgentClient(l.Addr().String(), "", "",
		&ClientSet{
			serviceAccountTokenPath: "./does-not-exist",
		},
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if !os.IsNotExist(err) {
		t.Fatal("expected not exist error, got:", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatal("unexpected error:", err)
	}
	if cl != nil {
		t.Fatal("expected nil agent client")
	}
}

type fakeServer struct{}

func (fakeServer) Connect(stream agent.AgentService_ConnectServer) error {
	h := metadata.Pairs(header.ServerID, "", header.ServerCount, "0")
	return stream.SendHeader(h)
}

var _ agent.AgentServiceServer = fakeServer{}
