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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	clientproto "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	agentproto "sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

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

func (s *testServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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

func (s *waitingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	close(s.requestReceivedCh)
	<-s.respondCh // Wait for permission to respond.
	w.Write([]byte("hello"))
}

func TestBasicProxy_GRPC(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ctx := context.Background()
	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	// run test client
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
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

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}

	if string(data) != "hello" {
		t.Errorf("expect %v; got %v", "hello", string(data))
	}
}

func TestProxyHandleDialError_GRPC(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ctx := context.Background()
	invalidServer := httptest.NewServer(newEchoServer("hello"))

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	// run test client
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
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
}

func TestProxyHandle_DoneContext_GRPC(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	// run test client
	ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	_, err = client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
	if err == nil {
		t.Error("Expected error when context is cancelled, did not receive error")
	} else if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestProxyHandle_SlowContext_GRPC(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	slowServer := newWaitingServer()
	server := httptest.NewServer(slowServer)
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	// run test client
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		<-ctx.Done() // Wait for context to time out.
		close(slowServer.respondCh)
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
	} else if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestProxyHandle_ContextCancelled_GRPC(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	slowServer := newWaitingServer()
	server := httptest.NewServer(slowServer)
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runGRPCProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	// run test client
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
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
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ctx := context.Background()
	length := 1 << 20 // 1M
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

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	// run test client
	tunnel, err := client.CreateSingleUseGrpcTunnel(ctx, proxy.front, grpc.WithInsecure())
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

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}

	if len(data) != length*chunks {
		t.Errorf("expect data length %d; got %d", length*chunks, len(data))
	}
}

func TestBasicProxy_HTTPCONN(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runHTTPConnProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	conn, err := net.Dial("tcp", proxy.front)
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

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}

	if string(data) != "hello" {
		t.Errorf("expect %v; got %v", "hello", string(data))
	}

}

func TestFailedDial_HTTPCONN(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	server := httptest.NewServer(newEchoServer("hello"))
	server.Close() // cleanup immediately so connections will fail

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, cleanup, err := runHTTPConnProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	conn, err := net.Dial("tcp", proxy.front)
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

	dialer := func(network, addr string) (net.Conn, error) {
		return conn, nil
	}

	c := &http.Client{
		Transport: &http.Transport{
			Dial: dialer,
		},
	}

	_, err = c.Get(server.URL)
	if err == nil {
		t.Error(err)
	} else if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("Unexpected error: %v", err)
	}

	err = wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		return proxy.getActiveHTTPConnectConns() == 0, nil
	})
	if err != nil {
		t.Errorf("while waiting for connection to be closed: %v", err)
	}
}

func localAddr(addr net.Addr) string {
	return addr.String()
}

type proxy struct {
	server *server.ProxyServer
	front  string
	agent  string

	getActiveHTTPConnectConns func() int
}

func runGRPCProxyServer() (proxy, func(), error) {
	p, _, cleanup, err := runGRPCProxyServerWithServerCount(1)
	return p, cleanup, err
}

func runGRPCProxyServerWithServerCount(serverCount int) (proxy, *server.ProxyServer, func(), error) {
	var proxy proxy
	var err error
	var lis, lis2 net.Listener

	server := server.NewProxyServer(uuid.New().String(), []server.ProxyStrategy{server.ProxyStrategyDefault}, serverCount, &server.AgentTokenAuthenticationOptions{})
	grpcServer := grpc.NewServer()
	agentServer := grpc.NewServer()
	cleanup := func() {
		if lis != nil {
			lis.Close()
		}
		if lis2 != nil {
			lis2.Close()
		}
		agentServer.Stop()
		grpcServer.Stop()
	}

	clientproto.RegisterProxyServiceServer(grpcServer, server)
	lis, err = net.Listen("tcp", "")
	if err != nil {
		return proxy, server, cleanup, err
	}
	go grpcServer.Serve(lis)
	proxy.front = localAddr(lis.Addr())

	agentproto.RegisterAgentServiceServer(agentServer, server)
	lis2, err = net.Listen("tcp", "")
	if err != nil {
		return proxy, server, cleanup, err
	}
	go func() {
		agentServer.Serve(lis2)
	}()
	proxy.agent = localAddr(lis2.Addr())
	proxy.server = server

	return proxy, server, cleanup, nil
}

func runHTTPConnProxyServer() (proxy, func(), error) {
	ctx := context.Background()
	var proxy proxy
	s := server.NewProxyServer(uuid.New().String(), []server.ProxyStrategy{server.ProxyStrategyDefault}, 0, &server.AgentTokenAuthenticationOptions{})
	agentServer := grpc.NewServer()

	agentproto.RegisterAgentServiceServer(agentServer, s)
	lis, err := net.Listen("tcp", "")
	if err != nil {
		return proxy, func() {}, err
	}
	go func() {
		agentServer.Serve(lis)
	}()
	proxy.agent = localAddr(lis.Addr())

	// http-connect
	active := int32(0)
	proxy.getActiveHTTPConnectConns = func() int { return int(atomic.LoadInt32(&active)) }
	handler := &server.Tunnel{
		Server: s,
	}
	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&active, 1)
			defer atomic.AddInt32(&active, -1)
			handler.ServeHTTP(w, r)
		}),
	}
	lis2, err := net.Listen("tcp", "")
	if err != nil {
		return proxy, func() {}, err
	}
	proxy.front = localAddr(lis2.Addr())

	go func() {
		err := httpServer.Serve(lis2)
		if err != nil {
			fmt.Println("http connect server error: ", err)
		}
	}()

	cleanup := func() {
		lis.Close()
		lis2.Close()
		httpServer.Shutdown(ctx)
	}
	proxy.server = s

	return proxy, cleanup, nil
}

func runAgent(addr string, stopCh <-chan struct{}) *agent.ClientSet {
	return runAgentWithID(uuid.New().String(), addr, stopCh)
}

func runAgentWithID(agentID, addr string, stopCh <-chan struct{}) *agent.ClientSet {
	cc := agent.ClientSetConfig{
		Address:       addr,
		AgentID:       agentID,
		SyncInterval:  100 * time.Millisecond,
		ProbeInterval: 100 * time.Millisecond,
		DialOptions:   []grpc.DialOption{grpc.WithInsecure()},
	}
	client := cc.NewAgentClientSet(stopCh)
	client.Serve()
	return client
}

func waitForHealthyClients(t *testing.T, expectedClientCount int, clientset *agent.ClientSet) {
	err := wait.Poll(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		hc := clientset.HealthyClientsCount()
		if hc == expectedClientCount {
			return true, nil
		}
		cc := clientset.ClientsCount()
		t.Logf("got %d clients, %d of them are healthy", cc, hc)
		return false, nil
	})
	if err != nil {
		t.Fatalf("Error waiting for healthy clients: %v", err)
	}
}

// waitForBackends waits for the proxy server to have the expected number of registered backends.
// This assumes the ProxyServer is using a single ProxyStrategy.
func waitForBackends(t *testing.T, expectedBackendCount int, proxy *server.ProxyServer) {
	err := wait.Poll(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		count := proxy.BackendManagers[0].NumBackends()
		if count == expectedBackendCount {
			return true, nil
		}
		t.Logf("got %d backends", count)
		return false, nil
	})
	if err != nil {
		t.Fatalf("Error waiting for backend count: %v", err)
	}
}
