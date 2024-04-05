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
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

type tcpLB struct {
	t        *testing.T
	mu       sync.RWMutex
	backends []string
}

func ioCopy(wc io.WriteCloser, r io.Reader) {
	defer wc.Close()
	io.Copy(wc, r)
}

func (lb *tcpLB) handleConnection(in net.Conn, backend string) {
	out, err := net.Dial("tcp", backend)
	if err != nil {
		lb.t.Log(err)
		return
	}
	go ioCopy(out, in)
	go ioCopy(in, out)
}

func (lb *tcpLB) serve(stopCh chan struct{}) string {
	ln, err := net.Listen("tcp", "127.0.0.1:")
	if err != nil {
		log.Fatalf("failed to bind: %s", err)
	}

	go func() {
		<-stopCh
		ln.Close()
	}()

	go func() {
		for {
			select {
			case <-stopCh:
				return
			default:
			}
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("failed to accept: %s", err)
				continue
			}
			// go lb.handleConnection(conn, lb.randomBackend())
			back := lb.randomBackend()
			go lb.handleConnection(conn, back)
		}
	}()

	return ln.Addr().String()
}

func (lb *tcpLB) addBackend(backend string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.backends = append(lb.backends, backend)

}

func (lb *tcpLB) removeBackend(backend string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for i := range lb.backends {
		if lb.backends[i] == backend {
			lb.backends = append(lb.backends[:i], lb.backends[i+1:]...)
			return
		}
	}
}

func (lb *tcpLB) randomBackend() string {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	i := rand.Intn(len(lb.backends)) /* #nosec G404 */
	return lb.backends[i]
}

const haServerCount = 3

func setupHAProxyServer(t *testing.T) []framework.ProxyServer {
	ps := make([]framework.ProxyServer, haServerCount)
	for i := 0; i < haServerCount; i++ {
		ps[i] = runGRPCProxyServerWithServerCount(t, haServerCount)
	}
	return ps
}

func TestBasicHAProxyServer_GRPC(t *testing.T) {
	server := httptest.NewServer(newEchoServer("hello"))
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy := setupHAProxyServer(t)

	lb := tcpLB{
		backends: []string{
			proxy[0].AgentAddr(),
			proxy[1].AgentAddr(),
			proxy[2].AgentAddr(),
		},
		t: t,
	}
	lbAddr := lb.serve(stopCh)

	a := runAgent(t, lbAddr)
	defer a.Stop()
	waitForConnectedServerCount(t, 3, a)

	// run test client
	testProxyServer(t, proxy[0].FrontAddr(), server.URL)
	testProxyServer(t, proxy[1].FrontAddr(), server.URL)
	testProxyServer(t, proxy[2].FrontAddr(), server.URL)

	t.Logf("basic HA proxy server test passed")

	// interrupt the HA server
	lb.removeBackend(proxy[0].AgentAddr())
	proxy[0].Stop()

	// give the agent some time to detect the disconnection
	waitForConnectedServerCount(t, 2, a)

	proxy4 := runGRPCProxyServerWithServerCount(t, haServerCount)
	lb.addBackend(proxy4.AgentAddr())
	defer func() {
		proxy[1].Stop()
		proxy[2].Stop()
		proxy4.Stop()
	}()

	// wait for the new server to be connected.
	waitForConnectedServerCount(t, 3, a)

	// run test client
	testProxyServer(t, proxy[1].FrontAddr(), server.URL)
	testProxyServer(t, proxy[2].FrontAddr(), server.URL)
	testProxyServer(t, proxy4.FrontAddr(), server.URL)
}

func testProxyServer(t *testing.T, front string, target string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Close the tunnel
	tunnel, err := createSingleUseGrpcTunnel(ctx, front)
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			DialContext: tunnel.DialContext,
		},
		Timeout: 1 * time.Second,
	}

	r, err := c.Get(target)
	if err != nil {
		t.Fatal(err)
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
