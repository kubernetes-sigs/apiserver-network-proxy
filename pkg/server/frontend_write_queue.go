/*
Copyright 2026 The Kubernetes Authors.

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

package server

import (
	"errors"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

const defaultFrontendWriteChannelSize = 10

// errBackendGone is returned when a write is abandoned because the agent that
// sent the packet went away.
var errBackendGone = errors.New("agent connection closed")

type frontendWriteQueueMetrics struct {
	mu   sync.Mutex
	full bool // guarded by mu

	fullGauge prometheus.Gauge // guarded by mu; keeps Inc and Dec on the same GaugeVec child
	stopped   bool             // guarded by mu
}

// enqueueFrontendWrite gives a frontend a bounded write buffer behind the
// common Frontend.Send path. When writeCh fills, it blocks without dropping
// packets or disconnecting a slow client, until there is room or closed is
// closed. This delays, rather than eliminates, head-of-line blocking of the
// shared agent receive loop. A nil abort never fires; otherwise the packet is
// dropped with errBackendGone once abort is closed. logKV identifies the
// frontend in logs.
func enqueueFrontendWrite(writeCh chan<- *client.Packet, closed, abort <-chan struct{}, state *frontendWriteQueueMetrics, pkt *client.Packet, logKV ...any) error {
	select {
	case <-closed:
		return net.ErrClosed
	case <-abort:
		return errBackendGone
	default:
	}

	select {
	case writeCh <- pkt:
		state.update(writeCh)
		return nil
	case <-closed:
		return net.ErrClosed
	case <-abort:
		return errBackendGone
	default:
	}

	klog.V(2).InfoS("Frontend write channel is full", logKV...)
	blocked := metrics.Metrics.BlockedFrontendWriteChannels()
	blocked.Inc()
	defer blocked.Dec()

	select {
	case writeCh <- pkt:
		state.update(writeCh)
		return nil
	case <-closed:
		return net.ErrClosed
	case <-abort:
		return errBackendGone
	}
}

// update publishes whether writeCh is full. Call it after every send to or
// receive from writeCh.
func (state *frontendWriteQueueMetrics) update(writeCh chan<- *client.Packet) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stopped {
		return
	}
	// Check under the lock even when unchanged: skipping a concurrent update
	// could otherwise let it publish stale occupancy after we return.
	full := len(writeCh) == cap(writeCh)
	if state.full == full {
		return
	}

	state.full = full
	if full {
		state.fullGauge = metrics.Metrics.FullFrontendWriteQueues()
		state.fullGauge.Inc()
	} else {
		state.fullGauge.Dec()
		state.fullGauge = nil
	}
}

// stop releases the durable full-queue accounting. Stream closure releases it
// even when the frontend writer is still blocked.
func (state *frontendWriteQueueMetrics) stop() {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stopped {
		return
	}

	state.stopped = true
	if state.full {
		state.full = false
		state.fullGauge.Dec()
		state.fullGauge = nil
	}
}
