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
	"k8s.io/klog/v2"

	commonmetrics "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

func (h *httpConnectStream) enqueueWrite(pkt *client.Packet) error {
	return enqueueFrontendWrite(h.writeCh, h.closed, nil, &h.writeMetrics, pkt, "host", h.host, "connectionID", h.connectID)
}

// serveFrontendWrites owns established socket writes. The HTTP handshake is
// completed before this goroutine starts, and CLOSE_RSP follows queued DATA.
// The socket is closed only on ordered close, an actual write error, or abort.
func (h *httpConnectStream) serveFrontendWrites() {
	defer close(h.writerDone)
	defer h.close()

	for {
		select {
		case <-h.closed:
			return
		default:
		}

		select {
		case pkt := <-h.writeCh:
			h.writeMetrics.update(h.writeCh)
			if pkt.Type == client.PacketType_CLOSE_RSP {
				return
			}
			if _, err := h.conn.Write(pkt.GetData().Data); err != nil {
				if !h.isClosed() {
					metrics.Metrics.ObserveStreamError(commonmetrics.SegmentToClient, err, pkt.Type)
					klog.ErrorS(err, "Queued send to HTTP-CONNECT frontend failed",
						"host", h.host, "dialID", h.dialID, "connectionID", h.connectID)
				}
				return
			}
		case <-h.closed:
			return
		}
	}
}
