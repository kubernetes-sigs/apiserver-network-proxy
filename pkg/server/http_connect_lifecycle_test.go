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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

type writerTestBackendConsumer struct {
	recvCh chan *client.Packet
	done   chan struct{}

	stopOnce sync.Once
}

func startWriterTestBackendConsumer(t *testing.T, server *ProxyServer, backend *Backend, agentID string, buffer int) *writerTestBackendConsumer {
	t.Helper()
	consumer := &writerTestBackendConsumer{
		recvCh: make(chan *client.Packet, buffer),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(consumer.done)
		server.serveRecvBackend(backend, agentID, consumer.recvCh)
	}()
	t.Cleanup(func() { consumer.stop(t) })
	return consumer
}

func (c *writerTestBackendConsumer) stop(t *testing.T) {
	t.Helper()
	c.stopOnce.Do(func() { close(c.recvCh) })
	select {
	case <-c.done:
	case <-time.After(writerTestSafetyTimeout):
		t.Errorf("serveRecvBackend did not stop")
	}
}

func writerTestDrainPacket() *client.Packet {
	return &client.Packet{Type: client.PacketType_DRAIN}
}

func writerTestDialResponse(dialID, connectID int64) *client.Packet {
	return &client.Packet{
		Type: client.PacketType_DIAL_RSP,
		Payload: &client.Packet_DialResponse{
			DialResponse: &client.DialResponse{Random: dialID, ConnectID: connectID},
		},
	}
}

func TestHTTPConnectLateDataCloseRequestPolicy(t *testing.T) {
	t.Run("before close response", func(t *testing.T) {
		const (
			agentID   = "late-before-agent"
			connectID = int64(501)
		)
		server := newWriterTestServer(2)
		backend, stream := newWriterTestBackend(context.Background(), agentID)
		httpWriter := newWriterTestImmediateHTTP()
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      httpWriter,
			CloseHTTP: httpWriter.close,
			closed:    make(chan struct{}),
			agentID:   agentID,
			connectID: connectID,
			backend:   backend,
		}
		server.addEstablished(agentID, connectID, connection)
		writer, _ := connection.attachHTTPWriter(server, nil, false)
		writer.start()
		consumer := startWriterTestBackendConsumer(t, server, backend, agentID, 8)

		writer.abort(httpConnectAbortFrontendClose)
		writerTestEventually(t, "initial connection-owned close request", func() bool {
			return stream.count(client.PacketType_CLOSE_REQ) == 1
		})
		consumer.recvCh <- dataPkt(connectID, []byte("late before acknowledgement"))
		consumer.recvCh <- writerTestDrainPacket()
		writerTestEventually(t, "late DATA processing marker", backend.IsDraining)
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 1 {
			t.Fatalf("CLOSE_REQ count after pre-acknowledgement DATA = %d, want unchanged count 1", got)
		}
		got, err := server.getFrontend(agentID, connectID)
		if err != nil || got != connection {
			t.Fatalf("terminal connection before acknowledgement = %p, %v; want retained %p", got, err, connection)
		}

		consumer.recvCh <- closeRspPkt(connectID, "")
		writerTestEventually(t, "terminal entry removal after close response", func() bool {
			_, err := server.getFrontend(agentID, connectID)
			return err != nil
		})
	})

	t.Run("after close response", func(t *testing.T) {
		const (
			agentID   = "late-after-agent"
			connectID = int64(502)
		)
		server := newWriterTestServer(2)
		backend, stream := newWriterTestBackend(context.Background(), agentID)
		httpWriter := newWriterTestImmediateHTTP()
		connection := &ProxyClientConnection{
			Mode:      ModeHTTPConnect,
			HTTP:      httpWriter,
			CloseHTTP: httpWriter.close,
			closed:    make(chan struct{}),
			agentID:   agentID,
			connectID: connectID,
			backend:   backend,
		}
		server.addEstablished(agentID, connectID, connection)
		writer, _ := connection.attachHTTPWriter(server, nil, false)
		writer.start()
		consumer := startWriterTestBackendConsumer(t, server, backend, agentID, 8)

		consumer.recvCh <- closeRspPkt(connectID, "")
		writerTestEventually(t, "graceful removal before late DATA", func() bool {
			_, err := server.getFrontend(agentID, connectID)
			return err != nil
		})
		// Model the Tunnel's terminal convergence after CLOSE_RSP closed its
		// socket. This is the one legitimate connection-owned request.
		writer.abort(httpConnectAbortFrontendClose)
		writerTestEventually(t, "connection-owned close request", func() bool {
			return stream.count(client.PacketType_CLOSE_REQ) == 1
		})

		consumer.recvCh <- dataPkt(connectID, []byte("late after acknowledgement"))
		consumer.recvCh <- writerTestDrainPacket()
		writerTestEventually(t, "post-acknowledgement DATA processing marker", backend.IsDraining)
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 2 {
			t.Fatalf("CLOSE_REQ count after post-acknowledgement DATA = %d, want 2 (one additional missing-frontend request)", got)
		}
		for _, id := range stream.closeRequestIDs() {
			if id != connectID {
				t.Fatalf("CLOSE_REQ connection ID = %d, want %d", id, connectID)
			}
		}
	})
}

