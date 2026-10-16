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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
)

// maxConnDataPacketSize bounds the payload of each DATA packet a serverConn
// sends, matching the read size used for HTTP-CONNECT tunnels.
const maxConnDataPacketSize = bufferSize

var errDialClosed = errors.New("dial closed")

type proxyAddr struct {
	network string
	address string
}

func (a *proxyAddr) Network() string { return a.network }
func (a *proxyAddr) String() string  { return a.address }

// serverConn is an implementation of net.Conn that proxies data over an
// established backend agent tunnel. It is also the ProxyStream of the
// Frontend registered for the connection, so packets from the agent are
// delivered to it through Send.
type serverConn struct {
	server      *ProxyServer
	backend     *Backend
	dialID      int64
	agentID     string
	dialAddress string

	// ctx is cancelled by Close. It aborts writes that are waiting for the
	// backend send lock and stops the closeWithBackend watcher.
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	cond *sync.Cond
	// connID is set once a successful DIAL_RSP has been accepted.
	connID int64
	// buffer holds DATA payloads received from the agent and not yet read.
	// It holds at most maxBuffered payloads; once full, Send blocks until
	// Read drains it or the connection is closed.
	buffer        [][]byte
	maxBuffered   int
	rdata         []byte
	closed        bool
	closeErr      error
	readDeadline  time.Time
	writeDeadline time.Time
	readTimer     *time.Timer

	// connected is closed once a successful DIAL_RSP has been accepted.
	connected chan struct{}
	// dialErrCh receives the error that failed the dial.
	dialErrCh chan error
	closeOnce sync.Once
}

var _ net.Conn = &serverConn{}
var _ ProxyStream = &serverConn{}
var _ io.Closer = &serverConn{}

func newServerConn(server *ProxyServer, backend *Backend, dialID int64, dialAddress string) *serverConn {
	ctx, cancel := context.WithCancel(context.Background()) // #nosec G118 -- Close owns and calls cancel.
	sc := &serverConn{
		server:      server,
		backend:     backend,
		dialID:      dialID,
		agentID:     backend.GetAgentID(),
		dialAddress: dialAddress,
		ctx:         ctx,
		cancel:      cancel,
		maxBuffered: max(server.frontendWriteChannelSize, 1),
		connected:   make(chan struct{}),
		dialErrCh:   make(chan error, 1),
	}
	sc.cond = sync.NewCond(&sc.mu)
	return sc
}

// Send receives a packet from the agent for this connection.
func (sc *serverConn) Send(pkt *client.Packet) error {
	switch pkt.Type {
	case client.PacketType_CLOSE_RSP:
		sc.handleClose(pkt.GetCloseResponse().Error)
		return nil
	case client.PacketType_DIAL_CLS:
		sc.handleDialError(errDialClosed)
		return nil
	case client.PacketType_DATA:
		return sc.handleData(pkt.GetData().Data)
	case client.PacketType_DIAL_RSP:
		resp := pkt.GetDialResponse()
		if resp.Error != "" {
			sc.handleDialError(errors.New(resp.Error))
			return nil
		}
		return sc.handleDialSuccess(resp.ConnectID)
	}
	return fmt.Errorf("attempt to send via unrecognized packet type %v", pkt.Type)
}

func (sc *serverConn) Recv() (*client.Packet, error) {
	<-sc.ctx.Done()
	return nil, io.EOF
}

func (sc *serverConn) Context() context.Context {
	return sc.ctx
}

// handleDialSuccess accepts the connection ID from a successful DIAL_RSP.
// If the dialer already gave up and closed the connection, it returns an
// error so that the DIAL_RSP handler removes the established connection and
// closes it on the agent.
func (sc *serverConn) handleDialSuccess(connID int64) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return net.ErrClosed
	}
	sc.connID = connID
	select {
	case <-sc.connected:
	default:
		close(sc.connected)
	}
	return nil
}

func (sc *serverConn) handleData(data []byte) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for !sc.closed && len(sc.buffer) >= sc.maxBuffered {
		sc.cond.Wait()
	}
	if sc.closed {
		return net.ErrClosed
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	sc.buffer = append(sc.buffer, cp)
	sc.cond.Broadcast()
	return nil
}

func (sc *serverConn) handleClose(errMsg string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.closed {
		sc.closed = true
		if errMsg != "" {
			sc.closeErr = errors.New(errMsg)
		} else {
			sc.closeErr = io.EOF
		}
		sc.cond.Broadcast()
	}
}

func (sc *serverConn) handleDialError(err error) {
	select {
	case sc.dialErrCh <- err:
	default:
	}
}

