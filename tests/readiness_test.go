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

import "testing"

func TestGRPCServerAndAgentReadiness(t *testing.T) {
	proxy, server, cleanup, err := runGRPCProxyServerWithServerCount(1)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ready, _ := server.Readiness.Ready()
	if ready {
		t.Fatalf("expected not ready")
	}

	ai := runAgent(t, proxy.agent)
	defer ai.Stop()
	waitForConnectedServerCount(t, 1, ai)

	// check the agent connected status
	isAgentReady := ai.Ready()
	if !isAgentReady {
		t.Fatalf("expected connection status 'true', got: %t", isAgentReady)
	}
	ready, _ = server.Readiness.Ready()
	if !ready {
		t.Fatalf("expected ready")
	}
}

func TestHTTPConnServerAndAgentReadiness(t *testing.T) {
	proxy, cleanup, err := runHTTPConnProxyServer()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	ai := runAgent(t, proxy.agent)
	defer ai.Stop()
	waitForConnectedServerCount(t, 1, ai)

	// check the agent connected status
	isAgentReady := ai.Ready()
	if !isAgentReady {
		t.Fatalf("expected connection status 'true', got: %t", isAgentReady)
	}
}

func TestAgentReadinessWithoutServer(t *testing.T) {
	ai := runAgent(t, "localhost:8080")
	defer ai.Stop()
	// check the agent connected status
	isAgentReady := ai.Ready()
	if isAgentReady {
		t.Fatalf("expected connection status 'false', got: %t", isAgentReady)
	}
}
