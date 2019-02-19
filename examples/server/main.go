package main

import (
	"flag"
	"fmt"
	"net"

	"google.golang.org/grpc"

	agentserver "github.com/anfernee/proxy-service/pkg/agent/server"
	"github.com/anfernee/proxy-service/proto/agent"
)

func main() {
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 8090))
	if err != nil {
		panic(lis)
	}

	grpcServer := grpc.NewServer()
	server := agentserver.NewProxyServer()

	agent.RegisterProxyServiceServer(grpcServer, server)
	grpcServer.Serve(lis)
}
