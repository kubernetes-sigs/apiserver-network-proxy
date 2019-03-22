package main

import (
	"fmt"
	"log"
	"net"
	"strings"

	proxy "github.com/anfernee/proxy-service/proto/proxy2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func main() {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", 8095))
	if err != nil {
		panic(err)
	}

	server := grpc.NewServer()
	proxy.RegisterProxyServiceServer(server, &ProxyServer{})

	log.Println("servering :8095")
	server.Serve(lis)
}

// ProxyServer ...
type ProxyServer struct {
}

// Proxy ...
func (s *ProxyServer) Proxy(stream proxy.ProxyService_ProxyServer) error {
	log.Println("got proxy request")

	md, ok := metadata.FromIncomingContext(stream.Context())
	log.Println("metadata", md, ok)

	bytes, err := stream.Recv()
	if err != nil {
		return err
	}
	log.Println("recv: ", string(bytes.Data))

	remote := md.Get(proxy.ProxyHeaderKey_REMOTE_ADDRESS.String())
	return stream.Send(&proxy.Bytes{Data: []byte(strings.Join(remote, ","))})
}
