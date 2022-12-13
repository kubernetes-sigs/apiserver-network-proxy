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

package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	commonmetrics "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

const (
	Namespace = "konnectivity_network_proxy"
	Subsystem = "client"
)

var (
	// Metrics provides access to all client metrics. The client
	// application is responsible for registering (via Metrics.RegisterMetrics).
	Metrics = newMetrics()
)

// ClientMetrics includes all the metrics of the konnectivity-client.
type ClientMetrics struct {
	registerOnce  sync.Once
	streamPackets *prometheus.CounterVec
	streamErrors  *prometheus.CounterVec
	dialFailures  *prometheus.CounterVec
}

type DialFailureReason string

const (
	DialFailureUnknown DialFailureReason = "unknown"
	// DialFailureTimeout indicates the hard 30 second timeout was hit.
	DialFailureTimeout DialFailureReason = "timeout"
	// DialFailureContext indicates that the context was cancelled or reached it's deadline before
	// the dial response was returned.
	DialFailureContext DialFailureReason = "context"
	// DialFailureEndpoint indicates that the konnectivity-agent was unable to reach the backend endpoint.
	DialFailureEndpoint DialFailureReason = "endpoint"
	// DialFailureDialClosed indicates that the client received a CloseDial response, indicating the
	// connection was closed before the dial could complete.
	DialFailureDialClosed DialFailureReason = "dialclosed"
	// DialFailureTunnelClosed indicates that the client connection was closed before the dial could
	// complete.
	DialFailureTunnelClosed DialFailureReason = "tunnelclosed"
)

func newMetrics() *ClientMetrics {
	// The denominator (total dials started) for both
	// dial_failure_total and dial_duration_seconds is the
	// stream_packets_total (common metric), where segment is
	// "from_client" and packet_type is "DIAL_REQ".
	dialFailures := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: Subsystem,
			Name:      "dial_failure_total",
			Help:      "Number of dial failures observed, by reason (example: remote endpoint error)",
		},
		[]string{
			"reason",
		},
	)
	return &ClientMetrics{
		streamPackets: commonmetrics.MakeStreamPacketsTotalMetric(Namespace, Subsystem),
		streamErrors:  commonmetrics.MakeStreamErrorsTotalMetric(Namespace, Subsystem),
		dialFailures:  dialFailures,
	}
}

// RegisterMetrics registers all metrics with the client application.
func (c *ClientMetrics) RegisterMetrics(r prometheus.Registerer) {
	c.registerOnce.Do(func() {
		r.MustRegister(c.streamPackets)
		r.MustRegister(c.streamErrors)
		r.MustRegister(c.dialFailures)
	})
}

// LegacyRegisterMetrics registers all metrics via MustRegister func.
// TODO: remove this once https://github.com/kubernetes/kubernetes/pull/114293 is available.
func (c *ClientMetrics) LegacyRegisterMetrics(mustRegisterFn func(...prometheus.Collector)) {
	c.registerOnce.Do(func() {
		mustRegisterFn(c.streamPackets)
		mustRegisterFn(c.streamErrors)
		mustRegisterFn(c.dialFailures)
	})
}

// Reset resets the metrics.
func (c *ClientMetrics) Reset() {
	c.streamPackets.Reset()
	c.streamErrors.Reset()
	c.dialFailures.Reset()
}

func (c *ClientMetrics) ObserveDialFailure(reason DialFailureReason) {
	c.dialFailures.WithLabelValues(string(reason)).Inc()
}

func (c *ClientMetrics) ObservePacket(segment commonmetrics.Segment, packetType client.PacketType) {
	commonmetrics.ObservePacket(c.streamPackets, segment, packetType)
}

func (c *ClientMetrics) ObserveStreamErrorNoPacket(segment commonmetrics.Segment, err error) {
	commonmetrics.ObserveStreamErrorNoPacket(c.streamErrors, segment, err)
}

func (c *ClientMetrics) ObserveStreamError(segment commonmetrics.Segment, err error, packetType client.PacketType) {
	commonmetrics.ObserveStreamError(c.streamErrors, segment, err, packetType)
}
