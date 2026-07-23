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
	"sync/atomic"
	"testing"
	"time"

	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
)

// A blocked or failing CONNECT response is connection-local. The shared
// backend consumer must continue processing control packets while that socket
// write is unresolved, and a failed response must never signal establishment.
func TestHTTPConnectInitialResponseFailureDoesNotBlockBackendConsumer(t *testing.T) {
	const (
		agentID   = "response-progress-agent"
		dialID    = int64(1201)
		connectID = int64(1202)
	)
	server := newWriterTestServer(2)
	backend, stream := newWriterTestBackend(context.Background(), agentID)
	httpWriter := newWriterTestGateFailHTTP(errors.New("CONNECT response failed"))
	connected := make(chan struct{})
	var closeCalls atomic.Int32
	connection := &ProxyClientConnection{
		Mode:                ModeHTTPConnect,
		HTTP:                httpWriter,
		CloseHTTP:           func() error { closeCalls.Add(1); return nil },
		closed:              make(chan struct{}),
		connected:           connected,
		httpInitialResponse: []byte(httpConnectSuccessResponse),
		start:               time.Now(),
		backend:             backend,
		dialID:              dialID,
		agentID:             agentID,
	}
	server.PendingDial.Add(dialID, connection)
	consumer := startWriterTestBackendConsumer(t, server, backend, agentID, 4)

	consumer.recvCh <- writerTestDialResponse(dialID, connectID)
	select {
	case <-httpWriter.started:
	case <-time.After(writerTestSafetyTimeout):
		t.Fatal("CONNECT response write did not start")
	}
	consumer.recvCh <- writerTestDrainPacket()
	writerTestEventually(t, "backend consumer progress during blocked CONNECT response", backend.IsDraining)
	select {
	case <-connected:
		t.Fatal("connected was signaled before the CONNECT response completed")
	default:
	}

	httpWriter.unblock()
	writerTestEventually(t, "failed CONNECT response cleanup", func() bool {
		return closeCalls.Load() == 1 && stream.count(client.PacketType_CLOSE_REQ) == 1
	})
	select {
	case <-connected:
		t.Fatal("connected was signaled after the CONNECT response failed")
	default:
	}
	if got, err := server.getFrontend(agentID, connectID); err != nil || got != connection {
		t.Fatalf("failed-response terminal entry = %p, %v; want retained %p", got, err, connection)
	}

	consumer.recvCh <- closeRspPkt(connectID, "")
	writerTestEventually(t, "failed-response acknowledgement removal", func() bool {
		_, err := server.getFrontend(agentID, connectID)
		return err != nil
	})
}
