/*
Copyright 2017 The Kubernetes Authors.

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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

type Direction string

const (
	namespace = "konnectivity_network_proxy"
	subsystem = "agent"

	// DirectionToServer indicates that the agent attempts to send a packet
	// to the proxy server.
	DirectionToServer Direction = "to_server"
	// DirectionFromServer indicates that the agent attempts to receive a
	// packet from the proxy server.
	DirectionFromServer Direction = "from_server"
)

var (
	// Metrics provides access to all dial metrics.
	Metrics = newAgentMetrics()
)

// AgentMetrics includes all the metrics of the proxy agent.
type AgentMetrics struct {
	failures *prometheus.CounterVec
}

// newAgentMetrics create a new AgentMetrics, configured with default metric names.
func newAgentMetrics() *AgentMetrics {
	failures := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "server_connection_failure_count",
			Help:      "Count of failures to send to or receive from the proxy server, labeled by the direction (from_server or to_server)",
		},
		[]string{"direction"},
	)
	prometheus.MustRegister(failures)
	return &AgentMetrics{failures: failures}
}

// Reset resets the metrics.
func (a *AgentMetrics) Reset() {
	a.failures.Reset()
}

// ObserveFailure records a failure to send to or receive from the proxy
// server, labeled by the direction.
func (a *AgentMetrics) ObserveFailure(direction Direction) {
	a.failures.WithLabelValues(string(direction)).Inc()
}
