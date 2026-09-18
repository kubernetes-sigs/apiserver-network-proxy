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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc/metadata"
	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

const (
	// bufferSize is the size of the buffer used for reading from the hijacked connection.
	// It matches the gRPC window size for optimal performance.
	bufferSize = 1 << 15 // 32KB

	connectEstablished = "HTTP/1.1 200 Connection Established\r\n\r\n"
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

	stream := newHTTPConnectStream(r, conn, bufrw, t.Server.frontendWriteChannelSize)
	// Proxy has returned, so no goroutine is reading the hijacked connection
	// any more and the read buffer can be recycled.
	defer stream.release()

	// Hand the request to the same packet handling that serves gRPC frontends.
	if err := t.Server.proxy(stream); err != nil {
		klog.V(2).InfoS("HTTP-CONNECT frontend closed with error", "host", r.Host, "dialID", stream.dialID, "error", err)
	}
}

// httpConnectStream adapts a hijacked HTTP CONNECT connection to the frontend
// ProxyStream interface.
//
// The CONNECT request itself is presented as the DIAL_REQ packet, bytes read
// from the client become DATA packets, and packets sent back to the frontend
// are turned into the CONNECT response or into raw tunnel bytes. This lets
// ProxyServer.Proxy handle an HTTP-CONNECT frontend with exactly the packet
// handling it uses for a gRPC frontend.
type httpConnectStream struct {
	conn  net.Conn
	bufrw *bufio.ReadWriter
	// ctx has the lifetime of the hijacked connection: cancel is called when
	// the stream is closed, so that Context() reports the end of the stream the
	// way a gRPC stream context does.
	ctx    context.Context
	cancel context.CancelFunc

	host   string
	dialID int64

	// dialRequested records that the synthetic DIAL_REQ has been handed to the
	// caller. Every later Recv reads tunnel bytes.
	dialRequested bool

	// connected is closed by Send once a successful DIAL_RSP has assigned
	// connectID. Recv must not build a DATA packet before that, because tunnel
	// bytes carry no connection ID of their own.
	connected chan struct{}
	connectID int64

	// closed is closed once the hijacked connection has been closed. It
	// unblocks a Recv that is waiting for a dial which will never complete.
	closed    chan struct{}
	closeOnce sync.Once

	// The writer starts only after the HTTP 200 response has been written.
	// Send queues established DATA and CLOSE_RSP in wire order. Neither channel
	// is replaced, and writeCh is never closed, so Close can race with Send.
	writeCh      chan *client.Packet
	writerDone   chan struct{}
	writeMetrics frontendWriteQueueMetrics

	// readBuf is scratch space for reading the hijacked connection. It is only
	// touched by Recv, which Frontend serializes.
	readBuf *[]byte
}

var _ ProxyStream = &httpConnectStream{}

var _ io.Closer = &httpConnectStream{}

func newHTTPConnectStream(r *http.Request, conn net.Conn, bufrw *bufio.ReadWriter, queueSize int) *httpConnectStream {
	// The frontend packet handling expects gRPC style incoming metadata. Give
	// it the client information the CONNECT request carried. The stream
	// lifetime is bound to the hijacked connection rather than to the request,
	// so it does not inherit the request context.
	ctx, cancel := context.WithCancel(context.Background()) // #nosec G118 -- close owns and calls cancel.
	ctx = metadata.NewIncomingContext(ctx,
		metadata.Pairs(header.UserAgent, r.UserAgent()))
	if queueSize <= 0 {
		queueSize = defaultFrontendWriteChannelSize
	}

	return &httpConnectStream{
		conn:       conn,
		bufrw:      bufrw,
		ctx:        ctx,
		cancel:     cancel,
		host:       r.Host,
		dialID:     rand.Int63(), /* #nosec G404 */
		connected:  make(chan struct{}),
		closed:     make(chan struct{}),
		writeCh:    make(chan *client.Packet, queueSize),
		writerDone: make(chan struct{}),
		readBuf:    bufferPool.Get().(*[]byte),
	}
}

func (h *httpConnectStream) Context() context.Context {
	return h.ctx
}

// Recv returns the CONNECT request as a DIAL_REQ, then the bytes the client
// writes into the tunnel as DATA packets.
func (h *httpConnectStream) Recv() (*client.Packet, error) {
	if !h.dialRequested {
		h.dialRequested = true
		return &client.Packet{
			Type: client.PacketType_DIAL_REQ,
			Payload: &client.Packet_DialRequest{
				DialRequest: &client.DialRequest{
					Protocol: "tcp",
					Address:  h.host,
					Random:   h.dialID,
				},
			},
		}, nil
	}

	select {
	case <-h.connected:
	case <-h.closed:
		return nil, io.EOF
	}

	buf := *h.readBuf
	for {
		n, err := h.bufrw.Read(buf)
		if n > 0 {
			// The packet is queued for the backend and outlives this call, so
			// it cannot reference the reusable read buffer. A read error that
			// accompanied the data is reported by the next Recv; bufio retains
			// it.
			data := make([]byte, n)
			copy(data, buf[:n])
			return &client.Packet{
				Type: client.PacketType_DATA,
				Payload: &client.Packet_Data{
					Data: &client.Data{
						ConnectID: h.connectID,
						Data:      data,
					},
				},
			}, nil
		}
		if err == nil {
			continue
		}
		if err == io.EOF || h.isClosed() {
			// A read that fails because we closed the connection ourselves is
			// an ordinary end of stream, not a stream failure.
			return nil, io.EOF
		}
		return nil, err
	}
}

