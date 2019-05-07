package agentclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

func TestServeData_HTTP(t *testing.T) {
	client := NewAgentClient("")
	client.stream = newStream()
	stopCh := make(chan struct{})

	// Start agnet
	go client.Serve(stopCh)
	defer close(stopCh)

	// Start test http server as remote service
	expectedBody := "Hello, client"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, expectedBody)
	}))
	defer ts.Close()

	// Stimulate sending KAS DIAL_REQ to AgentClient
	dialPacket := &agent.Packet{
		Type: agent.PacketType_DIAL_REQ,
		Payload: &agent.Packet_DialRequest{
			DialRequest: &agent.DialRequest{
				Protocol: "tcp",
				Address:  ts.URL[len("http://"):],
				Random:   111,
			},
		},
	}
	client.stream.Send(dialPacket)

	// Expect receiving DIAL_RSP packet from AgentClient
	pkg, _ := client.stream.Recv()
	if pkg == nil {
		t.Error("unexpected nil packet")
	}
	if pkg.Type != agent.PacketType_DIAL_RSP {
		t.Errorf("expect PacketType_DIAL_RSP; got %v", pkg.Type)
	}
	dialRsp := pkg.Payload.(*agent.Packet_DialResponse)
	connID := dialRsp.DialResponse.ConnectID
	if dialRsp.DialResponse.Random != 111 {
		t.Errorf("expect random=111; got %v", dialRsp.DialResponse.Random)
	}

	// Send Data (HTTP Request) via AgentClient to the test http server
	dataPacket := &agent.Packet{
		Type: agent.PacketType_DATA,
		Payload: &agent.Packet_Data{
			Data: &agent.Data{
				ConnectID: connID,
				Data:      []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"), // HTTP request
			},
		},
	}
	client.stream.Send(dataPacket)

	// Expect receiving http response via AgentClient
	pkg, _ = client.stream.Recv()
	if pkg == nil {
		t.Error("unexpected nil packet")
	}
	if pkg.Type != agent.PacketType_DATA {
		t.Errorf("expect PacketType_DIAL_RSP; got %v", pkg.Type)
	}
	data := pkg.Payload.(*agent.Packet_Data).Data.Data

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
}

// fakeStream implements AgentService_ConnectClient
type fakeStream struct {
	grpc.ClientStream
	ch chan *agent.Packet
}

func newStream() agent.AgentService_ConnectClient {
	s := &fakeStream{}
	s.ch = make(chan *agent.Packet)
	return s
}

func (s *fakeStream) Send(packet *agent.Packet) error {
	s.ch <- packet
	return nil
}

func (s *fakeStream) Recv() (*agent.Packet, error) {
	return <-s.ch, nil
}
