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
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	metricsclient "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client/metrics"
	clientmetricstest "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics/testing"
	metricsagent "sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	metricsserver "sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
	agentproto "sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

// Define a blackholed address, for which Dial is expected to hang. This address is reserved for
// benchmarking by RFC 6890.
const blackhole = "198.18.0.254:1234"

// test remote server
type testServer struct {
	echo   []byte
	chunks int
}

func newEchoServer(echo string) *testServer {
	return &testServer{
		echo:   []byte(echo),
		chunks: 1,
	}
}

func newSizedServer(length, chunks int) *testServer {
	return &testServer{
		echo:   make([]byte, length),
		chunks: chunks,
	}
}

func (s *testServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	for i := 0; i < s.chunks; i++ {
		w.Write(s.echo)
	}
}

type waitingServer struct {
	requestReceivedCh chan struct{} // channel is closed when the server receives a request
	respondCh         chan struct{} // server responds when this channel is closed
}

func newWaitingServer() *waitingServer {
	return &waitingServer{
		requestReceivedCh: make(chan struct{}),
		respondCh:         make(chan struct{}),
	}
}

func (s *waitingServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	close(s.requestReceivedCh)
	<-s.respondCh // Wait for permission to respond.
	w.Write([]byte("hello"))
}

type delayedServer struct {
	minWait time.Duration
	maxWait time.Duration
}

func newDelayedServer() *delayedServer {
	return &delayedServer{
		minWait: 500 * time.Millisecond,
		maxWait: 2 * time.Second,
	}
}

var _ = newDelayedServer() // Suppress unused lint error.

func (s *delayedServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	delay := time.Duration(rand.Int63n(int64(s.maxWait-s.minWait))) + s.minWait /* #nosec G404 */
	time.Sleep(delay)
	w.Write([]byte("hello"))
}

func TestBasicProxy_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test client
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnel, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
	}

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("expect %v; got %v", "hello", string(data))
	}
}

func TestProxyHandleDialError_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	invalidServer := httptest.NewServer(newEchoServer("hello"))

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test client
	tunnel, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
	}

	url := invalidServer.URL
	invalidServer.Close()

	_, err = c.Get(url)
	if err == nil {
		t.Error("Expected error when destination is unreachable, did not receive error")
	} else if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Unexpected error: %v", err)
	}

	if err := ps.Metrics().ExpectServerDialFailure(metricsserver.DialFailureErrorResponse, 1); err != nil {
		t.Error(err)
	}
	if err := a.Metrics().ExpectAgentDialFailure(metricsagent.DialFailureUnknown, 1); err != nil {
		t.Error(err)
	}
	resetAllMetrics() // For clean shutdown.
}

func TestProxyHandle_DoneContext_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test client
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	_, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err == nil {
		t.Error("Expected error when context is cancelled, did not receive error")
	} else if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestProxyHandle_RequestDeadlineExceeded_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	slowServer := newWaitingServer()
	server := httptest.NewServer(slowServer)
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	func() {
		// Ensure that tunnels aren't leaked with long-running servers.
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent(), goleak.IgnoreTopFunction("google.golang.org/grpc.(*addrConn).resetTransport"))

		// run test client
		tunnel, err := createSingleUseGrpcTunnel(context.Background(), ps.FrontAddr())
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		go func() {
			<-ctx.Done() // Wait for context to time out.
			close(slowServer.respondCh)
		}()

		c := &http.Client{
			Transport: &http.Transport{
				DialContext: tunnel.DialContext,
			},
		}

		req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}

		_, err = c.Do(req)
		if err == nil {
			t.Error("Expected error when context is cancelled, did not receive error")
		} else if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("Unexpected error: %v", err)
		}

		t.Log("Wait for tunnel to close")
		select {
		case <-tunnel.Done():
			t.Log("Tunnel closed successfully")
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Timed out waiting for tunnel to close")
		}
	}()
}

