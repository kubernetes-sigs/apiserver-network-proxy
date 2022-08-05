package tests

import (
	"testing"
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

	clientset := runAgent(proxy.agent, stopCh)
	waitForHealthyClients(t, 1, clientset)

	ready, _ = server.Readiness.Ready()
	if !ready {
		t.Fatalf("expected ready")
	}
}
