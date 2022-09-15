package agent

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
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
	pkg, err := stream.Recv()
	if err != nil {
		t.Fatal(err.Error())
	}
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

	// Send Data (HTTP Request) via (Agent) Client to the test http server
	dataPacket := newDataPacket(connID, []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	err = stream.Send(dataPacket)
	if err != nil {
		t.Error(err.Error())
	}

	// Expect receiving http response via (Agent) Client
	pkg, _ = stream.Recv()
	if pkg == nil {
		t.Fatal("unexpected nil packet")
	}
	if pkg.Type != client.PacketType_DATA {
		t.Errorf("expect PacketType_DATA; got %v", pkg.Type)
	}
	data := pkg.Payload.(*client.Packet_Data).Data.Data

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
	pkg, _ = stream.Recv()
	if pkg == nil {
		t.Fatal("unexpected nil packet")
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
	// connID will be removed when remoteToProxy on another goroutine exits.
	// Wait a short period to see if the connection gets cleaned up.
	waitForConnectionDeletion(t, testClient, connID)

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

		// Stimulate sending KAS DIAL_REQ to (Agent) Client
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

func waitForConnectionDeletion(t *testing.T, testClient *Client, connID int64) {
	t.Helper()
	err := wait.PollImmediate(100*time.Millisecond, 3*time.Second, func() (bool, error) {
		_, ok := testClient.connManager.Get(connID)
		return !ok, nil
	})
	if err != nil {
		t.Error("client.connContext not released")
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
