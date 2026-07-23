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
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

const (
	maxHTTPConnectErrorBodyBytes = 4 << 10
	httpConnectSuccessResponse   = "HTTP/1.1 200 Connection Established\r\n\r\n"
	httpConnectTruncationMarker  = "\n[error response truncated]\n"
)

type httpConnectEnqueueResult uint8

const (
	httpConnectEnqueueAccepted httpConnectEnqueueResult = iota
	httpConnectEnqueueClosed
	httpConnectEnqueueOverflow
)

type httpConnectCloseDisposition uint8

const (
	httpConnectCloseDraining httpConnectCloseDisposition = iota
	httpConnectCloseAlreadyForced
)

type httpConnectAbortReason string

const (
	httpConnectAbortBackendShutdown httpConnectAbortReason = "backend_shutdown"
	httpConnectAbortDialClosed      httpConnectAbortReason = "dial_closed"
	httpConnectAbortFrontendClose   httpConnectAbortReason = "frontend_close"
	httpConnectAbortQueueOverflow   httpConnectAbortReason = "queue_overflow"
	httpConnectAbortSetupRace       httpConnectAbortReason = "setup_race"
	httpConnectAbortWriteFailure    httpConnectAbortReason = "write_failure"
)

// httpConnectWriter is the sole owner of normal writes to one HTTP-CONNECT
// frontend. Its private bounded FIFO keeps a blocked socket from parking the
// shared agent-stream consumer.
type httpConnectWriter struct {
	server   *ProxyServer
	frontend *ProxyClientConnection

	// initialResponse is optional. Real Tunnels supply the successful CONNECT
	// response; already-established lower-level connections leave it empty.
	initialResponse []byte
	notifyConnected bool

	dataCh  chan []byte
	abortCh chan struct{}

	// mu linearizes enqueue with graceful and forced terminal transitions. It
	// is never held during HTTP or backend I/O.
	mu                sync.Mutex
	accepting         bool
	closeResponseSeen bool
	aborted           bool

	startOnce    sync.Once
	gracefulOnce sync.Once
	abortOnce    sync.Once
}

func newHTTPConnectWriter(
	server *ProxyServer,
	frontend *ProxyClientConnection,
	initialResponse []byte,
	notifyConnected bool,
) *httpConnectWriter {
	queueDepth := max(1, server.httpConnectQueueSize)
	return &httpConnectWriter{
		server:          server,
		frontend:        frontend,
		initialResponse: initialResponse,
		notifyConnected: notifyConnected,
		dataCh:          make(chan []byte, queueDepth),
		abortCh:         make(chan struct{}),
		accepting:       true,
	}
}

func (w *httpConnectWriter) start() {
	w.startOnce.Do(func() {
		go w.run()
	})
}

func (w *httpConnectWriter) run() {
	if !w.writeInitialResponse() {
		return
	}

	for {
		select {
		case <-w.abortCh:
			return
		default:
		}

		select {
		case <-w.abortCh:
			return
		case data, ok := <-w.dataCh:
			if !ok {
				w.finishGracefulClose()
				return
			}

			// Abort may have won at the same time as the dequeue. Do not write a
			// queued payload after observing that terminal transition.
			if w.isAborted() {
				return
			}
			if err := w.write(data); err != nil {
				agentID, connectID, _ := w.frontend.httpConnectionDetails()
				klog.ErrorS(err, "HTTP-CONNECT DATA write failed", "agentID", agentID, "connectionID", connectID)
				w.abort(httpConnectAbortWriteFailure)
				return
			}
		}
	}
}

func (w *httpConnectWriter) writeInitialResponse() bool {
	w.mu.Lock()
	if w.aborted {
		w.initialResponse = nil
		w.mu.Unlock()
		return false
	}
	response := w.initialResponse
	w.mu.Unlock()

	if len(response) > 0 {
		err := w.write(response)
		w.mu.Lock()
		w.initialResponse = nil
		w.mu.Unlock()
		if err != nil {
			agentID, connectID, _ := w.frontend.httpConnectionDetails()
			klog.ErrorS(err, "HTTP-CONNECT initial response write failed", "agentID", agentID, "connectionID", connectID)
			w.abort(httpConnectAbortWriteFailure)
			return false
		}
	}

	// Successful establishment and a concurrent abort are linearized by the
	// same writer-state mutex. An abort that wins must wake Tunnel only through
	// the closed notification, never through connected.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.aborted {
		return false
	}
	if w.notifyConnected && w.frontend.connected != nil {
		close(w.frontend.connected)
		w.notifyConnected = false
	}
	return true
}

func (w *httpConnectWriter) write(data []byte) error {
	start := time.Now()
	defer func() {
		metrics.Metrics.ObserveFrontendWriteLatency(time.Since(start))
	}()
	if w.frontend.HTTP == nil {
		return fmt.Errorf("HTTP-CONNECT frontend has no writer")
	}
	return writeAll(w.frontend.HTTP, data)
}

