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
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

func TestProxy_Agent_Disconnect_Persistent_Connection(t *testing.T) {
	testcases := []struct {
		name                string
		proxyServerFunction func(t testing.TB) framework.ProxyServer
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
			server := httptest.NewServer(newEchoServer("hello"))
			defer server.Close()

			ps := tc.proxyServerFunction(t)
			defer ps.Stop()

			a := runAgent(t, ps.AgentAddr())
			waitForConnectedAgentCount(t, 1, ps)

			// run test client

			c, err := tc.clientFunction(ctx, ps.FrontAddr(), server.URL)
			if err != nil {
				t.Fatalf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c, server.URL)

			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
			a.Stop()

			// Wait for the agent to disconnect
			waitForConnectedAgentCount(t, 0, ps)

			// Reuse same client to make the request
			_, err = clientRequest(c, server.URL)
			if err == nil {
				t.Errorf("expect request using http persistent connections to fail after dialing on a broken connection")
			} else if os.IsTimeout(err) {
				t.Errorf("expect request using http persistent connections to fail with error use of closed network connection. Got timeout")
			}
		})
	}
}

func TestAgentRestartReconnect(t *testing.T) {
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

			server := httptest.NewServer(newEchoServer("hello"))
			defer server.Close()

			ps := tc.proxyServerFunction(t)
			defer ps.Stop()

			ai1 := runAgent(t, ps.AgentAddr())
			waitForConnectedServerCount(t, 1, ai1)

			// run test client

			c, err := tc.clientFunction(ctx, ps.FrontAddr(), server.URL)
			if err != nil {
				t.Fatalf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c, server.URL)
			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
			ai1.Stop()

			// Wait for the agent to disconnect
			waitForConnectedAgentCount(t, 0, ps)

			// Reconnect agent
			ai2 := runAgent(t, ps.AgentAddr())
			defer ai2.Stop()
			waitForConnectedServerCount(t, 1, ai2)

			// Proxy requests should work again after agent reconnects
			c2, err := tc.clientFunction(ctx, ps.FrontAddr(), server.URL)
			if err != nil {
				t.Fatalf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c2, server.URL)

			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
		})
	}
}

func clientRequest(c *http.Client, addr string) ([]byte, error) {
	r, err := c.Get(addr)
	if err != nil {
		return nil, fmt.Errorf("http GET %q: %w", addr, err)
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	r.Body.Close()

	return data, nil
}

func createGrpcTunnelClient(ctx context.Context, proxyAddr, _ string) (*http.Client, error) {
	tunnel, err := createSingleUseGrpcTunnel(ctx, proxyAddr)
	if err != nil {
		return nil, err
	}

	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
	}

	return c, nil
}

func createHTTPConnectClient(_ context.Context, proxyAddr, addr string) (*http.Client, error) {
	conn, err := net.Dial("unix", proxyAddr)
	if err != nil {
		return nil, err
	}

	serverURL, _ := url.Parse(addr)

	// Send HTTP-Connect request
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", serverURL.Host, "127.0.0.1")
	if err != nil {
		return nil, err
	}

	// Parse the HTTP response for Connect
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("reading HTTP response from CONNECT: %v", err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("expect 200; got %d", res.StatusCode)
	}
	if br.Buffered() > 0 {
		return nil, fmt.Errorf("unexpected extra buffer")
	}

	dialer := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	c := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Dial: dialer,
		},
	}

	return c, nil
}