func TestHTTPConnectQueueOverflowIsolationAndRetention(t *testing.T) {
	const (
		agentID = "overflow-agent"
		slowID  = int64(601)
		fastID  = int64(602)
	)
	server := newWriterTestServer(1)
	backend, stream := newWriterTestBackend(context.Background(), agentID)
	slowHTTP := newWriterTestBlockingHTTP()
	fastHTTP := newWriterTestImmediateHTTP()
	slowConnection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      slowHTTP,
		CloseHTTP: slowHTTP.close,
		closed:    make(chan struct{}),
		agentID:   agentID,
		connectID: slowID,
		backend:   backend,
	}
	fastConnection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      fastHTTP,
		CloseHTTP: fastHTTP.close,
		closed:    make(chan struct{}),
		agentID:   agentID,
		connectID: fastID,
		backend:   backend,
	}
	server.addEstablished(agentID, slowID, slowConnection)
	server.addEstablished(agentID, fastID, fastConnection)
	consumer := startWriterTestBackendConsumer(t, server, backend, agentID, 16)

	consumer.recvCh <- dataPkt(slowID, []byte("in flight"))
	select {
	case <-slowHTTP.writeStarted:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("slow connection did not enter its blocked write")
	}
	consumer.recvCh <- dataPkt(slowID, []byte("queued"))
	consumer.recvCh <- dataPkt(slowID, []byte("overflow"))
	select {
	case <-slowHTTP.closeCh:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("queue overflow did not close the slow connection")
	}
	writerTestEventually(t, "overflow CLOSE_REQ", func() bool {
		return stream.count(client.PacketType_CLOSE_REQ) == 1
	})

	consumer.recvCh <- dataPkt(fastID, []byte("healthy"))
	writerTestEventually(t, "healthy connection DATA", func() bool {
		got, _, _ := fastHTTP.snapshot()
		return bytes.Equal(got, []byte("healthy"))
	})
	got, err := server.getFrontend(agentID, slowID)
	if err != nil || got != slowConnection {
		t.Fatalf("overflowed terminal entry = %p, %v; want retained %p", got, err, slowConnection)
	}
	if got, err := server.getFrontend(agentID, fastID); err != nil || got != fastConnection {
		t.Fatalf("healthy connection = %p, %v; want %p", got, err, fastConnection)
	}

	consumer.recvCh <- dataPkt(slowID, []byte("late while terminal"))
	consumer.recvCh <- writerTestDrainPacket()
	writerTestEventually(t, "terminal late-DATA marker", backend.IsDraining)
	if got := stream.count(client.PacketType_CLOSE_REQ); got != 1 {
		t.Fatalf("terminal late DATA changed CLOSE_REQ count to %d, want 1", got)
	}
	consumer.recvCh <- closeRspPkt(slowID, "")
	writerTestEventually(t, "overflow entry removal after acknowledgement", func() bool {
		_, err := server.getFrontend(agentID, slowID)
		return err != nil
	})
	if got, err := server.getFrontend(agentID, fastID); err != nil || got != fastConnection {
		t.Fatalf("healthy connection was affected by overflow acknowledgement: %p, %v", got, err)
	}
}

