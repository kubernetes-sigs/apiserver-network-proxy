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

package testing //nolint:revive

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
# HELP konnectivity_network_proxy_server_ready_backend_connections Number of konnectivity agent connected to the proxy server. DEPRECATED, please use ready_backends
# TYPE konnectivity_network_proxy_server_ready_backend_connections gauge`
	serverReadyBackendsSample = `konnectivity_network_proxy_server_ready_backend_connections{} %d`

	serverTotalReadyBackendsHeader = `
# HELP konnectivity_network_proxy_server_ready_backends Number of konnectivity agent connected to the proxy server
# TYPE konnectivity_network_proxy_server_ready_backends gauge`
	serverTotalReadyBackendsSample = `konnectivity_network_proxy_server_ready_backends{proxy_strategy="%s"} %d`

	serverEstablishedConnsHeader = `
# HELP konnectivity_network_proxy_server_established_connections Current number of established end-to-end connections (post-dial).
# TYPE konnectivity_network_proxy_server_established_connections gauge`
	serverEstablishedConnsSample = `konnectivity_network_proxy_server_established_connections{} %d`

	serverConnectionCloseHeader = `
# HELP konnectivity_network_proxy_server_connection_close_total Number of established end-to-end connections (post-dial) that were closed, by reason for the close.
# TYPE konnectivity_network_proxy_server_connection_close_total counter`
	serverConnectionCloseSample = `konnectivity_network_proxy_server_connection_close_total{reason="%s"} %d`

	agentDialFailureHeader = `
# HELP konnectivity_network_proxy_agent_endpoint_dial_failure_total Number of failures dialing the remote endpoint, by reason (example: timeout).
# TYPE konnectivity_network_proxy_agent_endpoint_dial_failure_total counter`
	agentDialFailureSample = `konnectivity_network_proxy_agent_endpoint_dial_failure_total{reason="%s"} %d`

	agentEndpointConnections = `
# HELP konnectivity_network_proxy_agent_open_endpoint_connections Current number of open endpoint connections.
# TYPE konnectivity_network_proxy_agent_open_endpoint_connections gauge
konnectivity_network_proxy_agent_open_endpoint_connections %d`

	agentConnectionCloseHeader = `
# HELP konnectivity_network_proxy_agent_endpoint_connection_close_total Number of established endpoint connections (post-dial) that were closed, by reason for the close.
# TYPE konnectivity_network_proxy_agent_endpoint_connection_close_total counter`
	agentConnectionCloseSample = `konnectivity_network_proxy_agent_endpoint_connection_close_total{reason="%s"} %d`
)

var DefaultTester = &Tester{}
var _ ServerTester = DefaultTester
var _ AgentTester = DefaultTester

type Tester struct {
	// Endpoint is the metrics endpoint to scrape metrics from. If it is empty, the in-process
	// DefaultGatherer is used.
	Endpoint string
}

type ServerTester interface {
	ExpectServerDialFailures(map[server.DialFailureReason]int) error
	ExpectServerDialFailure(server.DialFailureReason, int) error
	ExpectServerPendingDials(int) error
	ExpectServerReadyBackends(int) error
	ExpectServerEstablishedConns(int) error
	ExpectServerConnectionCloses(map[server.ConnectionCloseReason]int) error
	ExpectServerConnectionClose(server.ConnectionCloseReason, int) error
}

type AgentTester interface {
	ExpectAgentDialFailures(map[agent.DialFailureReason]int) error
	ExpectAgentDialFailure(agent.DialFailureReason, int) error
	ExpectAgentEndpointConnections(int) error
	ExpectAgentConnectionCloses(map[agent.ConnectionCloseReason]int) error
	ExpectAgentConnectionClose(agent.ConnectionCloseReason, int) error
}

func (t *Tester) ExpectServerDialFailures(expected map[server.DialFailureReason]int) error {
	expect := serverDialFailureHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(serverDialFailureSample+"\n", r, v)
	}
	return t.ExpectMetric(server.Namespace, server.Subsystem, "dial_failure_count", expect)
}

