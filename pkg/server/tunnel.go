/*
Copyright 2019 The Kubernetes Authors.

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
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
	commonmetrics "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

type httpFrontend struct {
	conn      net.Conn
	closeOnce sync.Once
	closed    chan struct{}
}

func (h *httpFrontend) Send(pkt *client.Packet) error {
	const segment = commonmetrics.SegmentToClient
	metrics.Metrics.ObservePacket(segment, pkt.Type)

	if pkt.Type == client.PacketType_CLOSE_RSP {
		h.close()
	} else if pkt.Type == client.PacketType_DIAL_CLS {
		h.close()
	} else if pkt.Type == client.PacketType_DATA {
		_, err := h.conn.Write(pkt.GetData().Data)
		return err
	} else if pkt.Type == client.PacketType_DIAL_RSP {
		if pkt.GetDialResponse().Error != "" {
			body := bytes.NewBufferString(pkt.GetDialResponse().Error)
			t := http.Response{
				StatusCode: 503,
				Body:       io.NopCloser(body),
			}

			t.Write(h.conn)
			h.close()
		}
	} else {
		return fmt.Errorf("attempt to send via unrecognized connection type %v", pkt.Type)
	}
	return nil
}

func (h *httpFrontend) Recv() (*client.Packet, error) {
	return nil, fmt.Errorf("Recv method not implemented on httpFrontend")
}

func (h *httpFrontend) close() {
	h.closeOnce.Do(func() {
		h.conn.Close()
		close(h.closed)
	})
}

// Tunnel implements Proxy based on HTTP Connect, which tunnels the traffic to
// the agent registered in ProxyServer.
type Tunnel struct {
	Server *ProxyServer
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metrics.Metrics.HTTPConnectionInc()
	defer metrics.Metrics.HTTPConnectionDec()

	klog.V(2).InfoS("Received request for host", "method", r.Method, "host", r.Host, "userAgent", r.UserAgent())
	if r.TLS != nil {
		klog.V(2).InfoS("TLS", "commonName", r.TLS.PeerCertificates[0].Subject.CommonName)
	}
	if r.Method != http.MethodConnect {
		http.Error(w, "this proxy only supports CONNECT passthrough", http.StatusMethodNotAllowed)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	frontend := &httpFrontend{
		conn:   conn,
		closed: make(chan struct{}),
	}
	defer frontend.close()

	random := rand.Int63() /* #nosec G404 */
	dialRequest := &client.Packet{
		Type: client.PacketType_DIAL_REQ,
		Payload: &client.Packet_DialRequest{
			DialRequest: &client.DialRequest{
				Protocol: "tcp",
				Address:  r.Host,
				Random:   random,
			},
		},
	}

	klog.V(4).Infof("Set pending(rand=%d) to %v", random, w)
	backend, err := t.Server.getBackend(r.Host)
	if err != nil {
		http.Error(w, fmt.Sprintf("currently no tunnels available: %v", err), http.StatusInternalServerError)
		return
	}
	connected := make(chan struct{})
	connection := &ProxyClientConnection{
		connected: connected,
		start:     time.Now(),
		backend:   backend,
		frontend:  frontend,
	}
	t.Server.PendingDial.Add(random, connection)
	if err := backend.Send(dialRequest); err != nil {
		klog.ErrorS(err, "failed to tunnel dial request")
		return
	}
	ctxt := backend.Context()
	if ctxt.Err() != nil {
		klog.ErrorS(err, "context reports failure")
	}

	select {
	case <-ctxt.Done():
		klog.V(5).Infoln("context reports done")
	default:
	}

	select {
	case <-connection.connected: // Waiting for response before we begin full communication.
	case <-frontend.closed: // Connection was closed before being established
	}

	defer func() {
		packet := &client.Packet{
			Type: client.PacketType_CLOSE_REQ,
			Payload: &client.Packet_CloseRequest{
				CloseRequest: &client.CloseRequest{
					ConnectID: connection.connectID,
				},
			},
		}

		if err = backend.Send(packet); err != nil {
			klog.V(2).InfoS("failed to send close request packet", "host", r.Host, "agentID", connection.agentID, "connectionID", connection.connectID)
		}
	}()

	klog.V(3).InfoS("Starting proxy to host", "host", r.Host)
	pkt := make([]byte, 1<<15) // Match GRPC Window size

	connID := connection.connectID
	agentID := connection.agentID
	var acc int

	for {
		n, err := bufrw.Read(pkt[:])
		acc += n
		if err == io.EOF {
			klog.V(1).InfoS("EOF from host", "host", r.Host)
			break
		}
		if err != nil {
			klog.ErrorS(err, "Received failure on connection")
			break
		}

		packet := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: connID,
					Data:      pkt[:n],
				},
			},
		}
		err = backend.Send(packet)
		if err != nil {
			klog.ErrorS(err, "error sending packet")
			break
		}
		klog.V(5).InfoS("Forwarding data on tunnel to agent",
			"bytes", n,
			"totalBytes", acc,
			"agentID", connection.agentID,
			"connectionID", connection.connectID)
	}

	klog.V(5).InfoS("Stopping transfer to host", "host", r.Host, "agentID", agentID, "connectionID", connID)
}