// enqueueData transfers ownership of data to the writer when accepted. It
// never waits for queue space.
func (w *httpConnectWriter) enqueueData(data []byte) httpConnectEnqueueResult {
	w.mu.Lock()
	if !w.accepting {
		w.mu.Unlock()
		return httpConnectEnqueueClosed
	}

	select {
	case w.dataCh <- data:
		w.mu.Unlock()
		return httpConnectEnqueueAccepted
	default:
		// Reaching overflow requires accepting to still be true. Because
		// beginGracefulClose records closeResponseSeen and clears accepting in
		// this same critical section, a backend close response cannot already
		// have been observed here.
		firstAbort, _ := w.abortLocked()
		w.mu.Unlock()

		if firstAbort {
			w.frontend.markHTTPWriterTerminal(w)
			metrics.Metrics.ObserveHTTPConnectDisconnect(metrics.HTTPConnectDisconnectQueueOverflow)
			agentID, connectID, _ := w.frontend.httpConnectionDetails()
			klog.V(2).InfoS("Closing overloaded HTTP-CONNECT frontend", "reason", httpConnectAbortQueueOverflow, "agentID", agentID, "connectionID", connectID)
			w.frontend.startHTTPForcedCleanup(w.server, w, httpConnectAbortQueueOverflow)
		}
		return httpConnectEnqueueOverflow
	}
}

func (w *httpConnectWriter) beginGracefulClose() httpConnectCloseDisposition {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.closeResponseSeen = true
	if w.aborted {
		return httpConnectCloseAlreadyForced
	}
	w.accepting = false
	w.gracefulOnce.Do(func() {
		close(w.dataCh)
	})
	return httpConnectCloseDraining
}

func (w *httpConnectWriter) abort(reason httpConnectAbortReason) {
	w.mu.Lock()
	firstAbort, closeResponseSeen := w.abortLocked()
	w.mu.Unlock()
	if !firstAbort {
		return
	}

	w.frontend.markHTTPWriterTerminal(w)
	if closeResponseSeen {
		agentID, connectID, _ := w.frontend.httpConnectionDetails()
		w.server.removeEstablishedIf(agentID, connectID, w.frontend)
	}
	w.frontend.startHTTPForcedCleanup(w.server, w, reason)
}

// abortLocked performs only the non-blocking state transition. The caller
// must hold w.mu and start cleanup after releasing it.
func (w *httpConnectWriter) abortLocked() (firstAbort, closeResponseSeen bool) {
	if w.aborted {
		return false, w.closeResponseSeen
	}
	w.aborted = true
	w.accepting = false
	w.abortOnce.Do(func() {
		close(w.abortCh)
	})
	return true, w.closeResponseSeen
}

func (w *httpConnectWriter) isAborted() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.aborted
}

func (w *httpConnectWriter) finishGracefulClose() {
	w.mu.Lock()
	aborted := w.aborted
	w.mu.Unlock()
	if aborted {
		return
	}

	w.frontend.markHTTPWriterTerminal(w)
	agentID, connectID, _ := w.frontend.httpConnectionDetails()
	w.server.removeEstablishedIf(agentID, connectID, w.frontend)
	if err := w.frontend.closeHTTP(); err != nil {
		klog.ErrorS(err, "HTTP-CONNECT frontend close failed", "agentID", agentID, "connectionID", connectID)
	}
}

