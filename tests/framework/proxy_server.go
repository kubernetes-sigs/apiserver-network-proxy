/*
Copyright 2023 The Kubernetes Authors.

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

package framework

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	clientproto "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	agentproto "sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

type ProxyServerOpts struct {
	ServerCount int
	Mode        string
}

type ProxyServerRunner interface {
	Start(ProxyServerOpts) (ProxyServer, error)
}

type ProxyServer interface {
	ConnectedBackends() int
	FrontendConnectionCount() int
	AgentAddr() string
	FrontAddr() string
	Ready() bool
	Stop() error
}

type InProcessProxyServerRunner struct{}

func (*InProcessProxyServerRunner) Start(opts ProxyServerOpts) (ProxyServer, error) {
	switch opts.Mode {
	case "http":
		return startHTTP(opts)
	case "grpc":
		return startGRPC(opts)
	default:
		panic("must specify proxy server mode")
	}
}

func startGRPC(opts ProxyServerOpts) (ProxyServer, error) {
	var err error

	commonServer, err := startInProcessCommonProxyServer(opts)
	if err != nil {
		return nil, err
	}

	ps := &inProcessGRPCProxyServer{
		inProcessCommonProxyServer: commonServer,
		grpcServer:                 grpc.NewServer(),
	}

	clientproto.RegisterProxyServiceServer(ps.grpcServer, ps.proxyServer)
	ps.grpcListener, err = net.Listen("tcp", "")
	if err != nil {
		return ps, err
	}
	go ps.grpcServer.Serve(ps.grpcListener)

	return ps, nil
}

type inProcessGRPCProxyServer struct {
	*inProcessCommonProxyServer

	grpcServer   *grpc.Server
	grpcListener net.Listener
}

func (ps *inProcessGRPCProxyServer) Stop() error {
	ps.stopCommonProxyServer()

	if ps.grpcListener != nil {
		ps.grpcListener.Close()
	}
	ps.grpcServer.Stop()
	return nil
}

func (ps *inProcessGRPCProxyServer) FrontAddr() string {
	return ps.grpcListener.Addr().String()
}

func (ps *inProcessGRPCProxyServer) FrontendConnectionCount() int {
	panic("unimplemented: inProcessGRPCProxyServer.FrontendConnectionCount") // FIXME: consider reading from metrics
}

func startHTTP(opts ProxyServerOpts) (ProxyServer, error) {
	var err error

	commonServer, err := startInProcessCommonProxyServer(opts)
	if err != nil {
		return nil, err
	}

	ps := &inProcessHTTPProxyServer{
		inProcessCommonProxyServer: commonServer,
	}

	// http-connect
	handler := &server.Tunnel{
		Server: ps.proxyServer,
	}
	ps.httpServer = &http.Server{
		ReadHeaderTimeout: 60 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&ps.httpConnectionCount, 1)
			defer atomic.AddInt32(&ps.httpConnectionCount, -1)
			handler.ServeHTTP(w, r)
		}),
	}
	ps.httpListener, err = net.Listen("tcp", "")
	if err != nil {
		return ps, err
	}

	go func() {
		err := ps.httpServer.Serve(ps.httpListener)
		if err != nil {
			fmt.Println("http connect server error: ", err)
		}
	}()

	return ps, nil
}

type inProcessHTTPProxyServer struct {
	*inProcessCommonProxyServer

	httpServer   *http.Server
	httpListener net.Listener

	httpConnectionCount int32 // atomic
}

func (ps *inProcessHTTPProxyServer) Stop() error {
	ps.stopCommonProxyServer()

	if ps.httpListener != nil {
		ps.httpListener.Close()
	}
	ps.httpServer.Shutdown(context.Background())

	return nil
}

func (ps *inProcessHTTPProxyServer) FrontAddr() string {
	return ps.httpListener.Addr().String()
}

func (ps *inProcessHTTPProxyServer) FrontendConnectionCount() int {
	return int(atomic.LoadInt32(&ps.httpConnectionCount))
}

// inProcessCommonProxyServer handles the common agent (backend) serving shared between the
// inProccessGRPCProxyServer and inProcessHTTPProxyServer
type inProcessCommonProxyServer struct {
	proxyServer *server.ProxyServer

	agentServer   *grpc.Server
	agentListener net.Listener
}

func startInProcessCommonProxyServer(opts ProxyServerOpts) (*inProcessCommonProxyServer, error) {
	s := server.NewProxyServer(uuid.New().String(), []server.ProxyStrategy{server.ProxyStrategyDefault}, opts.ServerCount, &server.AgentTokenAuthenticationOptions{})
	ps := &inProcessCommonProxyServer{
		proxyServer: s,
		agentServer: grpc.NewServer(),
	}
	agentproto.RegisterAgentServiceServer(ps.agentServer, s)
	var err error
	ps.agentListener, err = net.Listen("tcp", "")
	if err != nil {
		return ps, err
	}
	go ps.agentServer.Serve(ps.agentListener)

	return ps, nil
}

func (ps *inProcessCommonProxyServer) stopCommonProxyServer() {
	if ps.agentListener != nil {
		ps.agentListener.Close()
	}
	ps.agentServer.Stop()
}

func (ps *inProcessCommonProxyServer) AgentAddr() string {
	return ps.agentListener.Addr().String()
}

func (ps *inProcessCommonProxyServer) ConnectedBackends() int {
	numBackends := 0
	for _, bm := range ps.proxyServer.BackendManagers {
		numBackends += bm.NumBackends()
	}
	return numBackends
}

func (ps *inProcessCommonProxyServer) Ready() bool {
	ready, _ := ps.proxyServer.Readiness.Ready()
	return ready
}
