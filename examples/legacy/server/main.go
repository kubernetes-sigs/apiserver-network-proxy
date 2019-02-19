package main

import (
	"flag"
	"fmt"
	"net"

	"google.golang.org/grpc"

	"github.com/anfernee/proxy-service/pkg/server"
	proxy "github.com/anfernee/proxy-service/proto"
)

func main() {
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 8090))
	if err != nil {
		panic(lis)
	}

	grpcServer := grpc.NewServer()
	server := server.NewProxyServer()

	proxy.RegisterProxyServiceServer(grpcServer, server)
	grpcServer.Serve(lis)
}
