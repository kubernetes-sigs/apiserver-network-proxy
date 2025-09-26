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
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

// Tunnel implements Proxy based on HTTP Connect, which tunnels the traffic to
// the agent registered in ProxyServer.
type Tunnel struct {
	Server *ProxyServer
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metrics.Metrics.HTTPConnectionInc()
	defer metrics.Metrics.HTTPConnectionDec()

	klog.V(2).InfoS("Received request for host", "method", r.Method, "host", r.Host, "userAgent", r.UserAgent())
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
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

	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var closeOnce sync.Once
	defer closeOnce.Do(func() { conn.Close() })

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
		klog.ErrorS(err, "no tunnels available")
		conn.Write([]byte(fmt.Sprintf("HTTP/1.1 500 Internal Server Error\r\nContent-Type: text/plain\r\n\r\ncurrently no tunnels available: %v", err)))
		conn.Close()
		return
	}
	closed := make(chan struct{})
	connected := make(chan struct{})
	connection := &ProxyClientConnection{
		Mode: ModeHTTPConnect,
		HTTP: io.ReadWriter(conn), // pass as ReadWriter so the caller must close with CloseHTTP
		CloseHTTP: func() error {
			closeOnce.Do(func() { conn.Close() })
			close(closed)
			return nil
		},
		connected: connected,
		start:     time.Now(),
		backend:   backend,
		dialID:    random,
		agentID:   backend.GetAgentID(),
	}
	t.Server.PendingDial.Add(random, connection)
	if err := backend.Send(dialRequest); err != nil {
		klog.ErrorS(err, "failed to tunnel dial request", "host", r.Host, "dialID", connection.dialID, "agentID", connection.agentID)
		// Send proper HTTP error response
		conn.Write([]byte(fmt.Sprintf("HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nFailed to tunnel dial request: %v\r\n", err)))
		conn.Close()
		return
	}
	ctxt := backend.Context()
	if ctxt.Err() != nil {
		klog.ErrorS(ctxt.Err(), "context reports failure")
		conn.Write([]byte(fmt.Sprintf("HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nBackend context error: %v\r\n", ctxt.Err())))
		conn.Close()
		return
	}

	select {
	case <-ctxt.Done():
		klog.V(5).Infoln("context reports done")
	default:
	}

	select {
	case <-connection.connected: // Waiting for response before we begin full communication.
		// Now that connection is established, send 200 OK to switch to tunnel mode
		_, err = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		if err != nil {
			klog.ErrorS(err, "failed to send 200 connection established", "host", r.Host, "agentID", connection.agentID)
			conn.Close()
			return
		}
		klog.V(3).InfoS("Connection established, sent 200 OK", "host", r.Host, "agentID", connection.agentID, "connectionID", connection.connectID)

	case <-closed: // Connection was closed before being established
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
		conn.Close()
	}()

	connID := connection.connectID
	agentID := connection.agentID
	klog.V(3).InfoS("Starting proxy to host", "host", r.Host, "agentID", agentID, "connectionID", connID)

	pkt := make([]byte, 1<<15) // Match GRPC Window size
	var acc int

	for {
		n, err := bufrw.Read(pkt[:])
		acc += n
		if err == io.EOF {
			klog.V(1).InfoS("EOF from host", "host", r.Host, "agentID", agentID, "connectionID", connID)
			break
		}
		if err != nil {
			klog.ErrorS(err, "Received failure on connection", "host", r.Host, "agentID", agentID, "connectionID", connID)
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
			klog.ErrorS(err, "error sending packet", "host", r.Host, "agentID", agentID, "connectionID", connID)
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
