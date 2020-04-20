package tests

import (
	"testing"
	"time"
)

func TestReadiness(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	proxy, server, cleanup, err := runGRPCProxyServerWithServerCount(1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ready, _ := server.Readiness.Ready()
	if ready {
		t.Fatalf("expected not ready")
	}

	runAgent(proxy.agent, stopCh)

	// Wait for agent to register on proxy server
	time.Sleep(time.Second)

	ready, _ = server.Readiness.Ready()
	if !ready {
		t.Fatalf("expected ready")
	}
}
