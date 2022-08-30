package tests

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

func TestProxy_Agent_Disconnect_HTTP_Persistent_Connection(t *testing.T) {
	testcases := []struct {
		name                string
		proxyServerFunction func() (proxy, func(), error)
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

			stopCh := make(chan struct{})

			proxy, cleanup, err := tc.proxyServerFunction()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			runAgent(proxy.agent, stopCh)
			waitForConnectedAgentCount(t, 1, proxy.server)

			// run test client

			c, err := tc.clientFunction(ctx, proxy.front, server.URL)
			if err != nil {
				t.Errorf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c, server.URL)

			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
			close(stopCh)

			// Wait for the agent to disconnect
			waitForConnectedAgentCount(t, 0, proxy.server)

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

func TestProxy_Agent_Reconnect(t *testing.T) {
	testcases := []struct {
		name                string
		proxyServerFunction func() (proxy, func(), error)
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

			stopCh := make(chan struct{})

			proxy, cleanup, err := tc.proxyServerFunction()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			cs1 := runAgent(proxy.agent, stopCh)
			waitForConnectedServerCount(t, 1, cs1)

			// run test client

			c, err := tc.clientFunction(ctx, proxy.front, server.URL)
			if err != nil {
				t.Errorf("error obtaining client: %v", err)
			}

			_, err = clientRequest(c, server.URL)
			if err != nil {
				t.Errorf("expected no error on proxy request, got %v", err)
			}
			close(stopCh)

			// Wait for the agent to disconnect
			waitForConnectedAgentCount(t, 0, proxy.server)

			// Reconnect agent
			stopCh2 := make(chan struct{})
			defer close(stopCh2)
			cs2 := runAgent(proxy.agent, stopCh2)
			waitForConnectedServerCount(t, 1, cs2)

			// Proxy requests should work again after agent reconnects
			c2, err := tc.clientFunction(ctx, proxy.front, server.URL)
			if err != nil {
				t.Errorf("error obtaining client: %v", err)
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

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %w", err)
	}

	r.Body.Close()

	return data, nil
}

func createGrpcTunnelClient(ctx context.Context, proxyAddr, addr string) (*http.Client, error) {
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxyAddr, grpc.WithInsecure())
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

func createHTTPConnectClient(ctx context.Context, proxyAddr, addr string) (*http.Client, error) {
	conn, err := net.Dial("tcp", proxyAddr)
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
