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

	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

type proxyAddr struct {
	network string
	address string
}

func (a *proxyAddr) Network() string { return a.network }
func (a *proxyAddr) String() string  { return a.address }

// serverConn is an implementation of net.Conn that proxies data over an established backend agent tunnel.
type serverConn struct {
	server      *ProxyServer
	backend     *Backend
	connID      int64
	dialID      int64
	agentID     string
	dialAddress string

	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	cond          *sync.Cond
	buffer        [][]byte
	rdata         []byte
	closed        bool
	closeErr      error
	readDeadline  time.Time
	writeDeadline time.Time
	readTimer     *time.Timer

	connected chan struct{}
	dialErrCh chan error
	closeCh   chan struct{}
	closeOnce sync.Once
}

var _ net.Conn = &serverConn{}
var _ ProxyStream = &serverConn{}
var _ io.Closer = &serverConn{}

func newServerConn(server *ProxyServer, backend *Backend, dialID int64, dialAddress string) *serverConn {
	ctx, cancel := context.WithCancel(context.Background())
	sc := &serverConn{
		server:      server,
		backend:     backend,
		dialID:      dialID,
		agentID:     backend.GetAgentID(),
		dialAddress: dialAddress,
		ctx:         ctx,
		cancel:      cancel,
		connected:   make(chan struct{}),
		dialErrCh:   make(chan error, 1),
		closeCh:     make(chan struct{}),
	}
	sc.cond = sync.NewCond(&sc.mu)
	return sc
}

func (sc *serverConn) Send(pkt *client.Packet) error {
	switch pkt.Type {
	case client.PacketType_CLOSE_RSP:
		sc.handleClose(pkt.GetCloseResponse().Error)
		return nil
	case client.PacketType_DIAL_CLS:
		sc.handleDialClose()
		return nil
	case client.PacketType_DATA:
		return sc.handleData(pkt.GetData().Data)
	case client.PacketType_DIAL_RSP:
		dialErr := pkt.GetDialResponse().Error
		if dialErr != "" {
			sc.handleDialError(dialErr)
		} else {
			sc.mu.Lock()
			sc.connID = pkt.GetDialResponse().ConnectID
			sc.mu.Unlock()
			select {
			case <-sc.connected:
			default:
				close(sc.connected)
			}
		}
		return nil
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

func (sc *serverConn) handleData(data []byte) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.closed {
		return io.ErrClosedPipe
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

func (sc *serverConn) handleDialClose() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.closed {
		sc.closed = true
		sc.closeErr = errors.New("dial closed")
		sc.cond.Broadcast()
	}
	select {
	case sc.dialErrCh <- errors.New("dial closed"):
	default:
	}
}

func (sc *serverConn) handleDialError(errMsg string) {
	select {
	case sc.dialErrCh <- errors.New(errMsg):
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
			sc.buffer = sc.buffer[1:]
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
		return 0, io.ErrClosedPipe
	}
	if !sc.writeDeadline.IsZero() && !time.Now().Before(sc.writeDeadline) {
		sc.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	connID := sc.connID
	backend := sc.backend
	sc.mu.Unlock()

	packet := &client.Packet{
		Type: client.PacketType_DATA,
		Payload: &client.Packet_Data{
			Data: &client.Data{
				ConnectID: connID,
				Data:      b,
			},
		},
	}
	if err := backend.Send(packet); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (sc *serverConn) Close() error {
	sc.closeOnce.Do(func() {
		sc.cancel()
		sc.mu.Lock()
		sc.closed = true
		if sc.closeErr == nil {
			sc.closeErr = io.ErrClosedPipe
		}
		if sc.readTimer != nil {
			sc.readTimer.Stop()
			sc.readTimer = nil
		}
		sc.cond.Broadcast()
		connID := sc.connID
		dialID := sc.dialID
		agentID := sc.agentID
		backend := sc.backend
		sc.mu.Unlock()

		if connID != 0 {
			sc.server.removeEstablished(agentID, connID)
			packet := &client.Packet{
				Type: client.PacketType_CLOSE_REQ,
				Payload: &client.Packet_CloseRequest{
					CloseRequest: &client.CloseRequest{
						ConnectID: connID,
					},
				},
			}
			_ = backend.Send(packet)
		} else {
			sc.server.PendingDial.Remove(dialID)
			packet := &client.Packet{
				Type: client.PacketType_DIAL_CLS,
				Payload: &client.Packet_CloseDial{
					CloseDial: &client.CloseDial{
						Random: dialID,
					},
				},
			}
			_ = backend.Send(packet)
		}
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

func (sc *serverConn) SetWriteDeadline(t time.Time) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.writeDeadline = t
	return nil
}
