package tests

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

// The default IdleConnTimeout is 90s and will cause all Dials
// using the same Dialer to reuse the initial connection instead
// of calling the Dial function again. Setting it to 0 forces a new
// Dial on each new http client

const IDLECONNTIMEOUT = 0

func TestProxy_Concurrency_GRPCReuseTunnel(t *testing.T) {
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
	tunnel, err := client.CreateReusableGrpcTunnel(proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()

	var wg sync.WaitGroup
	verify := func() {
		defer wg.Done()

		c := &http.Client{
			Transport: &http.Transport{
				Dial:            tunnel.Dial,
				IdleConnTimeout: IDLECONNTIMEOUT,
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

func TestProxy_Concurrency_Different_Destination_GRPCReuseTunnel(t *testing.T) {
	length1 := 1 << 20
	length2 := 1 << 10
	chunks := 10
	server1 := httptest.NewServer(newSizedServer(length1, chunks))
	server2 := httptest.NewServer(newSizedServer(length2, chunks))
	defer server1.Close()
	defer server2.Close()

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
	tunnel, err := client.CreateReusableGrpcTunnel(proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()

	var wg sync.WaitGroup
	verify := func(l int, s *httptest.Server) {
		defer wg.Done()

		c := &http.Client{
			Transport: &http.Transport{
				Dial:            tunnel.Dial,
				IdleConnTimeout: IDLECONNTIMEOUT,
			},
		}

		r, err := c.Get(s.URL)
		if err != nil {
			t.Error(err)
		}

		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		defer r.Body.Close()

		if len(data) != l*chunks {
			t.Errorf("expect data length %d; got %d", l*chunks, len(data))
		}
	}

	wg.Add(2)
	go verify(length1, server1)
	go verify(length2, server2)
	wg.Wait()
}

func TestProxy_SequentialGRPCReuseTunnel(t *testing.T) {
	length1 := 1 << 20
	length2 := 1 << 10
	chunks := 10
	server1 := httptest.NewServer(newSizedServer(length1, chunks))
	server2 := httptest.NewServer(newSizedServer(length2, chunks))
	defer server1.Close()
	defer server2.Close()

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
	tunnel, err := client.CreateReusableGrpcTunnel(proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()

	verify := func(l int, s *httptest.Server) {

		c := &http.Client{
			Transport: &http.Transport{
				Dial:            tunnel.Dial,
				IdleConnTimeout: IDLECONNTIMEOUT,
			},
		}

		r, err := c.Get(s.URL)
		if err != nil {
			t.Error(err)
		}

		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		defer r.Body.Close()

		if len(data) != l*chunks {
			t.Errorf("expect data length %d; got %d", l*chunks, len(data))
		}
	}

	verify(length1, server1)
	verify(length2, server2)
}

func TestProxy_Concurrency_SingleUseDialer_Reuse_Fail(t *testing.T) {
	length := 1 << 10
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
	tunnel, err := client.CreateSingleUseGrpcTunnel(proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	firstDial := func() {
		defer wg.Done()

		c := &http.Client{
			Transport: &http.Transport{
				Dial:            tunnel.Dial,
				IdleConnTimeout: IDLECONNTIMEOUT,
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

	consecutiveDial := func() {
		defer wg.Done()

		c := &http.Client{
			Transport: &http.Transport{
				Dial:            tunnel.Dial,
				IdleConnTimeout: IDLECONNTIMEOUT,
			},
		}

		_, err := c.Get(server.URL)
		if err == nil {
			t.Error("Expected multiple dials on singleUseGRPCTunnel to fail")
		}
	}

	wg.Add(2)
	go firstDial()
	// Wait for the first connection to be established
	time.Sleep(time.Second)
	go consecutiveDial()
	wg.Wait()
}

func TestProxy_Concurrency_SingleUseDialer(t *testing.T) {
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

	var wg sync.WaitGroup
	verify := func() {
		defer wg.Done()

		tunnel, err := client.CreateSingleUseGrpcTunnel(proxy.front, grpc.WithInsecure())

		// run test client
		if err != nil {
			t.Fatal(err)
		}

		c := &http.Client{
			Transport: &http.Transport{
				Dial:            tunnel.Dial,
				IdleConnTimeout: IDLECONNTIMEOUT,
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
