package tests

import (
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/agentserver"
)

func TestProxy_HTTPConnect(t *testing.T) {
	s := newSizedServer(1<<10, 1)
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

	msg := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", httpConnAddr, serverAddr)
	fmt.Println("sending ", msg)

	_, err = conn.Write([]byte(msg))
	if err != nil {
		t.Fatal(err)
	}

	var buf [4096]byte
	n, err := conn.Read(buf[:])
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	fmt.Println("response:")
	fmt.Println(string(buf[:n]))

	n, err = conn.Read(buf[:])
	if err != nil {
		t.Fatal(err)
	}

	fmt.Println("response 2:")
	fmt.Println(string(buf[:n]))

	t.Fail()
}
