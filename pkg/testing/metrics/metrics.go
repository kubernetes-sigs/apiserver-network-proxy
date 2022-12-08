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

	agentDialFailureHeader = `
# HELP konnectivity_network_proxy_agent_endpoint_dial_failure_total Number of failures dialing the remote endpoint, by reason (example: timeout).
# TYPE konnectivity_network_proxy_agent_endpoint_dial_failure_total counter`
	agentDialFailureSample = `konnectivity_network_proxy_agent_endpoint_dial_failure_total{reason="%s"} %d`
)

func ExpectServerDialFailures(expected map[server.DialFailureReason]int) error {
	expect := serverDialFailureHeader + "\n"
	for r, v := range expected {
		expect += fmt.Sprintf(serverDialFailureSample+"\n", r, v)
	}
	return ExpectMetric(server.Namespace, server.Subsystem, server.DialFailuresMetric, expect)
}

func ExpectServerDialFailure(reason server.DialFailureReason, count int) error {
	return ExpectServerDialFailures(map[server.DialFailureReason]int{reason: count})
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

func ExpectMetric(namespace, subsystem, name, expected string) error {
	fqName := prometheus.BuildFQName(namespace, subsystem, name)
	return promtest.GatherAndCompare(prometheus.DefaultGatherer, strings.NewReader(expected), fqName)
}
