package main

import (
	"context"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	proxy "github.com/anfernee/proxy-service/proto/proxy2"
)

func main() {
	c, err := grpc.Dial("localhost:8095", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}

	client := proxy.NewProxyServiceClient(c)

	md := metadata.Pairs(proxy.ProxyHeaderKey_REMOTE_ADDRESS.String(), "8.8.8.8:53")
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	stream, err := client.Proxy(ctx)
	if err != nil {
		panic(err)
	}

	err = stream.Send(&proxy.Bytes{Data: []byte("hello")})
	if err != nil {
		panic(err)
	}

	bytes, err := stream.Recv()
	if err != nil {
		panic(err)
	}

	log.Printf("recv: %v", string(bytes.Data))

	err = stream.CloseSend()
	if err != nil {
		panic(err)
	}
}
