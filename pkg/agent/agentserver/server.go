package agentserver

import (
	"io"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
	"fmt"
	"net"
)

// ProxyClientConnection...
type ProxyClientConnection struct {
	Mode string
	Grpc agent.ProxyService_ProxyServer
	// Http http.ResponseWriter
	Http net.Conn
	connected chan struct{}
	connectID int64
}

func (c *ProxyClientConnection) send(pkt *agent.Packet) error {
	if c.Mode == "grpc" {
		stream := c.Grpc
		return stream.Send(pkt)
	} else if c.Mode == "http-connect" {
		if pkt.Type != agent.PacketType_DATA {
			return nil
		}
		writer := c.Http
		glog.Warningf("pkt is set to %v.", pkt)
		glog.Warningf("GetData() returns %v.", pkt.GetData())
		_, err := writer.Write(pkt.GetData().Data)
		return err
	} else {
		return fmt.Errorf("attempt to send via unrecognized connection type %q", c.Mode)
	}
}

// ProxyServer ...
type ProxyServer struct {
	Backend agent.AgentService_ConnectServer

	// connID track
	Frontends   map[int64]*ProxyClientConnection
	PendingDial map[int64]*ProxyClientConnection
}

var _ agent.AgentServiceServer = &ProxyServer{}

var _ agent.ProxyServiceServer = &ProxyServer{}

// NewProxyServer ...
func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		Frontends:   make(map[int64]*ProxyClientConnection),
		PendingDial: make(map[int64]*ProxyClientConnection),
	}
}

// Agent ...
func (s *ProxyServer) Proxy(stream agent.ProxyService_ProxyServer) error {
	glog.Info("proxy request from client")

	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	go s.serveRecv(stream, recvCh)

	defer func() {
		close(recvCh)
	}()

	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				close(stopCh)
				return
			}
			if err != nil {
				glog.Warningf("stream read error: %v", err)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

func (s *ProxyServer) serveRecv(stream agent.ProxyService_ProxyServer, recvCh <-chan *agent.Packet) {
	glog.Info("start serve recv ...")
	for pkt := range recvCh {
		switch pkt.Type {
		case agent.PacketType_DIAL_REQ:
			glog.Info("received DIAL_REQ")
			if s.Backend == nil {
				glog.Info("no backend found; drop")
				continue
			}

			if err := s.Backend.Send(pkt); err != nil {
				glog.Warningf("send packet to Backend failed: $v", err)
			}
			s.PendingDial[pkt.GetDialRequest().Random] = &ProxyClientConnection{
				Mode: "grpc",
				Grpc: stream,
			}
			glog.Info("DIAL_REQ sent to backend") // got this. but backend didn't receive anything.
		case agent.PacketType_DIAL_RSP:
			glog.Info("received DIAL_RSP;; ignore on Server")
		case agent.PacketType_ACK:
			glog.Info("received ACK")
		case agent.PacketType_CLOSE:
			glog.Info("received CLOSE")
		case agent.PacketType_DATA:
			glog.Info("received DATA")
			if s.Backend == nil {
				glog.Info("no Backend found; drop")
				continue
			}

			if err := s.Backend.Send(pkt); err != nil {
				glog.Warningf("send packet to Backend failed: $v", err)
			}
			glog.Info("DATA sent to backend")
		}
	}
}

// Ignored now
func (s *ProxyServer) serveSend(stream agent.ProxyService_ProxyServer, sendCh <-chan *agent.Packet) {
	glog.Info("start serve send ...")
	for pkt := range sendCh {
		err := stream.Send(pkt)
		if err != nil {
			glog.Warningf("stream write error: %v", err)
		}
	}
}

// Connect is for agent to connect to ProxyServer as next hop
func (s *ProxyServer) Connect(stream agent.AgentService_ConnectServer) error {
	glog.Info("connect request from Backend")

	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	glog.Infof("register Backend %v", stream)
	s.Backend = stream
	defer func() {
		glog.Infof("unregister Backend %v", stream)
		s.Backend = nil
	}()

	go s.serveRecvBackend(stream, recvCh)

	defer func() {
		close(recvCh)
	}()

	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				close(stopCh)
				return
			}
			if err != nil {
				glog.Warningf("stream read error: %v", err)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

// route the packet back to the correct client
func (s *ProxyServer) serveRecvBackend(stream agent.AgentService_ConnectServer, recvCh <-chan *agent.Packet) {
	for pkt := range recvCh {
		switch pkt.Type {
		case agent.PacketType_DIAL_RSP:
			resp := pkt.GetDialResponse()
			glog.Warningf("Received dial response for %d, connectID is %d", resp.Random, resp.ConnectID)
			if client, ok := s.PendingDial[resp.Random]; !ok {
				glog.Warning("DialResp not recognized; dropped")
			} else {
				err := client.send(pkt)
				delete(s.PendingDial, resp.Random)
				if err != nil {
					glog.Warning("send to client stream error: %v", err)
				} else {
					client.connectID = resp.ConnectID
					s.Frontends[resp.ConnectID] = client
					close(client.connected)
				}
			}
		case agent.PacketType_DATA:
			resp := pkt.GetData()
			if client, ok := s.Frontends[resp.ConnectID]; ok {
				if err := client.send(pkt); err != nil {
					glog.Warningf("send to client stream error: %v", err)
				}
			}
		default:
			glog.Warningf("unrecognized packet %+v", pkt)
		}
	}
}