func TestBackendShutdownAbortsOnlyMatchingHTTPConnections(t *testing.T) {
	const agentID = "shared-agent-id"
	server := newWriterTestServer(2)
	backendA, streamA := newWriterTestBackend(context.Background(), agentID)
	backendB, _ := newWriterTestBackend(context.Background(), agentID)

	activeHTTP := newWriterTestBlockingHTTP()
	drainingHTTP := newWriterTestBlockingHTTP()
	otherHTTP := newWriterTestBlockingHTTP()
	active := &ProxyClientConnection{Mode: ModeHTTPConnect, HTTP: activeHTTP, CloseHTTP: activeHTTP.close, closed: make(chan struct{}), agentID: agentID, connectID: 701, backend: backendA}
	draining := &ProxyClientConnection{Mode: ModeHTTPConnect, HTTP: drainingHTTP, CloseHTTP: drainingHTTP.close, closed: make(chan struct{}), agentID: agentID, connectID: 702, backend: backendA}
	other := &ProxyClientConnection{Mode: ModeHTTPConnect, HTTP: otherHTTP, CloseHTTP: otherHTTP.close, closed: make(chan struct{}), agentID: agentID, connectID: 703, backend: backendB}
	server.addEstablished(agentID, active.connectID, active)
	server.addEstablished(agentID, draining.connectID, draining)
	server.addEstablished(agentID, other.connectID, other)

	activeWriter, _ := active.attachHTTPWriter(server, nil, false)
	drainingWriter, _ := draining.attachHTTPWriter(server, nil, false)
	otherWriter, _ := other.attachHTTPWriter(server, nil, false)
	activeWriter.start()
	drainingWriter.start()
	otherWriter.start()
	activeWriter.enqueueData([]byte("active"))
	drainingWriter.enqueueData([]byte("draining"))
	otherWriter.enqueueData([]byte("other backend"))
	for name, started := range map[string]<-chan struct{}{
		"active": activeHTTP.writeStarted, "draining": drainingHTTP.writeStarted, "other": otherHTTP.writeStarted,
	} {
		select {
		case <-started:
		case <-time.After(writerTestSafetyTimeout):
			t.Fatalf("%s writer did not block", name)
		}
	}
	drainingWriter.beginGracefulClose()

	consumer := startWriterTestBackendConsumer(t, server, backendA, agentID, 0)
	consumer.stop(t)
	for name, closed := range map[string]<-chan struct{}{
		"active": activeHTTP.closeCh, "draining": drainingHTTP.closeCh,
	} {
		select {
		case <-closed:
		case <-time.After(writerTestSafetyTimeout):
			t.Fatalf("backend shutdown did not close %s writer", name)
		}
	}
	if _, err := server.getFrontend(agentID, active.connectID); err == nil {
		t.Fatal("active matching connection remained established after backend shutdown")
	}
	if _, err := server.getFrontend(agentID, draining.connectID); err == nil {
		t.Fatal("gracefully draining matching connection remained established after backend shutdown")
	}
	if got, err := server.getFrontend(agentID, other.connectID); err != nil || got != other {
		t.Fatalf("different backend connection = %p, %v; want untouched %p", got, err, other)
	}
	select {
	case <-otherHTTP.closeCh:
		t.Fatal("backend shutdown closed a connection owned by a different backend pointer")
	default:
	}
	if got := streamA.count(client.PacketType_CLOSE_REQ); got != 0 {
		t.Fatalf("dead backend received %d CLOSE_REQ packets", got)
	}

	// Release the intentionally untouched writer using its own backend lifecycle.
	server.removeEstablishedIf(agentID, other.connectID, other)
	other.suppressBackendCloseRequest()
	other.abortHTTP(server, httpConnectAbortBackendShutdown)
	select {
	case <-otherHTTP.closeCh:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("cleanup did not close the different-backend writer")
	}
}