func TestProxyDial_RequestCancelled_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	agent := &unresponsiveAgent{}
	if err := agent.Connect(ps.AgentAddr()); err != nil {
		t.Fatalf("Failed to connect unresponsive agent: %v", err)
	}
	defer agent.Close()
	waitForConnectedAgentCount(t, 1, ps)

	func() {
		// Ensure that tunnels aren't leaked with long-running servers.
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent(), goleak.IgnoreTopFunction("google.golang.org/grpc.(*addrConn).resetTransport"))

		// run test client
		tunnel, err := createSingleUseGrpcTunnel(context.Background(), ps.FrontAddr())
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			time.Sleep(1 * time.Second)
			cancel() // Cancel the request (client-side)
		}()

		_, err = tunnel.DialContext(ctx, "tcp", blackhole)
		if err == nil {
			t.Error("Expected error when context is cancelled, did not receive error")
		} else if _, reason := client.GetDialFailureReason(err); reason != metricsclient.DialFailureContext {
			t.Errorf("Unexpected error: %v", err)
		}

		select {
		case <-tunnel.Done():
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Timed out waiting for tunnel to close")
		}
	}()

	if err := clientmetricstest.ExpectClientDialFailure(metricsclient.DialFailureContext, 1); err != nil {
		t.Error(err)
	}
	if err := ps.Metrics().ExpectServerDialFailure(metricsserver.DialFailureFrontendClose, 1); err != nil {
		t.Error(err)
	}
	resetAllMetrics() // For clean shutdown.
}

func TestProxyDial_RequestCancelled_Concurrent_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	slowServer := newDelayedServer()
	server := httptest.NewServer(slowServer)
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	wg := sync.WaitGroup{}
	dialFn := func(id int, cancelDelay time.Duration) {
		defer wg.Done()

		// run test client
		tunnel, err := createSingleUseGrpcTunnel(context.Background(), ps.FrontAddr())
		if err != nil {
			t.Error(err)
			return
		}

		ctx, cancel := context.WithCancel(context.Background())

		go func() {
			time.Sleep(cancelDelay)
			cancel() // Cancel the request (client-side)
		}()

		c := &http.Client{
			Transport: &http.Transport{
				DialContext: tunnel.DialContext,
			},
		}

		req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		if err != nil {
			t.Error(err)
			return
		}

		c.Do(req) // Errors are expected.

		select {
		case <-tunnel.Done():
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Timed out waiting for tunnel to close")
		}
	}

	// Ensure that tunnels aren't leaked with long-running servers.
	ignoredGoRoutines := []goleak.Option{
		goleak.IgnoreCurrent(),
		goleak.IgnoreTopFunction("google.golang.org/grpc.(*addrConn).resetTransport"),
	}

	const concurrentConns = 50
	wg.Add(concurrentConns)
	for i := 0; i < concurrentConns; i++ {
		cancelDelayMs := rand.Int63n(1000) + 5 /* #nosec G404 */
		go dialFn(i, time.Duration(cancelDelayMs)*time.Millisecond)
	}
	wg.Wait()

	// Wait for the closed connections to propogate
	var endpointConnsErr, goLeaksErr error
	wait.PollImmediate(time.Second, wait.ForeverTestTimeout, func() (done bool, err error) {
		endpointConnsErr = a.Metrics().ExpectAgentEndpointConnections(0)
		goLeaksErr = goleak.Find(ignoredGoRoutines...)
		return endpointConnsErr == nil && goLeaksErr == nil, nil
	})

	if endpointConnsErr != nil {
		t.Errorf("Agent connections leaked: %v", endpointConnsErr)
	}
	if goLeaksErr != nil {
		t.Error(goLeaksErr)
	}

	resetAllMetrics() // For clean shutdown.
}