// discardQueuedData releases references to DATA that cannot be delivered
// after a forced abort. accepting is already false, so the queue cannot grow.
func (w *httpConnectWriter) discardQueuedData() {
	w.mu.Lock()
	w.initialResponse = nil
	w.mu.Unlock()

	for {
		select {
		case _, ok := <-w.dataCh:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func (c *ProxyClientConnection) attachHTTPWriter(
	server *ProxyServer,
	initialResponse []byte,
	notifyConnected bool,
) (*httpConnectWriter, bool) {
	c.httpMu.Lock()
	defer c.httpMu.Unlock()
	if c.httpWriter != nil {
		return c.httpWriter, true
	}
	if c.httpTerminal {
		return nil, false
	}
	w := newHTTPConnectWriter(server, c, initialResponse, notifyConnected)
	if len(initialResponse) > 0 {
		c.httpInitialResponse = nil
	}
	c.httpWriter = w
	return w, true
}

func (c *ProxyClientConnection) configuredHTTPWriter(server *ProxyServer, notifyConnected bool) (*httpConnectWriter, bool) {
	c.httpMu.Lock()
	defer c.httpMu.Unlock()
	if c.httpWriter != nil {
		return c.httpWriter, true
	}
	if c.httpTerminal {
		return nil, false
	}
	w := newHTTPConnectWriter(server, c, c.httpInitialResponse, notifyConnected)
	c.httpInitialResponse = nil
	c.httpWriter = w
	return w, true
}

func (c *ProxyClientConnection) markHTTPWriterTerminal(expected *httpConnectWriter) {
	c.httpMu.Lock()
	defer c.httpMu.Unlock()
	if expected == nil || c.httpWriter == expected {
		c.httpTerminal = true
	}
}

func (c *ProxyClientConnection) httpWriterIsTerminal(expected *httpConnectWriter) bool {
	c.httpMu.Lock()
	defer c.httpMu.Unlock()
	return c.httpTerminal || c.httpWriter != expected
}

func (c *ProxyClientConnection) abortHTTP(server *ProxyServer, reason httpConnectAbortReason) {
	c.httpMu.Lock()
	c.httpTerminal = true
	w := c.httpWriter
	c.httpMu.Unlock()

	if w != nil {
		w.abort(reason)
		return
	}
	c.startHTTPForcedCleanup(server, nil, reason)
}

func (c *ProxyClientConnection) startHTTPForcedCleanup(
	server *ProxyServer,
	w *httpConnectWriter,
	reason httpConnectAbortReason,
) {
	c.httpCleanupOnce.Do(func() {
		go func() {
			if w != nil {
				w.discardQueuedData()
			}
			if err := c.closeHTTP(); err != nil {
				agentID, connectID, _ := c.httpConnectionDetails()
				klog.ErrorS(err, "HTTP-CONNECT frontend close failed", "reason", reason, "agentID", agentID, "connectionID", connectID)
			}
			c.sendBackendCloseRequest(server, string(reason))
		}()
	})
}

func (c *ProxyClientConnection) closeHTTP() error {
	c.closeHTTPOnce.Do(func() {
		if c.closed != nil {
			close(c.closed)
		}
		if c.CloseHTTP != nil {
			c.closeHTTPErr = c.CloseHTTP()
		}
	})
	return c.closeHTTPErr
}

func (c *ProxyClientConnection) setHTTPConnectionDetails(agentID string, connectID int64) {
	c.httpMu.Lock()
	c.agentID = agentID
	c.connectID = connectID
	c.httpMu.Unlock()
}

func (c *ProxyClientConnection) httpConnectionDetails() (agentID string, connectID int64, backend *Backend) {
	c.httpMu.Lock()
	defer c.httpMu.Unlock()
	return c.agentID, c.connectID, c.backend
}

func (c *ProxyClientConnection) suppressBackendCloseRequest() {
	c.httpMu.Lock()
	c.httpSuppressCloseRequest = true
	c.httpMu.Unlock()
}

func (c *ProxyClientConnection) sendBackendCloseRequestAsync(server *ProxyServer, reason string) {
	go c.sendBackendCloseRequest(server, reason)
}

func (c *ProxyClientConnection) sendBackendCloseRequest(server *ProxyServer, reason string) {
	_, connectID, backend := c.httpConnectionDetails()
	if connectID == 0 || backend == nil || backend.conn == nil {
		// In particular, a pre-establishment terminal path must not consume the
		// once guard. A concurrent successful DIAL_RSP may assign a real ID and
		// still needs to close that backend connection.
		return
	}

	c.closeRequestOnce.Do(func() {
		c.httpMu.Lock()
		suppressed := c.httpSuppressCloseRequest
		connectID = c.connectID
		backend = c.backend
		dialID := c.dialID
		c.httpMu.Unlock()
		if suppressed || connectID == 0 || backend == nil || backend.conn == nil {
			return
		}
		if backend.Context().Err() != nil {
			return
		}
		server.sendBackendClose(backend, connectID, dialID, reason)
	})
}

func serializeHTTPConnectDialError(dialErr string) []byte {
	body := []byte(dialErr)
	if len(body) > maxHTTPConnectErrorBodyBytes {
		marker := []byte(httpConnectTruncationMarker)
		prefixLen := maxHTTPConnectErrorBodyBytes - len(marker)
		body = append(append([]byte(nil), body[:prefixLen]...), marker...)
	}

	statusCode := mapDialErrorToHTTPStatus(dialErr)
	response := http.Response{
		StatusCode:    statusCode,
		Status:        fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Header: http.Header{
			"Content-Type": []string{"text/plain; charset=utf-8"},
		},
		Proto:      "HTTP/1.1",
		ProtoMinor: 1,
		ProtoMajor: 1,
	}
	var serialized bytes.Buffer
	// bytes.Buffer writes cannot fail.
	_ = response.Write(&serialized)
	return serialized.Bytes()
}

// writeAll preserves stream semantics for writers that make partial progress.
func writeAll(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if n < 0 || n > len(data) {
			return io.ErrShortWrite
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
