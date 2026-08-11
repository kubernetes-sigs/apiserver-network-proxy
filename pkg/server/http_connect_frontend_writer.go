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
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

const defaultFrontendWriteChannelSize = 10

var errFrontendWriterStopped = errors.New("frontend writer stopped")
var errFrontendBackendStopped = errors.New("frontend backend stopped")

type frontendWriteQueueMetrics struct {
	mu   sync.Mutex
	full atomic.Bool // transitions are serialized by mu; loads avoid locking on most packets

	fullGauge prometheus.Gauge // guarded by mu; keeps Inc and Dec on the same GaugeVec child
	stopped   bool             // guarded by mu
}

// startFrontendWriter starts the bounded per-connection writer used by
// established HTTP CONNECT tunnels. It is intentionally a no-op for gRPC;
// gRPC frontends retain their existing inline send behavior.
func (c *ProxyClientConnection) startFrontendWriter(queueSize int, frontendWriterDone func()) {
	if c.Mode != ModeHTTPConnect {
		return
	}
	c.frontendWriteStartOnce.Do(func() {
		c.frontendWriterDone = frontendWriterDone
		c.initializeFrontendWriter(queueSize)
		go c.serveFrontendWrites()
	})
}

// initializeFrontendWriter creates the writer channels while the HTTP CONNECT
// request is still pending. Tunnel.ServeHTTP calls it before PendingDial.Add;
// the pending-dial mutex then publishes the initialized fields to the backend
// receive goroutine that handles DIAL_RSP. Initialization is separate from
// startup so ServeHTTP can stop a connection that exits before its writer starts.
func (c *ProxyClientConnection) initializeFrontendWriter(queueSize int) {
	if c.Mode != ModeHTTPConnect || c.frontendWriteCh != nil {
		return
	}
	if queueSize <= 0 {
		queueSize = defaultFrontendWriteChannelSize
	}
	c.frontendWriteCh = make(chan *client.Packet, queueSize)
	c.frontendWriteStopCh = make(chan struct{})
	c.frontendWriteDone = make(chan struct{})
	c.frontendWriteReady = make(chan struct{})
}

func (c *ProxyClientConnection) serveFrontendWrites() {
	defer close(c.frontendWriteDone)
	defer func() {
		if c.frontendWriterDone != nil {
			c.frontendWriterDone()
		}
	}()
	defer c.stopFrontendWriter()

	select {
	case <-c.frontendWriteReady:
	case <-c.frontendWriteStopCh:
		return
	}

	for {
		select {
		case <-c.frontendWriteStopCh:
			return
		default:
		}

		select {
		case pkt := <-c.frontendWriteCh:
			c.updateFrontendWriteQueueMetric()
			if err := c.send(pkt); err != nil {
				klog.ErrorS(err, "Queued send to frontend failed",
					"packetType", pkt.Type,
					"agentID", c.agentID,
					"connectionID", c.connectID,
				)
			}
			if pkt.Type == client.PacketType_CLOSE_RSP {
				return
			}
		case <-c.frontendWriteStopCh:
			return
		}
	}
}

// sendEstablished queues packets for established HTTP CONNECT tunnels. gRPC
// frontends retain their existing synchronous send behavior. Once the bounded
// HTTP CONNECT queue fills, this deliberately blocks and preserves the previous
// serveRecvBackend behavior until the frontend resumes or the connection ends.
func (c *ProxyClientConnection) sendEstablished(pkt *client.Packet) error {
	if c.Mode != ModeHTTPConnect || c.frontendWriteCh == nil {
		return c.send(pkt)
	}

	backendDone := c.backendDone()
	select {
	case <-c.frontendWriteStopCh:
		return errFrontendWriterStopped
	case <-backendDone:
		return errFrontendBackendStopped
	default:
	}

	select {
	case c.frontendWriteCh <- pkt:
		c.updateFrontendWriteQueueMetric()
		return nil
	case <-c.frontendWriteStopCh:
		return errFrontendWriterStopped
	case <-backendDone:
		return errFrontendBackendStopped
	default:
	}

	klog.V(2).InfoS("Frontend write channel is full",
		"agentID", c.agentID,
		"connectionID", c.connectID,
	)
	blockedFrontendWriteChannels := metrics.Metrics.BlockedFrontendWriteChannels()
	blockedFrontendWriteChannels.Inc()
	defer blockedFrontendWriteChannels.Dec()

	select {
	case c.frontendWriteCh <- pkt:
		c.updateFrontendWriteQueueMetric()
		return nil
	case <-c.frontendWriteStopCh:
		return errFrontendWriterStopped
	case <-backendDone:
		return errFrontendBackendStopped
	}
}

func (c *ProxyClientConnection) backendDone() <-chan struct{} {
	if c.backend == nil {
		return nil
	}
	return c.backend.Done()
}

func (c *ProxyClientConnection) updateFrontendWriteQueueMetric() {
	state := &c.frontendWriteMetrics
	full := len(c.frontendWriteCh) == cap(c.frontendWriteCh)
	if state.full.Load() == full {
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stopped {
		return
	}
	// Re-read after taking the lock because the writer and backend receive
	// goroutines can change the channel length concurrently.
	full = len(c.frontendWriteCh) == cap(c.frontendWriteCh)
	if state.full.Load() == full {
		return
	}

	state.full.Store(full)
	if full {
		state.fullGauge = metrics.Metrics.FullFrontendWriteQueues()
		state.fullGauge.Inc()
	} else {
		state.fullGauge.Dec()
		state.fullGauge = nil
	}
}

// stopFrontendWriteQueueMetric releases the durable full-queue accounting.
// Every initialized writer must reach stopFrontendWriter; frontendWriteStopOnce
// guarantees that the metric cleanup and stop signal happen exactly once.
func (c *ProxyClientConnection) stopFrontendWriteQueueMetric() {
	state := &c.frontendWriteMetrics
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.stopped {
		return
	}

	state.stopped = true
	if state.full.Swap(false) {
		state.fullGauge.Dec()
		state.fullGauge = nil
	}
}

func (c *ProxyClientConnection) markFrontendWriteReady() {
	c.frontendWriteReadyOnce.Do(func() { close(c.frontendWriteReady) })
}

func (c *ProxyClientConnection) stopFrontendWriter() {
	if c.frontendWriteStopCh == nil {
		return
	}
	// Stopping cancels queued writes. It is only used when the HTTP connection
	// is already closing; ordinary CLOSE_RSP packets remain ordered in writeCh.
	c.frontendWriteStopOnce.Do(func() {
		c.stopFrontendWriteQueueMetric()
		close(c.frontendWriteStopCh)
	})
}
