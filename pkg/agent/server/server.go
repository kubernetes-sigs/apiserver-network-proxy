package agentserver

import (
	"io"
	"net"
	"sync/atomic"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
)

// implement close

// ProxyServer ...
type ProxyServer struct {
	nextConnID int64
	conns      map[int64]net.Conn
}

// NewProxyServer ...
func NewProxyServer() *ProxyServer {
	return &ProxyServer{
		conns: make(map[int64]net.Conn),
	}
}

// Agent ...
func (s *ProxyServer) Proxy(stream agent.ProxyService_ProxyServer) error {
	glog.Info("proxy request from client")

	sendCh := make(chan *agent.Packet, 10)
	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	go s.serveRecv(stream, sendCh, recvCh)
	go s.serveSend(stream, sendCh)

	defer func() {
		close(sendCh)
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

func (s *ProxyServer) serveRecv(stream agent.ProxyService_ProxyServer, sendCh chan<- *agent.Packet, recvCh <-chan *agent.Packet) {
	glog.Info("start serve recv ...")
	for pkt := range recvCh {
		switch pkt.Type {
		case agent.PacketType_DIAL_REQ:
			glog.Info("received DIAL_REQ")

			resp := &agent.Packet{
				Type:    agent.PacketType_DIAL_RSP,
				Payload: &agent.Packet_DialResponse{DialResponse: &agent.DialResponse{}},
			}

			dialReq := pkt.GetDialRequest()
			resp.GetDialResponse().Random = dialReq.Random

			conn, err := net.Dial(dialReq.Protocol, dialReq.Address)
			if err != nil {
				resp.GetDialResponse().Error = err.Error()
				sendCh <- resp
				continue
			}

			connID := atomic.AddInt64(&s.nextConnID, 1)
			s.conns[connID] = conn

			resp.GetDialResponse().ConnectID = connID
			sendCh <- resp

			go s.pipe(conn, sendCh, connID)

		case agent.PacketType_DIAL_RSP:
			glog.Info("received DIAL_RSP;; ignore on server")
			// ignore ...
		case agent.PacketType_ACK:
			glog.Info("received ACK")
		case agent.PacketType_CLOSE:
			glog.Info("received CLOSE")
		case agent.PacketType_DATA:
			glog.Info("received DATA")
			data := pkt.GetData()

			if conn, ok := s.conns[data.ConnectID]; ok {
				pos := 0
				for {
					n, err := conn.Write(data.Data[pos:])
					if n > 0 {
						pos += n
						continue
					}
					if err != nil {
						glog.Errorf("conn write error: %v", err)
						return
					}
				}
			}

			// drop packet if not recognized

			// TODO: ACK?
		}
	}
}

func (s *ProxyServer) serveSend(stream agent.ProxyService_ProxyServer, sendCh <-chan *agent.Packet) {
	glog.Info("start serve send ...")
	for pkt := range sendCh {
		err := stream.Send(pkt)
		if err != nil {
			glog.Warningf("stream write error: %v", err)
		}
	}
}

func (s *ProxyServer) pipe(conn net.Conn, sendCh chan<- *agent.Packet, connID int64) {
	var buf [1 << 12]byte

	resp := &agent.Packet{
		Type: agent.PacketType_DATA,
	}

	for {
		n, err := conn.Read(buf[:])
		if err == io.EOF {
			close(sendCh)
			return
		} else if err != nil {
			glog.Errorf("connection read error: %v", err)
			resp.Payload = &agent.Packet_Data{Data: &agent.Data{
				Error:     err.Error(),
				ConnectID: connID,
			}}
			sendCh <- resp
		} else {
			resp.Payload = &agent.Packet_Data{Data: &agent.Data{
				Data:      buf[:n],
				ConnectID: connID,
			}}
			sendCh <- resp
		}

	}
}
