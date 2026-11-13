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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

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

	// ctx is cancelled by Close. It ends writes in progress, aborts their
	// sends if they have not reached the agent stream yet, and stops the
	// closeWithBackend watcher.
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	cond *sync.Cond
	// connID is set once a successful DIAL_RSP has been accepted.
	connID int64
	// buffer holds DATA payloads received from the agent and not yet read.
	// It holds at most maxBuffered payloads; once full, Send blocks until
	// Read drains it or the connection is closed. Send runs on the agent's
	// receive loop, so as with a full HTTP-CONNECT write queue, a reader that
	// stops reading stalls every connection on that agent.
	buffer      [][]byte
	maxBuffered int
	rdata       []byte
	closed      bool
	// localClosed is set by Close. Unlike a close by the agent, it discards
	// data that has not been read.
	localClosed   bool
	closeErr      error
	readDeadline  time.Time
	writeDeadline time.Time
	// writeDeadlineChanged is closed and replaced when the write deadline
	// changes, so that a Write in progress applies the new deadline.
	writeDeadlineChanged chan struct{}
	// writeErr is set once a Write has timed out. Every later Write fails
	// with it, because the abandoned DATA packet may still reach the agent.
	writeErr  error
	readTimer *time.Timer

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

		writeDeadlineChanged: make(chan struct{}),
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
			sc.handleDialError(dialResponseError(resp.Error))
			return nil
		}
		return sc.handleDialSuccess(resp.ConnectID)
	}
	return fmt.Errorf("attempt to send via unrecognized packet type %v", pkt.Type)
}

// dialResponseError returns the error a failed DIAL_RSP reports. The server
// itself fails pending dials with DIAL_RSPs carrying these errors, which are
// returned as themselves so that callers can match them with errors.Is.
func dialResponseError(msg string) error {
	for _, err := range []error{errBackendClosedWhileDialing, errBackendDialTimeout} {
		if msg == err.Error() {
			return err
		}
	}
	return errors.New(msg)
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
		if sc.localClosed {
			return 0, net.ErrClosed
		}
		if !sc.readDeadline.IsZero() && !time.Now().Before(sc.readDeadline) {
			return 0, os.ErrDeadlineExceeded
		}

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

		sc.cond.Wait()
	}
}

// Write sends b to the agent in DATA packets of at most
// maxConnDataPacketSize bytes.
func (sc *serverConn) Write(b []byte) (n int, err error) {
	for n < len(b) {
		chunk := b[n:min(len(b), n+maxConnDataPacketSize)]
		if err := sc.writePacket(chunk); err != nil {
			return n, err
		}
		n += len(chunk)
	}
	return n, nil
}

// writePacket sends data to the agent in one DATA packet. It returns once the
// packet is sent, the connection is closed, or the write deadline passes.
//
// Once a gRPC Send has started it cannot be interrupted short of ending the
// agent stream, which would break every connection on the agent. The send
// therefore runs in its own goroutine, which is left to finish when
// writePacket gives up. If it had not reached the agent stream yet, the
// cancelled context aborts it; otherwise it is the single send holding the
// backend send lock, and ends with the stream if it does not complete.
func (sc *serverConn) writePacket(data []byte) error {
	sc.mu.Lock()
	if sc.closed {
		sc.mu.Unlock()
		return net.ErrClosed
	}
	if sc.writeErr != nil {
		err := sc.writeErr
		sc.mu.Unlock()
		return err
	}
	if !sc.writeDeadline.IsZero() && !time.Now().Before(sc.writeDeadline) {
		// Nothing has been sent, so unlike a timeout of a send in progress,
		// this does not fail later Writes.
		sc.mu.Unlock()
		return os.ErrDeadlineExceeded
	}
	connID := sc.connID
	sc.mu.Unlock()

	// An abandoned send outlives this call, so it must not reference the
	// caller's buffer.
	packet := &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: connID,
				Data:      bytes.Clone(data),
			},
		},
	}
	ctx, cancel := context.WithCancel(sc.ctx)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- sc.backend.SendContext(ctx, packet)
	}()

	for {
		sc.mu.Lock()
		deadline := sc.writeDeadline
		deadlineChanged := sc.writeDeadlineChanged
		sc.mu.Unlock()

		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			expired = timer.C
		}
		select {
		case err := <-errCh:
			if timer != nil {
				timer.Stop()
			}
			if err != nil && sc.ctx.Err() != nil {
				return net.ErrClosed
			}
			return err
		case <-sc.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return net.ErrClosed
		case <-expired:
			sc.mu.Lock()
			if sc.writeErr == nil {
				sc.writeErr = os.ErrDeadlineExceeded
			}
			sc.mu.Unlock()
			return os.ErrDeadlineExceeded
		case <-deadlineChanged:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

// Close closes the connection locally and tells the agent to close it. It
// does not wait for the agent to be told: a Write may be blocked sending to
// the agent, and the close message would be queued behind it.
func (sc *serverConn) Close() error {
	sc.closeOnce.Do(func() {
		sc.mu.Lock()
		sc.closed = true
		sc.localClosed = true
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
		sc.backend.queueSend(newCloseRequestPacket(connID))
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

// SetWriteDeadline sets the deadline for Write calls, including one that is
// already in progress. After a Write has timed out, the stream of DATA sent
// to the agent may be incomplete, so all later Writes fail; as with tls.Conn,
// a new deadline does not make the connection writable again.
func (sc *serverConn) SetWriteDeadline(t time.Time) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.writeDeadline = t
	close(sc.writeDeadlineChanged)
	sc.writeDeadlineChanged = make(chan struct{})
	return nil
}
