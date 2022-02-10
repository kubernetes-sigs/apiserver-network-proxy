package tests

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

func TestProxy_ConcurrencyGRPC(t *testing.T) {
	ctx := context.Background()
	length := 1 << 20
	chunks := 10
	server := httptest.NewServer(newSizedServer(length, chunks))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	runAgent(proxy.agent, stopCh)

	// Wait for agent to register on proxy server
	time.Sleep(time.Second)

	// run test client
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	verify := func() {
		defer wg.Done()

		c := &http.Client{
			Transport: &http.Transport{
				DialContext: tunnel.DialContext,
			},
		}

		r, err := c.Get(server.URL)
		if err != nil {
			t.Error(err)
		}

		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
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

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runHTTPConnProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	runAgent(proxy.agent, stopCh)

	// Wait for agent to register on proxy server
	wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
		ready, _ := proxy.server.Readiness.Ready()
		return ready, nil
	})

	// run test clients
	var wg sync.WaitGroup
	verify := func() {
		defer wg.Done()
		tunnel, err := createHTTPConnectClient(ctx, proxy.front, server.URL)
		if err != nil {
			t.Error(err)
		}
		data, err := clientRequest(tunnel, server.URL)
		if err != nil {
			t.Error(err)
		}
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

// This test verifies that when one stream between a proxy agent and the proxy
// server terminates, the proxy server does not terminate other frontends
// supported by the same proxy agent but on different streams.
func TestAgent_MultipleConn(t *testing.T) {
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
			echoServer := newEchoServer("hello")
			echoServer.wchan = make(chan struct{})
			server := httptest.NewServer(echoServer)
			defer server.Close()

			stopCh := make(chan struct{})
			stopCh2 := make(chan struct{})

			proxy, cleanup, err := tc.proxyServerFunction()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			runAgentWithID("multipleAgentConn", proxy.agent, stopCh)
			defer close(stopCh)

			// Wait for agent to register on proxy server
			wait.Poll(100*time.Millisecond, 5*time.Second, func() (bool, error) {
				ready, _ := proxy.server.Readiness.Ready()
				return ready, nil
			})

			// run test client
			c, err := tc.clientFunction(ctx, proxy.front, server.URL)

			fcnStopCh := make(chan struct{})

			go func() {
				_, err := clientRequest(c, server.URL)
				if err != nil {
					t.Errorf("expected no error on proxy request, got %v", err)
				}
				close(fcnStopCh)
			}()

			// Running an agent with the same ID simulates a second connection from the same agent.
			// This simulates the scenario where a proxy agent established connections with HA proxy server
			// and creates multiple connections with the same proxy server
			runAgentWithID("multipleAgentConn", proxy.agent, stopCh2)
			close(stopCh2)
			// Wait for the server to run cleanup routine
			time.Sleep(1 * time.Second)
			close(echoServer.wchan)

			<-fcnStopCh
		})
	}
}
