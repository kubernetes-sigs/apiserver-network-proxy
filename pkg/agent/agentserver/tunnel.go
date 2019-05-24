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

package agentserver

import (
	"io"
	"math/rand"
	"net/http"

	"k8s.io/klog"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

type Tunnel struct {
	Server *ProxyServer
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	klog.Infof("Received %s request to %q from %v",
		r.Method,
		r.Host,
		r.TLS.PeerCertificates[0].Subject.CommonName) // can do authz with certs
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

	conn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	random := rand.Int63()
	dialRequest := &agent.Packet{
		Type: agent.PacketType_DIAL_REQ,
		Payload: &agent.Packet_DialRequest{
			DialRequest: &agent.DialRequest{
				Protocol: "tcp",
				Address:  r.Host,
				Random:   random,
			},
		},
	}
	klog.Infof("Set pending[%d] to %v", random, w)
	connected := make(chan struct{})
	connection := &ProxyClientConnection{
		Mode: "http-connect",
		// Http: w,
		Http:      conn,
		connected: connected,
	}
	t.Server.PendingDial[random] = connection
	if t.Server.Backend == nil {
		http.Error(w, "currently no tunnels available", http.StatusInternalServerError)
	}
	if err := t.Server.Backend.Send(dialRequest); err != nil {
		klog.Errorf("failed to tunnel dial request %v", err)
		// http.Error(w, fmt.Sprintf("failed to tunnel dial request %v", err), http.StatusInternalServerError) // TODO REINSTATE
		return
	}
	ctxt := t.Server.Backend.Context()
	if ctxt.Err() != nil {
		klog.Errorf("context reports error %v", err)
	}
	select {
	case <-ctxt.Done():
		klog.Errorf("context reports done!!!")
	default:
	}
	select {
	case <-connection.connected: // Waiting for response before we begin full communication.
	}

	defer conn.Close()

	klog.Infof("Starting proxy to %q", r.Host)
	pkt := make([]byte, 1<<12)
	for {
		n, err := conn.Read(pkt[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			klog.Errorf("Received error on connection %v", err)
			break
		}
		packet := &agent.Packet{
			Type: agent.PacketType_DATA,
			Payload: &agent.Packet_Data{
				Data: &agent.Data{
					ConnectID: connection.connectID,
					Data:      pkt[:n],
				},
			},
		}
		t.Server.Backend.Send(packet)
		klog.Infof("Forwarding on tunnel, packet %v", string(pkt[:n]))
	}
	klog.Infof("Stopping transfer to %q", r.Host)
	delete(t.Server.Frontends, connection.connectID)
}
