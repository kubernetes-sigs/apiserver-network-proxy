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