func TestHTTPConnectGracefulDrainMapLifecycle(t *testing.T) {
	const (
		agentID   = "drain-agent"
		connectID = int64(801)
	)
	server := newWriterTestServer(2)
	httpWriter := newWriterTestBlockingHTTP()
	removedAtClose := make(chan bool, 1)
	connection := &ProxyClientConnection{
		Mode:      ModeHTTPConnect,
		HTTP:      httpWriter,
		closed:    make(chan struct{}),
		agentID:   agentID,
		connectID: connectID,
	}
	connection.CloseHTTP = func() error {
		_, err := server.getFrontend(agentID, connectID)
		removedAtClose <- err != nil
		return httpWriter.close()
	}
	server.addEstablished(agentID, connectID, connection)
	writer, _ := connection.attachHTTPWriter(server, nil, false)
	writer.start()
	writer.enqueueData([]byte("first"))
	select {
	case <-httpWriter.writeStarted:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("graceful writer did not enter its blocked first write")
	}
	writer.enqueueData([]byte("second"))
	writer.beginGracefulClose()
	if got, err := server.getFrontend(agentID, connectID); err != nil || got != connection {
		t.Fatalf("draining connection = %p, %v; want discoverable %p", got, err, connection)
	}

	httpWriter.release()
	select {
	case removed := <-removedAtClose:
		if !removed {
			t.Fatal("connection was still established when terminal CloseHTTP began")
		}
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("graceful writer did not reach terminal CloseHTTP")
	}
	stream, closes, _ := httpWriter.snapshot()
	if !bytes.Equal(stream, []byte("firstsecond")) {
		t.Fatalf("gracefully drained stream = %q, want %q", stream, "firstsecond")
	}
	if closes != 1 {
		t.Fatalf("CloseHTTP calls = %d, want 1", closes)
	}
}

func TestHTTPConnectWedgedGracefulCloseIsConnectionLocal(t *testing.T) {
	const (
		agentID   = "wedged-graceful-agent"
		wedgedID  = int64(901)
		healthyID = int64(902)
	)
	server := newWriterTestServer(2)
	backend, stream := newWriterTestBackend(context.Background(), agentID)
	wedgedHTTP := newWriterTestBlockingHTTP()
	healthyHTTP := newWriterTestImmediateHTTP()
	wedged := &ProxyClientConnection{Mode: ModeHTTPConnect, HTTP: wedgedHTTP, CloseHTTP: wedgedHTTP.close, closed: make(chan struct{}), agentID: agentID, connectID: wedgedID, backend: backend}
	healthy := &ProxyClientConnection{Mode: ModeHTTPConnect, HTTP: healthyHTTP, CloseHTTP: healthyHTTP.close, closed: make(chan struct{}), agentID: agentID, connectID: healthyID, backend: backend}
	server.addEstablished(agentID, wedgedID, wedged)
	server.addEstablished(agentID, healthyID, healthy)
	wedgedWriter, _ := wedged.attachHTTPWriter(server, nil, false)
	wedgedWriter.start()
	consumer := startWriterTestBackendConsumer(t, server, backend, agentID, 8)

	consumer.recvCh <- dataPkt(wedgedID, []byte("final blocked DATA"))
	select {
	case <-wedgedHTTP.writeStarted:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("wedged graceful writer did not block")
	}
	consumer.recvCh <- closeRspPkt(wedgedID, "")
	consumer.recvCh <- writerTestDrainPacket()
	writerTestEventually(t, "graceful close processing marker", backend.IsDraining)
	if got, err := server.getFrontend(agentID, wedgedID); err != nil || got != wedged {
		t.Fatalf("wedged graceful connection = %p, %v; want retained %p", got, err, wedged)
	}

	consumer.recvCh <- dataPkt(healthyID, []byte("healthy progress"))
	writerTestEventually(t, "healthy progress beside wedged graceful writer", func() bool {
		got, _, _ := healthyHTTP.snapshot()
		return bytes.Equal(got, []byte("healthy progress"))
	})
	select {
	case <-wedgedHTTP.closeCh:
		t.Fatal("graceful close acquired an implicit timeout before backend shutdown")
	default:
	}

	consumer.stop(t)
	select {
	case <-wedgedHTTP.closeCh:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("backend shutdown did not reap the wedged graceful writer")
	}
	if _, err := server.getFrontend(agentID, wedgedID); err == nil {
		t.Fatal("wedged graceful connection remained established after backend shutdown")
	}
	if got := stream.count(client.PacketType_CLOSE_REQ); got != 0 {
		t.Fatalf("backend shutdown emitted %d CLOSE_REQ packets on the dead stream", got)
	}
}

