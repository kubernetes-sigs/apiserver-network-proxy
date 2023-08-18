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
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

func TestProxy_ConcurrencyGRPC(t *testing.T) {
	ctx := context.Background()
	length := 1 << 20
	chunks := 10
	server := httptest.NewServer(newSizedServer(length, chunks))
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	var wg sync.WaitGroup
	verify := func() {
		defer wg.Done()

		// run test client
		tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, ps.FrontAddr(), grpc.WithInsecure())
		if err != nil {
			t.Error(err)
			return
		}

		c := &http.Client{
			Transport: &http.Transport{
				DialContext: tunnel.DialContext,
			},
		}

		r, err := c.Get(server.URL)
		if err != nil {
			t.Error(err)
			return
		}

		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer r.Body.Close()

		if len(data) != length*chunks {
			t.Errorf("expect data length %d; got %d", length*chunks, len(data))
		}
	}

	concurrency := 10
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go verify()
	}
	wg.Wait()
}

func TestProxy_ConcurrencyHTTP(t *testing.T) {
	ctx := context.Background()
	length := 1 << 20
	chunks := 10
	server := httptest.NewServer(newSizedServer(length, chunks))
	defer server.Close()

	ps := runHTTPConnProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test clients
	var wg sync.WaitGroup
	verify := func() {
		defer wg.Done()
		tunnel, err := createHTTPConnectClient(ctx, ps.FrontAddr(), server.URL)
		if err != nil {
			t.Error(err)
		}
		data, err := clientRequest(tunnel, server.URL)
		if err != nil {
			t.Error(err)
		} else if len(data) != length*chunks {
			t.Errorf("expect data length %d; got %d", length*chunks, len(data))
		}
	}

	concurrency := 10
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go verify()
	}
	wg.Wait()
}

// This test verifies that when one stream between a proxy agent and the proxy
// server terminates, the proxy server does not terminate other frontends
// supported by the same proxy agent but on different streams.
func TestAgent_MultipleConn(t *testing.T) {
	testcases := []struct {
		name                string
		proxyServerFunction func(testing.TB) framework.ProxyServer
		clientFunction      func(context.Context, string, string) (*http.Client, error)
	}{
		{
			name:                "grpc",
			proxyServerFunction: runGRPCProxyServer,
			clientFunction:      createGrpcTunnelClient,
		},
		{
			name:                "http-connect",
			proxyServerFunction: runHTTPConnProxyServer,
			clientFunction:      createHTTPConnectClient,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			waitServer := newWaitingServer()
			server := httptest.NewServer(waitServer)
			defer server.Close()

			ps := tc.proxyServerFunction(t)
			defer ps.Stop()

			ai1 := runAgentWithID(t, "multipleAgentConn", ps.AgentAddr())
			defer ai1.Stop()
			waitForConnectedServerCount(t, 1, ai1)

			// run test client
			c, err := tc.clientFunction(ctx, ps.FrontAddr(), server.URL)
			if err != nil {
				t.Fatal(err)
			}

			fcnStopCh := make(chan struct{})

			go func() {
				_, err := clientRequest(c, server.URL)
				if err != nil {
					t.Errorf("expected no error on proxy request, got %v", err)
				}
				close(fcnStopCh)
			}()
			<-waitServer.requestReceivedCh

			// Running an agent with the same ID simulates a second connection from the same agent.
			// This simulates the scenario where a proxy agent established connections with HA proxy server
			// and creates multiple connections with the same proxy server
			ai2 := runAgentWithID(t, "multipleAgentConn", ps.AgentAddr())
			defer ai2.Stop()
			waitForConnectedServerCount(t, 1, ai2)
			// Wait for the server to run cleanup routine
			waitForConnectedAgentCount(t, 1, ps)
			close(waitServer.respondCh)

			<-fcnStopCh
		})
	}
}