func (sc *serverConn) Read(b []byte) (n int, err error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	for {
		if len(sc.rdata) > 0 {
			n = copy(b, sc.rdata)
			sc.rdata = sc.rdata[n:]
			if len(sc.rdata) == 0 {
				sc.rdata = nil
			}
			return n, nil
		}

		if len(sc.buffer) > 0 {
			sc.rdata = sc.buffer[0]
			sc.buffer[0] = nil
			sc.buffer = sc.buffer[1:]
			// Wake a Send waiting for buffer space.
			sc.cond.Broadcast()
			continue
		}

		if sc.closed {
			if sc.closeErr != nil {
				return 0, sc.closeErr
			}
			return 0, io.EOF
		}

		if !sc.readDeadline.IsZero() && !time.Now().Before(sc.readDeadline) {
			return 0, os.ErrDeadlineExceeded
		}

		sc.cond.Wait()
	}
}

func (sc *serverConn) Write(b []byte) (n int, err error) {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return 0, net.ErrClosed
	}
	connID := sc.connID
	deadline := sc.writeDeadline
	sc.mu.Unlock()

	ctx := sc.ctx
	if !deadline.IsZero() {
		if !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	for n < len(b) {
		chunk := b[n:min(len(b), n+maxConnDataPacketSize)]
		packet := &client.Packet{
			Type: client.PacketType_DATA,
			Payload: &client.Packet_Data{
				Data: &client.Data{
					ConnectID: connID,
					Data:      chunk,
				},
			},
		}
		if err := sc.backend.SendContext(ctx, packet); err != nil {
			switch {
			case sc.ctx.Err() != nil:
				err = net.ErrClosed
			case errors.Is(err, context.DeadlineExceeded):
				err = os.ErrDeadlineExceeded
			}
			return n, err
		}
		n += len(chunk)
	}
	return n, nil
}

// Close closes the connection locally and tells the agent to close it. It
// does not wait for the agent to be told: a Write may be blocked sending to
// the agent, and the close message would be queued behind it.
func (sc *serverConn) Close() error {
	sc.closeOnce.Do(func() {
		sc.mu.Lock()
		sc.closed = true
		if sc.closeErr == nil {
			sc.closeErr = net.ErrClosed
		}
		if sc.readTimer != nil {
			sc.readTimer.Stop()
			sc.readTimer = nil
		}
		sc.cond.Broadcast()
		connID := sc.connID
		sc.mu.Unlock()
		sc.cancel()

		if connID == 0 {
			// The dial did not complete, and DialContext has told the agent
			// to stop dialing if needed. If a successful DIAL_RSP is being
			// handled concurrently, handleDialSuccess will now reject it and
			// the DIAL_RSP handler closes the connection on the agent.
			sc.server.PendingDial.Remove(sc.dialID)
			return
		}
		if sc.server.removeEstablished(sc.agentID, connID) == nil {
			// The agent or the agent connection already closed it.
			return
		}
		metrics.Metrics.ObserveEstablishedConnectionsClosed(metrics.ConnectionClosedFrontendClose, 1)
		packet := &client.Packet{
			Type: client.PacketType_CLOSE_REQ,
			Payload: &client.Packet_CloseRequest{
				CloseRequest: &client.CloseRequest{
					ConnectID: connID,
				},
			},
		}
		go func() {
			if err := sc.backend.Send(packet); err != nil {
				klog.V(5).ErrorS(err, "Failed to send close to agent",
					"dialID", sc.dialID, "agentID", sc.agentID, "connectionID", connID)
			}
		}()
	})
	return nil
}

func (sc *serverConn) LocalAddr() net.Addr {
	return &proxyAddr{network: "tcp", address: "konnectivity-server"}
}

func (sc *serverConn) RemoteAddr() net.Addr {
	return &proxyAddr{network: "tcp", address: sc.dialAddress}
}

func (sc *serverConn) SetDeadline(t time.Time) error {
	_ = sc.SetReadDeadline(t)
	return sc.SetWriteDeadline(t)
}

func (sc *serverConn) SetReadDeadline(t time.Time) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.readDeadline = t
	if sc.readTimer != nil {
		sc.readTimer.Stop()
		sc.readTimer = nil
	}
	if !t.IsZero() {
		d := time.Until(t)
		if d <= 0 {
			sc.cond.Broadcast()
		} else {
			sc.readTimer = time.AfterFunc(d, func() {
				sc.mu.Lock()
				sc.cond.Broadcast()
				sc.mu.Unlock()
			})
		}
	}
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls. A Write that is
// already in progress keeps the deadline it started with.
func (sc *serverConn) SetWriteDeadline(t time.Time) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.writeDeadline = t
	return nil
}
