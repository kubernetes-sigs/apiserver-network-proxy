/*
Copyright 2019 The Kubernetes Authors.

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

package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc"

	proxy "sigs.k8s.io/apiserver-network-proxy/proto"
)

func main() {
	conn, err := grpc.Dial("localhost:8090", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}

	client := proxy.NewProxyServiceClient(conn)

	// Run remote simple http service on server side as
	// "python -m SimpleHTTPServer"

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
}
