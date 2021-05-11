package tests

import (
	"net"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	agentproto "sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

func TestClientReconnects(t *testing.T) {
	connections := make(chan struct{})
	s := &testAgentServerImpl{
		onConnect: func(stream agent.AgentService_ConnectServer) error {
			stream.SetHeader(metadata.New(map[string]string{
				header.ServerID:    uuid.Must(uuid.NewRandom()).String(),
				header.ServerCount: "1",
			}))
			connections <- struct{}{}
			return nil
		},
	}

	svr := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(svr, s)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		svr.Serve(lis)
	}()

	stopCh := make(chan struct{})
	defer close(stopCh)
	runAgentWithID("test-id", lis.Addr().String(), stopCh)

	<-connections
	svr.Stop()

	lis2, err := net.Listen("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	svr2 := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(svr2, s)
	go func() {
		if err := svr2.Serve(lis2); err != nil {
			panic(err)
		}
	}()

	<-connections
}

type testAgentServerImpl struct {
	onConnect func(agent.AgentService_ConnectServer) error
}

func (t *testAgentServerImpl) Connect(svr agent.AgentService_ConnectServer) error {
	return t.onConnect(svr)
}