func TestProxyDial_AgentTimeout_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	func() {
		// Ensure that tunnels aren't leaked with long-running servers.
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent(), goleak.IgnoreTopFunction("google.golang.org/grpc.(*addrConn).resetTransport"))

		// run test client
		tunnel, err := createSingleUseGrpcTunnel(context.Background(), ps.FrontAddr())
		if err != nil {
			t.Fatal(err)
		}

		// Agent should time out after 5 seconds and return a DIAL_RSP with an error.
		_, err = tunnel.DialContext(context.Background(), "tcp", blackhole)
		if err == nil {
			t.Error("Expected error when context is cancelled, did not receive error")
		} else if _, reason := client.GetDialFailureReason(err); reason != metricsclient.DialFailureEndpoint {
			t.Errorf("Unexpected error: %v", err)
		}

		if err := clientmetricstest.ExpectClientDialFailure(metricsclient.DialFailureEndpoint, 1); err != nil {
			t.Error(err)
		}
		if err := ps.Metrics().ExpectServerDialFailure(metricsserver.DialFailureErrorResponse, 1); err != nil {
			t.Error(err)
		}
		if err := a.Metrics().ExpectAgentDialFailure(metricsagent.DialFailureTimeout, 1); err != nil {
			t.Error(err)
		}
		resetAllMetrics() // For clean shutdown.

		select {
		case <-tunnel.Done():
		case <-time.After(wait.ForeverTestTimeout):
			t.Errorf("Timed out waiting for tunnel to close")
		}
	}()
}

func TestProxyHandle_TunnelContextCancelled_GRPC(t *testing.T) {
	expectCleanShutdown(t)

	slowServer := newWaitingServer()
	server := httptest.NewServer(slowServer)
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tunnel, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		<-slowServer.requestReceivedCh // Wait for server to receive request.
		cancel()
		close(slowServer.respondCh) // Unblock server response.
	}()

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
	}

	// TODO: handle case where there is no context on the request.
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	if err != nil {
		t.Error(err)
	}

	_, err = c.Do(req)
	if err == nil {
		t.Error("Expected error when context is cancelled, did not receive error")
	} else if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestProxy_LargeResponse(t *testing.T) {
	expectCleanShutdown(t)

	ctx := context.Background()
	length := 1 << 20 // 1M
	chunks := 10
	server := httptest.NewServer(newSizedServer(length, chunks))
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// run test client
	tunnel, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
	}

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Error(err)
	}
	req.Close = true

	r, err := c.Do(req)
	if err != nil {
		t.Error(err)
	}

	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Error(err)
	}

	if len(data) != length*chunks {
		t.Errorf("expect data length %d; got %d", length*chunks, len(data))
	}
}

func TestBasicProxy_HTTPCONN(t *testing.T) {
	expectCleanShutdown(t)

	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	ps := runHTTPConnProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	conn, err := net.Dial("unix", ps.FrontAddr())
	if err != nil {
		t.Error(err)
	}

	serverURL, _ := url.Parse(server.URL)

	// Send HTTP-Connect request
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", serverURL.Host, "127.0.0.1")
	if err != nil {
		t.Error(err)
	}

	// Parse the HTTP response for Connect
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Errorf("reading HTTP response from CONNECT: %v", err)
	}
	if res.StatusCode != 200 {
		t.Errorf("expect 200; got %d", res.StatusCode)
	}
	if br.Buffered() > 0 {
		t.Error("unexpected extra buffer")
	}

	dialer := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	c := &http.Client{
		Transport: &http.Transport{
			Dial: dialer,
		},
	}

	r, err := c.Get(server.URL)
	if err != nil {
		t.Error(err)
	}

	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Error(err)
	}

	if string(data) != "hello" {
		t.Errorf("expect %v; got %v", "hello", string(data))
	}

}

