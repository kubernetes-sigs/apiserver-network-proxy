package agent

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

func TestServeData_HTTP(t *testing.T) {
	var err error
	var stream agent.AgentService_ConnectClient
	stopCh := make(chan struct{})
	testClient := &Client{
		serverChannel: make(chan *client.Packet, xfrChannelSize),
		serverError:   nil,
		connManager:   newConnectionManager(),
		stopCh:        stopCh,
	}
	testClient.stream, stream = pipe()

	// Start agent
	go testClient.Serve()
	go testClient.writeToProxyServer()
	defer close(stopCh)

	// Start test http server as remote service
	expectedBody := "Hello, client"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, expectedBody)
	}))
	defer ts.Close()

	// Stimulate sending KAS DIAL_REQ to (Agent) Client
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
	if _, ok := testClient.connManager.Get(connID); ok {
		t.Error("client.connContext not released")
	}
}

func TestClose_Client(t *testing.T) {
	var stream agent.AgentService_ConnectClient
	stopCh := make(chan struct{})
	testClient := &Client{
		serverChannel: make(chan *client.Packet, xfrChannelSize),
		serverError:   nil,
		connManager: newConnectionManager(),
		stopCh:      stopCh,
	}
	testClient.stream, stream = pipe()

	// Start agent
	go testClient.Serve()
	go testClient.writeToProxyServer()
	defer close(stopCh)

	// Start test http server as remote service
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello, world")
	}))
	defer ts.Close()

	// Stimulate sending KAS DIAL_REQ to (Agent) Client
	dialPacket := newDialPacket("tcp", ts.URL[len("http://"):], 111)
	err := stream.Send(dialPacket)
	if err != nil {
		t.Fatal(err)
	}

	// Expect receiving DIAL_RSP packet from (Agent) Client
	pkg, _ := stream.Recv()
	if pkg == nil {
		t.Fatal("unexpected nil packet")
	}
	if pkg.Type != client.PacketType_DIAL_RSP {
		t.Errorf("expect PacketType_DIAL_RSP; got %v", pkg.Type)
	}
	dialRsp := pkg.Payload.(*client.Packet_DialResponse)
	connID := dialRsp.DialResponse.ConnectID
	if dialRsp.DialResponse.Random != 111 {
		t.Errorf("expect random=111; got %v", dialRsp.DialResponse.Random)
	}

	closePacket := newClosePacket(connID)
	if err := stream.Send(closePacket); err != nil {
		t.Fatal(err)
	}

	// Expect receiving close response via (Agent) Client
	pkg, _ = stream.Recv()
	if pkg == nil {
		t.Error("unexpected nil packet")
	}
	if pkg.Type != client.PacketType_CLOSE_RSP {
		t.Errorf("expect PacketType_CLOSE_RSP; got %v", pkg.Type)
	}
	closeErr := pkg.Payload.(*client.Packet_CloseResponse).CloseResponse.Error
	if closeErr != "" {
		t.Errorf("expect nil closeErr; got %v", closeErr)
	}

	// Verify internal state is consistent
	if _, ok := testClient.connManager.Get(connID); ok {
		t.Error("client.connContext not released")
	}

	// Verify remote conn is closed
	if err := stream.Send(closePacket); err != nil {
		t.Fatal(err)
	}

	// Expect receiving close response via (Agent) Client
	pkg, _ = stream.Recv()
	if pkg == nil {
		t.Error("unexpected nil packet")
	}
	if pkg.Type != client.PacketType_CLOSE_RSP {
		t.Errorf("expect PacketType_CLOSE_RSP; got %+v", pkg)
	}
	closeErr = pkg.Payload.(*client.Packet_CloseResponse).CloseResponse.Error
	if closeErr != "Unknown connectID" {
		t.Errorf("expect Unknown connectID; got %v", closeErr)
	}

}

// fakeStream implements AgentService_ConnectClient
type fakeStream struct {
	grpc.ClientStream
	r <-chan *client.Packet
	w chan<- *client.Packet
}

func pipe() (agent.AgentService_ConnectClient, agent.AgentService_ConnectClient) {
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
	case pkg := <-s.r:
		klog.V(4).InfoS("[DEBUG] recv", "packet", pkg)
		return pkg, nil
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
