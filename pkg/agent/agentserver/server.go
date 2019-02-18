package agentserver

import (
	"io"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
)

// ProxyServer ...
type ProxyServer struct {
	backend agent.AgentService_ConnectServer

	// connID track
	frontends   map[int64]agent.ProxyService_ProxyServer
	pendingDial map[int64]agent.ProxyService_ProxyServer
}

var _ agent.AgentServiceServer = &ProxyServer{}

var _ agent.ProxyServiceServer = &ProxyServer{}

// NewProxyServer ...
func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		frontends:   make(map[int64]agent.ProxyService_ProxyServer),
		pendingDial: make(map[int64]agent.ProxyService_ProxyServer),
	}
}

// Proxy ...
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
			if s.backend == nil {
				glog.Info("no backend found; drop")
				continue
			}

			if err := s.backend.Send(pkt); err != nil {
				glog.Warningf("send packet to backend failed: $v", err)
			}
			s.pendingDial[pkt.GetDialRequest().Random] = stream
		case agent.PacketType_DIAL_RSP:
			glog.Info("received DIAL_RSP;; ignore on server")
		case agent.PacketType_ACK:
			glog.Info("received ACK")
		case agent.PacketType_CLOSE:
			glog.Info("received CLOSE")
		case agent.PacketType_DATA:
			glog.Info("received DATA")
			if s.backend == nil {
				glog.Info("no backend found; drop")
				continue
			}

			if err := s.backend.Send(pkt); err != nil {
				glog.Warningf("send packet to backend failed: $v", err)
			}
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
	glog.Info("connect request from backend")

	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	glog.Infof("register backend %v", stream)
	s.backend = stream
	defer func() {
		glog.Infof("unregister backend %v", stream)
		s.backend = nil
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
			if stream, ok := s.pendingDial[resp.Random]; !ok {
				glog.Warning("DialResp not recognized; dropped")
			} else {
				err := stream.Send(pkt)
				delete(s.pendingDial, resp.Random)
				if err != nil {
					glog.Warning("send to client stream error: %v", err)
				} else {
					s.frontends[resp.ConnectID] = stream
				}
			}
		case agent.PacketType_DATA:
			resp := pkt.GetData()
			if stream, ok := s.frontends[resp.ConnectID]; ok {
				if err := stream.Send(pkt); err != nil {
					glog.Warningf("send to client stream error: %v", err)
				}
			}
		default:
			glog.Warningf("unrecognized packet %+v", pkt)
		}
	}
}
