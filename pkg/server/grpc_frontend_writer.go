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
	"sync"

	"k8s.io/klog/v2"

	commonmetrics "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

// grpcFrontendStream queues packets for a gRPC frontend and sends them from
// its own goroutine, so a frontend that stops reading blocks the agent receive
// loop only once its queue is full.
//
// It is not an io.Closer, so Frontend.closeWithBackend leaves it open and the
// CLOSE_RSP that serveRecvBackend sends when the agent goes away still reaches
// the frontend.
type grpcFrontendStream struct {
	client.ProxyService_ProxyServer

	// writeCh is never closed, so stop can race with Send.
	writeCh      chan *client.Packet
	closed       chan struct{}
	stopOnce     sync.Once
	writerDone   chan struct{}
	writeMetrics frontendWriteQueueMetrics
}

var _ ProxyStream = &grpcFrontendStream{}

// newGRPCFrontendStream starts the writer for stream. A negative queueSize
// selects the default. The caller must call drain before the handler returns.
func newGRPCFrontendStream(stream client.ProxyService_ProxyServer, queueSize int) *grpcFrontendStream {
	if queueSize < 0 {
		queueSize = defaultFrontendWriteChannelSize
	}
	g := &grpcFrontendStream{
		ProxyService_ProxyServer: stream,
		writeCh:                  make(chan *client.Packet, queueSize),
		closed:                   make(chan struct{}),
		writerDone:               make(chan struct{}),
	}
	go g.serveFrontendWrites()
	return g
}

// Send queues pkt for the writer. It blocks while the queue is full.
func (g *grpcFrontendStream) Send(pkt *client.Packet) error {
	return enqueueFrontendWrite(g.writeCh, g.closed, &g.writeMetrics, pkt, "packetType", pkt.Type)
}

func (g *grpcFrontendStream) serveFrontendWrites() {
	defer close(g.writerDone)
	defer g.stop()

	for {
		select {
		case <-g.closed:
			return
		default:
		}

		select {
		case pkt := <-g.writeCh:
			g.writeMetrics.update(g.writeCh)
			if pkt == nil { // queued by drain
				return
			}
			if err := g.ProxyService_ProxyServer.Send(pkt); err != nil {
				if !g.isClosed() {
					metrics.Metrics.ObserveStreamError(commonmetrics.SegmentToClient, err, pkt.Type)
					klog.ErrorS(err, "Queued send to gRPC frontend failed", "packetType", pkt.Type)
				}
				return
			}
		case <-g.closed:
			return
		}
	}
}

func (g *grpcFrontendStream) isClosed() bool {
	select {
	case <-g.closed:
		return true
	default:
		return false
	}
}

// drain waits for the writer to send the packets already queued, then stops
// it. It gives up once the stream context ends, which also fails a send in
// progress.
func (g *grpcFrontendStream) drain() {
	defer g.stop()
	done := g.Context().Done()
	select {
	case g.writeCh <- nil:
		g.writeMetrics.update(g.writeCh)
	case <-g.closed:
		return
	case <-done:
		return
	}
	select {
	case <-g.writerDone:
	case <-done:
	}
}

// stop ends the writer and fails later Sends, including one waiting for queue
// space. Packets still queued are dropped.
func (g *grpcFrontendStream) stop() {
	g.stopOnce.Do(func() {
		g.writeMetrics.stop()
		close(g.closed)
	})
}