// This test races the real DIAL_RSP routing path against the Tunnel teardown
// ownership operation. It asserts publication and side-effect semantics, not
// the current implementation's number or placement of terminal checks.
func TestHTTPConnectDialResponseTeardownRace(t *testing.T) {
	const iterations = 80
	for i := 0; i < iterations; i++ {
		agentID := "attachment-race-agent"
		dialID := int64(10000 + i)
		connectID := int64(11000 + i)
		server := newWriterTestServer(2)
		backend, stream := newWriterTestBackend(context.Background(), agentID)
		httpWriter := newWriterTestImmediateHTTP()
		connected := make(chan struct{})
		connection := &ProxyClientConnection{
			Mode:                ModeHTTPConnect,
			HTTP:                httpWriter,
			CloseHTTP:           httpWriter.close,
			closed:              make(chan struct{}),
			connected:           connected,
			httpInitialResponse: []byte(httpConnectSuccessResponse),
			start:               time.Now(),
			backend:             backend,
			dialID:              dialID,
			agentID:             agentID,
		}
		server.PendingDial.Add(dialID, connection)
		consumer := startWriterTestBackendConsumer(t, server, backend, agentID, 0)

		start := make(chan struct{})
		var teardownOwned atomic.Bool
		var racers sync.WaitGroup
		racers.Add(2)
		go func() {
			defer racers.Done()
			<-start
			consumer.recvCh <- writerTestDialResponse(dialID, connectID)
		}()
		go func() {
			defer racers.Done()
			<-start
			if server.PendingDial.Remove(dialID) != nil {
				teardownOwned.Store(true)
			}
			connection.abortHTTP(server, httpConnectAbortFrontendClose)
		}()
		close(start)
		racers.Wait()
		consumer.recvCh <- writerTestDrainPacket()
		writerTestEventually(t, "DIAL_RSP/teardown processing marker", backend.IsDraining)
		writerTestEventually(t, "one close request from DIAL_RSP/teardown convergence", func() bool {
			return stream.count(client.PacketType_CLOSE_REQ) == 1
		})

		connection.httpMu.Lock()
		writerBefore := connection.httpWriter
		terminal := connection.httpTerminal
		connection.httpMu.Unlock()
		if !terminal {
			t.Fatal("teardown race did not leave the connection terminal")
		}
		writerAfter, attached := connection.attachHTTPWriter(server, nil, false)
		if writerBefore == nil {
			if attached || writerAfter != nil {
				t.Fatal("terminal teardown permitted a fresh writer attachment")
			}
		} else if !attached || writerAfter != writerBefore {
			t.Fatalf("terminal lookup replaced writer %p with %p (attached=%t)", writerBefore, writerAfter, attached)
		}

		if teardownOwned.Load() {
			select {
			case <-connected:
				t.Fatal("connected was signaled after pending teardown won ownership")
			default:
			}
			if _, err := server.getFrontend(agentID, connectID); err == nil {
				t.Fatal("connection was published after pending teardown won ownership")
			}
		}

		if got, err := server.getFrontend(agentID, connectID); err == nil {
			if got != connection {
				t.Fatalf("established pointer = %p, want %p", got, connection)
			}
			consumer.recvCh <- dataPkt(connectID, []byte("late terminal DATA"))
			consumer.recvCh <- closeRspPkt(connectID, "")
			writerTestEventually(t, "terminal entry acknowledgement", func() bool {
				_, err := server.getFrontend(agentID, connectID)
				return err != nil
			})
		}
		consumer.stop(t)
		if got := stream.count(client.PacketType_CLOSE_REQ); got != 1 {
			t.Fatalf("DIAL_RSP/teardown CLOSE_REQ count = %d, want 1", got)
		}
		ids := stream.closeRequestIDs()
		if len(ids) != 1 || ids[0] != connectID {
			t.Fatalf("DIAL_RSP/teardown CLOSE_REQ IDs = %v, want [%d]", ids, connectID)
		}
		writerTestEventually(t, "DIAL_RSP/teardown HTTP close", func() bool {
			_, closes, _ := httpWriter.snapshot()
			return closes == 1
		})
		written, closes, _ := httpWriter.snapshot()
		if len(written) > 0 && !bytes.Equal(written, []byte(httpConnectSuccessResponse)) {
			t.Fatalf("DIAL_RSP/teardown wrote %q, want at most one complete CONNECT response", written)
		}
		if closes != 1 {
			t.Fatalf("DIAL_RSP/teardown CloseHTTP calls = %d, want 1", closes)
		}
		if _, err := server.getFrontend(agentID, connectID); err == nil {
			t.Fatal("connection reappeared after terminal acknowledgement")
		}
	}
}
