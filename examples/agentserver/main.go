package main

import (
	"fmt"
	"net"

	"google.golang.org/grpc"

	"github.com/anfernee/proxy-service/pkg/agent/agentserver"
	"github.com/anfernee/proxy-service/proto/agent"
)

func main() {
	server := agentserver.NewProxyServer()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 8090))
	if err != nil {
		panic(err)
	}

	grpcServer := grpc.NewServer()

	agent.RegisterProxyServiceServer(grpcServer, server)
	go grpcServer.Serve(lis)

	lis2, err := net.Listen("tcp", fmt.Sprintf(":%d", 8091))
	if err != nil {
		panic(err)
	}

	grpcServer2 := grpc.NewServer()

	agent.RegisterAgentServiceServer(grpcServer2, server)
	go grpcServer2.Serve(lis2)

	stopCh := make(chan struct{})
	<-stopCh
}