// Send turns a frontend packet into the CONNECT response, tunnel bytes, or a
// close of the hijacked connection.
func (h *httpConnectStream) Send(pkt *client.Packet) error {
	switch pkt.Type {
	case client.PacketType_DIAL_RSP:
		return h.sendDialResponse(pkt.GetDialResponse())
	case client.PacketType_DATA:
		return h.enqueueWrite(pkt)
	case client.PacketType_CLOSE_RSP:
		select {
		case <-h.connected:
			return h.enqueueWrite(pkt)
		default:
			h.close()
			return nil
		}
	case client.PacketType_DIAL_CLS:
		h.close()
		return nil
	default:
		return fmt.Errorf("attempt to send unsupported packet type %v to an HTTP-CONNECT frontend", pkt.Type)
	}
}

func (h *httpConnectStream) sendDialResponse(resp *client.DialResponse) error {
	if resp.Error != "" {
		h.writeDialError(resp.Error)
		h.close()
		return nil
	}

	// Ordered before the response so that a Recv released by close(connected)
	// always observes the connection ID.
	h.connectID = resp.ConnectID
	if _, err := h.conn.Write([]byte(connectEstablished)); err != nil {
		klog.ErrorS(err, "failed to send 200 connection established", "host", h.host,
			"dialID", h.dialID, "connectionID", resp.ConnectID)
		h.close()
		return err
	}
	klog.V(3).InfoS("Connection established, sent 200 OK", "host", h.host,
		"dialID", h.dialID, "connectionID", resp.ConnectID)
	go h.serveFrontendWrites()
	close(h.connected)
	return nil
}

// writeDialError reports a failed dial to the client as an HTTP error response,
// since the tunnel was never established.
func (h *httpConnectStream) writeDialError(dialErr string) {
	statusCode := mapDialErrorToHTTPStatus(dialErr)
	body := bytes.NewBufferString(dialErr)
	resp := http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Body:       io.NopCloser(body),
		Header: http.Header{
			"Content-Type": []string{"text/plain; charset=utf-8"},
		},
		ContentLength: int64(body.Len()),
		Proto:         "HTTP/1.1",
		ProtoMinor:    1,
		ProtoMajor:    1,
	}
	if err := resp.Write(h.conn); err != nil {
		klog.V(2).ErrorS(err, "failed to write dial error to HTTP-CONNECT frontend",
			"host", h.host, "dialID", h.dialID, "dialError", dialErr)
	}
}

func (h *httpConnectStream) isClosed() bool {
	select {
	case <-h.closed:
		return true
	default:
		return false
	}
}

// close closes the hijacked connection, which also unblocks a Recv that is
// either waiting for the dial to complete or reading tunnel bytes.
// It also interrupts a blocked socket write and any Send waiting for queue space.
func (h *httpConnectStream) close() {
	h.closeOnce.Do(func() {
		h.stopFrontendWriteQueueMetric()
		close(h.closed)
		if err := h.conn.Close(); err != nil {
			klog.V(4).ErrorS(err, "failed to close hijacked connection", "host", h.host, "dialID", h.dialID)
		}
		h.cancel()
	})
}

// Close aborts the stream; unlike a queued CLOSE_RSP, it does not drain writes.
func (h *httpConnectStream) Close() error {
	h.close()
	return nil
}

// release closes the stream and recycles its read buffer. It must only be
// called once no one is reading the stream any more.
func (h *httpConnectStream) release() {
	h.close()
	bufferPool.Put(h.readBuf)
}

// mapDialErrorToHTTPStatus maps common TCP/network error strings to appropriate HTTP status codes
func mapDialErrorToHTTPStatus(errStr string) int {
	// Convert to lowercase for case-insensitive matching
	errLower := strings.ToLower(errStr)

	// Check each error pattern and return appropriate status code
	switch {
	// Timeouts - backend didn't respond in time -> 504 Gateway Timeout
	case strings.Contains(errLower, "i/o timeout"),
		strings.Contains(errLower, "deadline exceeded"),
		strings.Contains(errLower, "context deadline exceeded"),
		strings.Contains(errLower, "timeout"),
		strings.Contains(errLower, "timed out"):
		return 504

	// No tunnel to serve the request, and resource exhaustion. Both are
	// retryable proxy side conditions -> 503 Service Unavailable
	case strings.Contains(errLower, "no agent available"),
		strings.Contains(errLower, "too many open files"),
		strings.Contains(errLower, "socket: too many open files"):
		return 503

	// Connection errors -> 502 Bad Gateway
	case strings.Contains(errLower, "connection refused"),
		strings.Contains(errLower, "connection reset by peer"),
		strings.Contains(errLower, "broken pipe"),
		strings.Contains(errLower, "network is unreachable"),
		strings.Contains(errLower, "no route to host"),
		strings.Contains(errLower, "host is unreachable"),
		strings.Contains(errLower, "network is down"):
		return 502

	// DNS resolution failures -> 502 Bad Gateway
	case strings.Contains(errLower, "no such host"),
		strings.Contains(errLower, "name resolution"),
		strings.Contains(errLower, "lookup") && strings.Contains(errLower, "no such host"):
		return 502

	// TLS/SSL errors -> 502 Bad Gateway
	case strings.Contains(errLower, "tls"),
		strings.Contains(errLower, "ssl"),
		strings.Contains(errLower, "certificate"):
		return 502

	// Default to 502 Bad Gateway for unknown proxy errors
	default:
		return 502
	}
}
