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
	"testing"

	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

func TestServerRestartAgentReconnect(t *testing.T) {
	agentPort, err := framework.FreePorts(1)
	if err != nil {
		t.Fatal(err)
	}
	opts := framework.ProxyServerOpts{
		Mode:        server.ModeGRPC,
		ServerCount: 1,
		AgentPort:   agentPort[0],
	}
	ps, err := Framework.ProxyServerRunner.Start(t, opts)
	if err != nil {
		t.Fatalf("Failed to start gRPC proxy server: %v", err)
	}

	a := runAgentWithID(t, "test-id", ps.AgentAddr())
	defer a.Stop()

	waitForConnectedServerCount(t, 1, a)
	ps.Stop()
	waitForConnectedServerCount(t, 0, a)

	ps2, err := Framework.ProxyServerRunner.Start(t, opts)
	if err != nil {
		t.Fatalf("Failed to start gRPC proxy server: %v", err)
	}
	defer ps2.Stop()

	waitForConnectedServerCount(t, 1, a)
}
