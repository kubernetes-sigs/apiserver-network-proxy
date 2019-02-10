package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc"

	proxy "github.com/anfernee/proxy-service/proto"
)

func main() {
	conn, err := grpc.Dial("localhost:8090", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}

	client := proxy.NewProxyServiceClient(conn)

	dialReq := &proxy.DialRequest{
		Protocol: "tcp",
		Address:  "localhost:8000",
	}

	ctx := context.Background()

	resp, err := client.Dial(ctx, dialReq)
	if err != nil {
		panic(err)
	}

	log.Println(resp)

	stream, err := client.Connect(ctx)
	if err != nil {
		panic(err)
	}

	msg := &proxy.Data{
		StreamID: resp.StreamID,
	}

	err = stream.Send(msg)
	if err != nil {
		panic(err)
	}

	msg = &proxy.Data{
		StreamID: resp.StreamID,
		Data:     []byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"),
	}

	err = stream.Send(msg)
	if err != nil {
		panic(err)
	}

	for {
		dataresp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		fmt.Println(string(dataresp.Data))
	}

	/*
		for {
			msg := &proxy.ConnectRequest{
				Protocol: "tcp",
				Address:  "localhost:8000",
			}

			err := stream.Send(msg)
			if err != nil {
				log.Printf("send error: %v", err)
				break
			}

			msg = &proxy.ConnectRequest{
				Protocol: "tcp",
				Address:  "localhost:9000",
			}

			err = stream.Send(msg)
			if err != nil {
				log.Printf("send error: %v", err)
				break
			}

			resp, err := stream.Recv()
			if err != nil {
				log.Printf("recv error: %v", err)
				break
			}

			log.Printf("resp: %v", resp)
		}
	*/
}
