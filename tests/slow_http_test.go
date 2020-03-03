package tests

import (
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
)

// test remote server
type testSlowServer struct {
	interval time.Duration
	received []byte
	err      error
}

func (s *testSlowServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		return
	}

	defer req.Body.Close()

	var buf [4096]byte
	for {
		n, err := req.Body.Read(buf[:])
		s.received = append(s.received, buf[:n]...)
		if err != nil {
			s.err = err
			return
		}
		klog.Info("read a chunk")
		time.Sleep(s.interval)
	}

}

func TestProxy_SlowServer(t *testing.T) {
	s := &testSlowServer{interval: 10 * time.Millisecond}

	server := httptest.NewServer(s)
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
	tunnel, err := client.CreateGrpcTunnel(proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			Dial: tunnel.Dial,
		},
	}

	sent := []byte{}
	for i := 0; i < 10000; i++ {
		sent = append(sent, []byte("helloworld")...)
	}

	r := bytes.NewBuffer(sent)

	resp, err := c.Post(server.URL, "", r)
	if err != nil {
		t.Error(err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expect 200; got %d", resp.StatusCode)
	}

	if string(s.received) != string(sent) {
		t.Errorf("expect len received %d; got %d", len(sent), len(s.received))
	}

	// t.Fail()
}

func TestProxy_Slow_TLSServer(t *testing.T) {
	s := &testSlowServer{interval: 10 * time.Millisecond}

	server := httptest.NewTLSServer(s)
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
	tunnel, err := client.CreateGrpcTunnel(proxy.front, grpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}

	c := &http.Client{
		Transport: &http.Transport{
			Dial:            tunnel.Dial,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	sent := []byte{}
	for i := 0; i < 10000; i++ {
		sent = append(sent, []byte("helloworld")...)
	}

	r := bytes.NewBuffer(sent)

	resp, err := c.Post(server.URL, "", r)
	if err != nil {
		t.Error(err)
	}

	if resp.StatusCode != 200 {
		t.Errorf("expect 200; got %d", resp.StatusCode)
	}

	if string(s.received) != string(sent) {
		t.Errorf("expect len received %d; got %d", len(sent), len(s.received))
	}
}
