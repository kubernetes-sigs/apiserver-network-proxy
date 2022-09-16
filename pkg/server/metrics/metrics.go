/*
Copyright 2020 The Kubernetes Authors.

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
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	Namespace = "konnectivity_network_proxy"
	Subsystem = "server"

	DialLatencyMetric        = "dial_duration_seconds"
	FrontendLatencyMetric    = "frontend_write_duration_seconds"
	GRPCConnectionsMetric    = "grpc_connections"
	HTTPConnectionsMetric    = "http_connections"
	BackendConnectionsMetric = "ready_backend_connections"
	PendingDialsMetric       = "pending_backend_dials"
	FullRecvChannelMetric    = "full_receive_channels"
	DialFailuresMetric       = "dial_failure_count"

	// Proxy is the ProxyService method used to handle incoming streams.
	Proxy = "Proxy"
	// Connect is the AgentService method used to establish next hop.
	Connect = "Connect"
)

var (
	// Use buckets ranging from 10 ns to 12.5 seconds.
	latencyBuckets = []float64{0.000001, 0.00001, 0.0001, 0.005, 0.025, 0.1, 0.5, 2.5, 12.5}

	// Metrics provides access to all dial metrics.
	Metrics = newServerMetrics()
)

// ServerMetrics includes all the metrics of the proxy server.
type ServerMetrics struct {
	latencies         *prometheus.HistogramVec
	frontendLatencies *prometheus.HistogramVec
	connections       *prometheus.GaugeVec
	httpConnections   prometheus.Gauge
	backend           *prometheus.GaugeVec
	pendingDials      *prometheus.GaugeVec
	fullRecvChannels  *prometheus.GaugeVec
	dialFailures      *prometheus.CounterVec
}

// newServerMetrics create a new ServerMetrics, configured with default metric names.
func newServerMetrics() *ServerMetrics {
	latencies := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      DialLatencyMetric,
			Help:      "Latency of dial to the remote endpoint in seconds",
			Buckets:   latencyBuckets,
		},
		[]string{},
	)
	frontendLatencies := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      FrontendLatencyMetric,
			Help:      "Latency of write to the frontend in seconds",
			Buckets:   latencyBuckets,
		},
		[]string{},
	)
	connections := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      GRPCConnectionsMetric,
			Help:      "Number of current grpc connections, partitioned by service method.",
		},
		[]string{
			"service_method",
		},
	)
	httpConnections := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      HTTPConnectionsMetric,
			Help:      "Number of current HTTP CONNECT connections",
		},
	)
	backend := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      BackendConnectionsMetric,
			Help:      "Number of konnectivity agent connected to the proxy server",
		},
		[]string{},
	)
	pendingDials := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      PendingDialsMetric,
			Help:      "Current number of pending backend dial requests",
		},
		[]string{},
	)
	fullRecvChannels := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      FullRecvChannelMetric,
			Help:      "Number of current connections blocked by a full receive channel, partitioned by service method.",
		},
		[]string{
			"service_method",
		},
	)
	dialFailures := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      DialFailuresMetric,
			Help:      "Number of dial failures observed. Multiple failures can occur for a single dial request.",
		},
		[]string{
			"reason",
		},
	)

	prometheus.MustRegister(latencies)
	prometheus.MustRegister(frontendLatencies)
	prometheus.MustRegister(connections)
	prometheus.MustRegister(httpConnections)
	prometheus.MustRegister(backend)
	prometheus.MustRegister(pendingDials)
	prometheus.MustRegister(fullRecvChannels)
	prometheus.MustRegister(dialFailures)
	return &ServerMetrics{
		latencies:         latencies,
		frontendLatencies: frontendLatencies,
		connections:       connections,
		httpConnections:   httpConnections,
		backend:           backend,
		pendingDials:      pendingDials,
		fullRecvChannels:  fullRecvChannels,
		dialFailures:      dialFailures,
	}
}

// Reset resets the metrics.
func (a *ServerMetrics) Reset() {
	a.latencies.Reset()
	a.frontendLatencies.Reset()
	a.connections.Reset()
	a.backend.Reset()
	a.pendingDials.Reset()
	a.fullRecvChannels.Reset()
	a.dialFailures.Reset()
}

// ObserveDialLatency records the latency of dial to the remote endpoint.
func (a *ServerMetrics) ObserveDialLatency(elapsed time.Duration) {
	a.latencies.WithLabelValues().Observe(elapsed.Seconds())
}

// ObserveFrontendWriteLatency records the latency of dial to the remote endpoint.
func (a *ServerMetrics) ObserveFrontendWriteLatency(elapsed time.Duration) {
	a.frontendLatencies.WithLabelValues().Observe(elapsed.Seconds())
}

// ConnectionInc increments a new grpc client connection.
func (a *ServerMetrics) ConnectionInc(serviceMethod string) {
	a.connections.With(prometheus.Labels{"service_method": serviceMethod}).Inc()
}

// ConnectionDec decrements a finished grpc client connection.
func (a *ServerMetrics) ConnectionDec(serviceMethod string) {
	a.connections.With(prometheus.Labels{"service_method": serviceMethod}).Dec()
}

// HTTPConnectionDec increments a new HTTP CONNECTION connection.
func (a *ServerMetrics) HTTPConnectionInc() { a.httpConnections.Inc() }

// HTTPConnectionDec decrements a finished HTTP CONNECTION connection.
func (a *ServerMetrics) HTTPConnectionDec() { a.httpConnections.Dec() }

// SetBackendCount sets the number of backend connection.
func (a *ServerMetrics) SetBackendCount(count int) {
	a.backend.WithLabelValues().Set(float64(count))
}

// SetPendingDialCount sets the number of pending dials.
func (a *ServerMetrics) SetPendingDialCount(count int) {
	a.pendingDials.WithLabelValues().Set(float64(count))
}

// FullRecvChannel retrieves the metric for counting full receive channels.
func (a *ServerMetrics) FullRecvChannel(serviceMethod string) prometheus.Gauge {
	return a.fullRecvChannels.With(prometheus.Labels{"service_method": serviceMethod})
}

type DialFailureReason string

const (
	DialFailureErrorResponse        DialFailureReason = "error_response"        // Dial failure reported by the agent back to the server.
	DialFailureUnrecognizedResponse DialFailureReason = "unrecognized_response" // Dial repsonse received for unrecognozide dial ID.
	DialFailureSendResponse         DialFailureReason = "send_rsp"              // Successful dial response from agent, but failed to send to frontend.
	DialFailureBackendClose         DialFailureReason = "backend_close"         // Received a DIAL_CLS from the backend before the dial completed.
	DialFailureFrontendClose        DialFailureReason = "frontend_close"        // Received a DIAL_CLS from the frontend before the dial completed.
)

func (a *ServerMetrics) ObserveDialFailure(reason DialFailureReason) {
	a.dialFailures.With(prometheus.Labels{"reason": string(reason)}).Inc()
}
