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

package tests

import (
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"k8s.io/apimachinery/pkg/util/wait"
	agentproto "sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

func TestClientReconnects(t *testing.T) {
	connections := make(chan struct{})
	s := &testAgentServerImpl{
		onConnect: func(stream agentproto.AgentService_ConnectServer) error {
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
		if err := svr.Serve(lis); err != nil {
			panic(err)
		}
	}()

	a := runAgentWithID(t, "test-id", lis.Addr().String())
	defer a.Stop()

	select {
	case <-connections:
		// Expected
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatal("Timed out waiting for agent to connect")
	}
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
	defer svr2.Stop()

	select {
	case <-connections:
		// Expected
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatal("Timed out waiting for agent to reconnect")
	}
}

type testAgentServerImpl struct {
	onConnect func(agentproto.AgentService_ConnectServer) error
}

func (t *testAgentServerImpl) Connect(svr agentproto.AgentService_ConnectServer) error {
	return t.onConnect(svr)
}
