package tests

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/agentserver"
)

func TestProxy_HTTPConnect(t *testing.T) {
	s := newSizedServer(1<<10, 10)
	server := httptest.NewServer(s)
	defer server.Close()

	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, proxyServer, cleanup, err := runGRPCProxyServerWithServerCount(1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	runAgent(proxy.agent, stopCh)

	httpConnServer := httptest.NewServer(&agentserver.Tunnel{
		Server: proxyServer,
	})
	defer httpConnServer.Close()

	// Wait for agent to register on proxy server
	time.Sleep(time.Second)

	httpConnAddr := strings.Trim(httpConnServer.URL, "http://")
	serverAddr := strings.Trim(server.URL, "http://")
	conn, err := net.Dial("tcp", httpConnAddr)
	if err != nil {
		t.Fatal(err)
	}

	msg := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", serverAddr, serverAddr)
	_, err = conn.Write([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}

	// Read CONNECT result
	var buf [4096]byte
	if _, err := conn.Read(buf[:]); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}

	fmt.Println("response 2:")
	fmt.Println(resp.Status)

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 10*1<<10 {
		t.Errorf("expect len(body)=%d; got %d", 10*1<<10, len(body))
	}
}