func TestFailedDNSLookupProxy_HTTPCONN(t *testing.T) {
	expectCleanShutdown(t)

	ps := runHTTPConnProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	conn, err := net.Dial("unix", ps.FrontAddr())
	if err != nil {
		t.Error(err)
	}

	urlString := "http://thissssssxxxxx.com:80"
	serverURL, _ := url.Parse(urlString)

	// Send HTTP-Connect request
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", serverURL.Host, "127.0.0.1")
	if err != nil {
		t.Error(err)
	}

	// Parse the HTTP response for Connect
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Errorf("reading HTTP response from CONNECT: %v", err)
	}

	if res.StatusCode != 200 {
		t.Errorf("expect 200; got %d", res.StatusCode)
	}
	if br.Buffered() > 0 {
		t.Error("unexpected extra buffer")
	}
	dialer := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	c := &http.Client{
		Transport: &http.Transport{
			Dial: dialer,
		},
	}

	resp, err := c.Get(urlString)
	if err != nil {
		t.Error(err)
	}

	if resp.StatusCode != 503 {
		t.Errorf("expect 503; got %d", res.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Error(err)
	}

	if !strings.Contains(string(body), "no such host") {
		t.Errorf("Unexpected error: %v", err)
	}

	if err := ps.Metrics().ExpectServerDialFailure(metricsserver.DialFailureErrorResponse, 1); err != nil {
		t.Error(err)
	}
	if err := a.Metrics().ExpectAgentDialFailure(metricsagent.DialFailureUnknown, 1); err != nil {
		t.Error(err)
	}
	resetAllMetrics() // For clean shutdown.
}

func TestFailedDial_HTTPCONN(t *testing.T) {
	expectCleanShutdown(t)

	server := httptest.NewServer(newEchoServer("hello"))
	server.Close() // cleanup immediately so connections will fail

	ps := runHTTPConnProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	conn, err := net.Dial("unix", ps.FrontAddr())
	if err != nil {
		t.Error(err)
	}

	serverURL, _ := url.Parse(server.URL)

	// Send HTTP-Connect request
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", serverURL.Host, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	// Parse the HTTP response for Connect
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading HTTP response from CONNECT: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("expect 200; got %d", res.StatusCode)
	}

	dialer := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	c := &http.Client{
		Transport: &http.Transport{
			Dial: dialer,
		},
	}

	resp, err := c.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatalf("Expected error reading response body; response=%q", body)
	} else if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Error(err)
	}

	if !strings.Contains(string(body), "connection refused") {
		t.Errorf("Unexpected error: %v", err)
	}

	if err := ps.Metrics().ExpectServerDialFailure(metricsserver.DialFailureErrorResponse, 1); err != nil {
		t.Error(err)
	}
	if err := a.Metrics().ExpectAgentDialFailure(metricsagent.DialFailureUnknown, 1); err != nil {
		t.Error(err)
	}
	resetAllMetrics() // For clean shutdown.
}

func TestProxyHandle_AfterDrain(t *testing.T) {
	expectCleanShutdown(t)

	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	ps := runGRPCProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// Drain agent
	a.Drain()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tunnel, err := createSingleUseGrpcTunnel(ctx, ps.FrontAddr())
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
	}

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("expect %v; got %v", "hello", string(data))
	}
}

func runGRPCProxyServer(t testing.TB) framework.ProxyServer {
	return runGRPCProxyServerWithServerCount(t, 1)
}

func runGRPCProxyServerWithServerCount(t testing.TB, serverCount int) framework.ProxyServer {
	opts := framework.ProxyServerOpts{
		Mode:        server.ModeGRPC,
		ServerCount: serverCount,
	}
	ps, err := Framework.ProxyServerRunner.Start(t, opts)
	if err != nil {
		t.Fatalf("Failed to start gRPC proxy server: %v", err)
	}
	return ps
}

func runHTTPConnProxyServer(t testing.TB) framework.ProxyServer {
	opts := framework.ProxyServerOpts{
		Mode:        server.ModeHTTPConnect,
		ServerCount: 1,
	}
	ps, err := Framework.ProxyServerRunner.Start(t, opts)
	if err != nil {
		t.Fatalf("Failed to start HTTP proxy server: %v", err)
	}
	return ps
}

func runAgent(t testing.TB, addr string) framework.Agent {
	return runAgentWithID(t, uuid.New().String(), addr)
}

func runAgentWithID(t testing.TB, agentID, addr string) framework.Agent {
	opts := framework.AgentOpts{
		AgentID:    agentID,
		ServerAddr: addr,
	}
	a, err := Framework.AgentRunner.Start(t, opts)
	if err != nil {
		t.Fatalf("Failed to start agent: %v", err)
	}
	return a
}

