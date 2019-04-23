package agentserver

import (
	"net/http"
	"io"
	"github.com/anfernee/proxy-service/proto/agent"
	"math/rand"
	"github.com/golang/glog"
)

type Tunnel struct {
	Server *ProxyServer
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	glog.Infof("Received %s request to %q from %v",
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
	glog.Infof("Set pending[%d] to %v", random, w)
	connected := make(chan struct{})
	connection := &ProxyClientConnection{
		Mode: "http-connect",
		// Http: w,
		Http: conn,
		connected: connected,
	}
	t.Server.PendingDial[random] = connection
	if t.Server.Backend == nil {
		http.Error(w, "currently no tunnels available", http.StatusInternalServerError)
	}
	if err := t.Server.Backend.Send(dialRequest); err != nil {
		glog.Errorf("failed to tunnel dial request %v", err)
		// http.Error(w, fmt.Sprintf("failed to tunnel dial request %v", err), http.StatusInternalServerError) // TODO REINSTATE
		return
	}
	ctxt := t.Server.Backend.Context()
	if ctxt.Err() != nil {
		glog.Errorf("context reports error %v", err)
	}
	select {
	case <-ctxt.Done():
		glog.Errorf("context reports done!!!")
	default:
	}
	select {
	case <-connection.connected: // Waiting for response before we begin full communication.
	}

	defer conn.Close()

	glog.Infof("Starting proxy to %q", r.Host)
	pkt := make([]byte, 1 << 12)
	for {
		n, err := conn.Read(pkt[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			glog.Errorf("Received error on connection %v", err)
			break
		}
		packet := &agent.Packet{
			Type: agent.PacketType_DATA,
			Payload: &agent.Packet_Data{
				Data: &agent.Data{
					ConnectID: connection.connectID,
					Data:	 pkt[:n],
				},
			},
		}
		t.Server.Backend.Send(packet)
		glog.Infof("Forwarding on tunnel, packet %v", string(pkt[:n]))
	}
	glog.Infof("Stopping transfer to %q", r.Host)
	delete(t.Server.Frontends, connection.connectID)
}


