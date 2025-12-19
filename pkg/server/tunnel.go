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

const (
	// bufferSize is the size of the buffer used for reading from the hijacked connection.
	// It matches the gRPC window size for optimal performance.
	bufferSize = 1 << 15 // 32KB
)

// bufferPool is a pool of byte slices used for reading data from hijacked connections.
// This reduces memory allocations and GC pressure by reusing buffers across connections.
var bufferPool = sync.Pool{
	New: func() interface{} {
		// Allocate a new buffer when the pool is empty
		buf := make([]byte, bufferSize)
		return &buf
	},
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
		// The hijacked connection will be closed by the closeOnce defer.
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

	// This defer acts as a safeguard to ensure we clean up the pending dial
	// if the connection is never successfully established.
	established := false
	defer func() {
		if !established {
			if t.Server.PendingDial.Remove(random) != nil {
				// This metric is observed only when the frontend closes the connection.
				// Other failure reasons are observed elsewhere.
				metrics.Metrics.ObserveDialFailure(metrics.DialFailureFrontendClose)
			}
		}
	}()

	if err := backend.Send(dialRequest); err != nil {
		klog.ErrorS(err, "failed to tunnel dial request", "host", r.Host, "dialID", connection.dialID, "agentID", connection.agentID)
		metrics.Metrics.ObserveDialFailure(metrics.DialFailureBackendClose)
		// Send proper HTTP error response
		conn.Write([]byte(fmt.Sprintf("HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nFailed to tunnel dial request: %v\r\n", err)))
		// The deferred cleanup will run when we return here.
		return
	}

	ctxt := backend.Context()

	select {
	case <-connection.connected: // Waiting for response before we begin full communication.
		// The connection is successful. Mark it as established so the deferred
		// cleanup function knows not to remove it from PendingDial.
		established = true

		// Now that connection is established, send 200 OK to switch to tunnel mode
		_, err = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		if err != nil {
			klog.ErrorS(err, "failed to send 200 connection established", "host", r.Host, "agentID", connection.agentID)
			// We return here, but since `established` is true, the deferred
			// function will not remove the pending dial. The agent-side goroutine
			// is responsible for the established connection now.
			return
		}
		klog.V(3).InfoS("Connection established, sent 200 OK", "host", r.Host, "agentID", connection.agentID, "connectionID", connection.connectID)

	case <-closed: // Connection was closed by the client before being established
		klog.V(2).InfoS("Frontend connection closed before being established", "host", r.Host, "dialID", connection.dialID, "agentID", connection.agentID)
		// The deferred cleanup will run when we return here.
		return

	case <-ctxt.Done(): // Backend connection died before being established
		klog.ErrorS(ctxt.Err(), "backend context closed before connection was established", "host", r.Host, "dialID", connection.dialID, "agentID", connection.agentID)
		metrics.Metrics.ObserveDialFailure(metrics.DialFailureBackendClose)
		// Send proper HTTP error response
		conn.Write([]byte(fmt.Sprintf("HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nBackend context error: %v\r\n", ctxt.Err())))
		// The deferred cleanup will run when we return here.
		return
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
		// The top-level defer handles conn.Close()
	}()

	connID := connection.connectID
	agentID := connection.agentID
	klog.V(3).InfoS("Starting proxy to host", "host", r.Host, "agentID", agentID, "connectionID", connID)

	// Get a buffer from the pool
	bufPtr := bufferPool.Get().(*[]byte)
	pkt := *bufPtr
	defer func() {
		// Return the buffer to the pool when done
		bufferPool.Put(bufPtr)
	}()

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