type unresponsiveAgent struct {
	conn *grpc.ClientConn
}

// Connect registers the unresponsive agent with the proxy server.
func (a *unresponsiveAgent) Connect(address string) error {
	agentID := uuid.New().String()
	agentCert := filepath.Join(framework.CertsDir, framework.TestAgentCertFile)
	agentKey := filepath.Join(framework.CertsDir, framework.TestAgentKeyFile)
	caCert := filepath.Join(framework.CertsDir, framework.TestCAFile)
	host, _, _ := net.SplitHostPort(address)
	tlsConfig, err := util.GetClientTLSConfig(caCert, agentCert, agentKey, host, nil)
	if err != nil {
		return err
	}
	conn, err := grpc.Dial(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return err
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		header.AgentID, agentID)
	_, err = agentproto.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		conn.Close()
		return err
	}

	a.conn = conn
	return nil
}

func (a *unresponsiveAgent) Close() {
	a.conn.Close()
}

// waitForConnectedServerCount waits for the agent ClientSet to have the expected number of health
// server connections (HealthyClientsCount).
func waitForConnectedServerCount(t testing.TB, expectedServerCount int, a framework.Agent) {
	t.Helper()
	startTime := time.Now()
	lastUpdate := startTime
	err := wait.PollImmediate(100*time.Millisecond, 5*time.Minute, func() (bool, error) {
		csc, err := a.GetConnectedServerCount()
		if err != nil {
			return false, err
		}
		if csc == expectedServerCount {
			return true, nil
		}
		if time.Since(lastUpdate) > 5*time.Second {
			t.Logf("[%s] Waiting for %d servers, got %d", time.Since(startTime), expectedServerCount, csc)
			lastUpdate = time.Now()
		}
		return false, nil
	})
	if err != nil {
		if csc, err := a.GetConnectedServerCount(); err == nil {
			t.Logf("got %d server connections; expected %d", csc, expectedServerCount)
		}
		t.Fatalf("Error waiting for healthy clients: %v", err)
	}
}

// waitForConnectedAgentCount waits for the proxy server to have the expected number of registered
// agents (backends). This assumes the ProxyServer is using a single ProxyStrategy.
func waitForConnectedAgentCount(t testing.TB, expectedAgentCount int, ps framework.ProxyServer) {
	t.Helper()
	err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		count := ps.ConnectedBackends()
		if count == expectedAgentCount {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		count := ps.ConnectedBackends()
		t.Logf("got %d backends; expected %d", count, expectedAgentCount)
	}
}

func resetAllMetrics() {
	metricsclient.Metrics.Reset()
	metricsserver.Metrics.Reset()
	metricsagent.Metrics.Reset()
}

func expectCleanShutdown(t testing.TB) {
	resetAllMetrics()
	currentGoRoutines := goleak.IgnoreCurrent()
	t.Cleanup(func() {
		goleak.VerifyNone(t, currentGoRoutines, goleak.IgnoreTopFunction("google.golang.org/grpc.(*addrConn).resetTransport"))
		if err := clientmetricstest.ExpectClientDialFailures(nil); err != nil {
			t.Errorf("Unexpected %s metric: %v", "dial_failure_total", err)
		}

		// The following checks are only used with in-process agent/server testing.
		if err := metricstest.DefaultTester.ExpectServerDialFailures(nil); err != nil {
			t.Errorf("Unexpected %s metric: %v", "dial_failure_count", err)
		}
		if err := metricstest.DefaultTester.ExpectAgentDialFailures(nil); err != nil {
			t.Errorf("Unexpected %s metric: %v", "endpoint_dial_failure_total", err)
		}
	})
}

func createSingleUseGrpcTunnel(ctx context.Context, addr string) (client.Tunnel, error) {
	return client.CreateSingleUseGrpcTunnel(ctx, addr,
		grpc.WithContextDialer(
			func(ctx context.Context, addr string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", addr)
			}),
		grpc.WithBlock(),
		grpc.WithReturnConnectionError(),
		grpc.WithTimeout(30*time.Second), // matches http.DefaultTransport dial timeout
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}
