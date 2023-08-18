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
	ps := runGRPCProxyServerWithServerCount(t, 1)
	defer ps.Stop()

	if ps.Ready() {
		t.Fatalf("expected not ready")
	}

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// check the agent connected status
	isAgentReady := a.Ready()
	if !isAgentReady {
		t.Fatalf("expected connection status 'true', got: %t", isAgentReady)
	}
	if !ps.Ready() {
		t.Fatalf("expected ready")
	}
}

func TestHTTPConnServerAndAgentReadiness(t *testing.T) {
	ps := runHTTPConnProxyServer(t)
	defer ps.Stop()

	a := runAgent(t, ps.AgentAddr())
	defer a.Stop()
	waitForConnectedServerCount(t, 1, a)

	// check the agent connected status
	isAgentReady := a.Ready()
	if !isAgentReady {
		t.Fatalf("expected connection status 'true', got: %t", isAgentReady)
	}
}

func TestAgentReadinessWithoutServer(t *testing.T) {
	a := runAgent(t, "localhost:8080")
	defer a.Stop()
	// check the agent connected status
	isAgentReady := a.Ready()
	if isAgentReady {
		t.Fatalf("expected connection status 'false', got: %t", isAgentReady)
	}
}
