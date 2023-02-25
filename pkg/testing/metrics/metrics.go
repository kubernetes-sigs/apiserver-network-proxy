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

package testing

import (
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	agent "sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	server "sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

const (
	serverDialFailureHeader = `
# HELP konnectivity_network_proxy_server_dial_failure_count Number of dial failures observed. Multiple failures can occur for a single dial request.
# TYPE konnectivity_network_proxy_server_dial_failure_count counter`
	serverDialFailureSample = `konnectivity_network_proxy_server_dial_failure_count{reason="%s"} %d`

	serverPendingDialsHeader = `
# HELP konnectivity_network_proxy_server_pending_backend_dials Current number of pending backend dial requests
# TYPE konnectivity_network_proxy_server_pending_backend_dials gauge`
	serverPendingDialsSample = `konnectivity_network_proxy_server_pending_backend_dials{} %d`

	serverReadyBackendsHeader = `
# HELP konnectivity_network_proxy_server_ready_backend_connections Number of konnectivity agent connected to the proxy server
# TYPE konnectivity_network_proxy_server_ready_backend_connections gauge`
	serverReadyBackendsSample = `konnectivity_network_proxy_server_ready_backend_connections{} %d`

	serverEstablishedConnsHeader = `
# HELP konnectivity_network_proxy_server_established_connections Current number of established end-to-end connections (post-dial).
# TYPE konnectivity_network_proxy_server_established_connections gauge`
	serverEstablishedConnsSample = `konnectivity_network_proxy_server_established_connections{} %d`

	agentDialFailureHeader = `
# HELP konnectivity_network_proxy_agent_endpoint_dial_failure_total Number of failures dialing the remote endpoint, by reason (example: timeout).
# TYPE konnectivity_network_proxy_agent_endpoint_dial_failure_total counter`
	agentDialFailureSample = `konnectivity_network_proxy_agent_endpoint_dial_failure_total{reason="%s"} %d`

	agentEndpointConnections = `
# HELP konnectivity_network_proxy_agent_open_endpoint_connections Current number of open endpoint connections.
# TYPE konnectivity_network_proxy_agent_open_endpoint_connections gauge
konnectivity_network_proxy_agent_open_endpoint_connections %d`
)

func ExpectServerDialFailures(expected map[server.DialFailureReason]int) error {
	expect := serverDialFailureHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(serverDialFailureSample+"\n", r, v)
	}
	return ExpectMetric(server.Namespace, server.Subsystem, "dial_failure_count", expect)
}

func ExpectServerDialFailure(reason server.DialFailureReason, count int) error {
	return ExpectServerDialFailures(map[server.DialFailureReason]int{reason: count})
}

func ExpectServerPendingDials(v int) error {
	expect := serverPendingDialsHeader + "\n"
	expect += fmt.Sprintf(serverPendingDialsSample+"\n", v)
	return ExpectMetric(server.Namespace, server.Subsystem, "pending_backend_dials", expect)
}

func ExpectServerReadyBackends(v int) error {
	expect := serverReadyBackendsHeader + "\n"
	expect += fmt.Sprintf(serverReadyBackendsSample+"\n", v)
	return ExpectMetric(server.Namespace, server.Subsystem, "ready_backend_connections", expect)
}

func ExpectServerEstablishedConns(v int) error {
	expect := serverEstablishedConnsHeader + "\n"
	expect += fmt.Sprintf(serverEstablishedConnsSample+"\n", v)
	return ExpectMetric(server.Namespace, server.Subsystem, "established_connections", expect)
}

func ExpectAgentDialFailures(expected map[agent.DialFailureReason]int) error {
	expect := agentDialFailureHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(agentDialFailureSample+"\n", r, v)
	}
	return ExpectMetric(agent.Namespace, agent.Subsystem, "endpoint_dial_failure_total", expect)
}

func ExpectAgentDialFailure(reason agent.DialFailureReason, count int) error {
	return ExpectAgentDialFailures(map[agent.DialFailureReason]int{reason: count})
}

func ExpectAgentEndpointConnections(count int) error {
	expect := fmt.Sprintf(agentEndpointConnections+"\n", count)
	return ExpectMetric(agent.Namespace, agent.Subsystem, "open_endpoint_connections", expect)
}

func ExpectMetric(namespace, subsystem, name, expected string) error {
	fqName := prometheus.BuildFQName(namespace, subsystem, name)
	return promtest.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected), fqName)
}