func (t *Tester) ExpectServerDialFailure(reason server.DialFailureReason, count int) error {
	return t.ExpectServerDialFailures(map[server.DialFailureReason]int{reason: count})
}

func (t *Tester) ExpectServerPendingDials(v int) error {
	expect := serverPendingDialsHeader + "\n"
	expect += fmt.Sprintf(serverPendingDialsSample+"\n", v)
	return t.ExpectMetric(server.Namespace, server.Subsystem, "pending_backend_dials", expect)
}

func (t *Tester) ExpectServerReadyBackends(v int) error {
	expect := serverReadyBackendsHeader + "\n"
	expect += fmt.Sprintf(serverReadyBackendsSample+"\n", v)
	return t.ExpectMetric(server.Namespace, server.Subsystem, "ready_backend_connections", expect)
}

func (t *Tester) ExpectServerTotalReadyBackends(expected map[string]int) error {
	expect := serverTotalReadyBackendsHeader + "\n"
	for proxyStrategy, numOfBackends := range expected {
		expect += fmt.Sprintf(serverTotalReadyBackendsSample+"\n", proxyStrategy, numOfBackends)
	}
	return t.ExpectMetric(server.Namespace, server.Subsystem, "ready_backends", expect)
}

func (t *Tester) ExpectServerEstablishedConns(v int) error {
	expect := serverEstablishedConnsHeader + "\n"
	expect += fmt.Sprintf(serverEstablishedConnsSample+"\n", v)
	return t.ExpectMetric(server.Namespace, server.Subsystem, "established_connections", expect)
}

func (t *Tester) ExpectServerConnectionCloses(expected map[server.ConnectionCloseReason]int) error {
	expect := serverConnectionCloseHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(serverConnectionCloseSample+"\n", r, v)
	}
	return t.ExpectMetric(server.Namespace, server.Subsystem, "connection_close_total", expect)
}

func (t *Tester) ExpectServerConnectionClose(reason server.ConnectionCloseReason, count int) error {
	return t.ExpectServerConnectionCloses(map[server.ConnectionCloseReason]int{reason: count})
}

func (t *Tester) ExpectAgentDialFailures(expected map[agent.DialFailureReason]int) error {
	expect := agentDialFailureHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(agentDialFailureSample+"\n", r, v)
	}
	return t.ExpectMetric(agent.Namespace, agent.Subsystem, "endpoint_dial_failure_total", expect)
}

func (t *Tester) ExpectAgentDialFailure(reason agent.DialFailureReason, count int) error {
	return t.ExpectAgentDialFailures(map[agent.DialFailureReason]int{reason: count})
}

func (t *Tester) ExpectAgentEndpointConnections(count int) error {
	expect := fmt.Sprintf(agentEndpointConnections+"\n", count)
	return t.ExpectMetric(agent.Namespace, agent.Subsystem, "open_endpoint_connections", expect)
}

func (t *Tester) ExpectAgentConnectionCloses(expected map[agent.ConnectionCloseReason]int) error {
	expect := agentConnectionCloseHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(agentConnectionCloseSample+"\n", r, v)
	}
	return t.ExpectMetric(agent.Namespace, agent.Subsystem, "endpoint_connection_close_total", expect)
}

func (t *Tester) ExpectAgentConnectionClose(reason agent.ConnectionCloseReason, count int) error {
	return t.ExpectAgentConnectionCloses(map[agent.ConnectionCloseReason]int{reason: count})
}

func (t *Tester) ExpectMetric(namespace, subsystem, name, expected string) error {
	fqName := prometheus.BuildFQName(namespace, subsystem, name)
	if t.Endpoint == "" {
		return promtest.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected), fqName)
	}
	return promtest.ScrapeAndCompare(t.Endpoint, strings.NewReader(expected), fqName)
}
