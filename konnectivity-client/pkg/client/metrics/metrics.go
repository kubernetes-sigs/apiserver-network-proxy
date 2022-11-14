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
}

func newMetrics() *ClientMetrics {
	return &ClientMetrics{
		streamPackets: commonmetrics.MakeStreamPacketsTotalMetric(Namespace, Subsystem),
		streamErrors:  commonmetrics.MakeStreamErrorsTotalMetric(Namespace, Subsystem),
	}
}

// RegisterMetrics registers all metrics with the client application.
func (c *ClientMetrics) RegisterMetrics(r prometheus.Registerer, namespace, subsystem string) {
	c.registerOnce.Do(func() {
		r.MustRegister(c.streamPackets)
		r.MustRegister(c.streamErrors)
	})
}

// LegacyRegisterMetrics registers all metrics via MustRegister func.
// TODO: remove this once https://github.com/kubernetes/kubernetes/pull/114293 is available.
func (c *ClientMetrics) LegacyRegisterMetrics(mustRegisterFn func(...prometheus.Collector), namespace, subsystem string) {
	c.registerOnce.Do(func() {
		mustRegisterFn(c.streamPackets)
		mustRegisterFn(c.streamErrors)
	})
}

// Reset resets the metrics.
func (c *ClientMetrics) Reset() {
	c.streamPackets.Reset()
	c.streamErrors.Reset()
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
