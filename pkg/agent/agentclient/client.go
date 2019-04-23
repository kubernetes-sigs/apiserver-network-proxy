package agentclient

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
	"google.golang.org/grpc"
)

type AgentClient struct {
	nextConnID int64
	conns      map[int64]net.Conn
	dataChs    map[int64]chan []byte
	address    string

	stream agent.AgentService_ConnectClient
}

func NewAgentClient(address string) *AgentClient {
	a := &AgentClient{
		conns:   make(map[int64]net.Conn),
		address: address,
		dataChs: make(map[int64]chan []byte),
	}

	return a
}

func (a *AgentClient) Connect(opts ...grpc.DialOption) error {
	c, err := grpc.Dial(a.address, opts...)
	if err != nil {
		return err
	}

	client := agent.NewAgentServiceClient(c)

	a.stream, err = client.Connect(context.Background())
	if err != nil {
		return err
	}

	return nil
}

func (a *AgentClient) Serve(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			glog.Info("stop agent client.")
			return
		default:
		}

		glog.Info("waiting packets...")

		pkt, err := a.stream.Recv()
		if err == io.EOF {
			glog.Info("received EOF, exit")
			return
		}
		if err != nil {
			glog.Warningf("stream read error: %v", err)
			return
		}

		glog.Infof("[tracing] recv packet %+v", pkt)

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
				if err := a.stream.Send(resp); err != nil {
					glog.Warningf("stream send error: %v", err)
				}
				continue
			}

			connID := atomic.AddInt64(&a.nextConnID, 1)
			a.conns[connID] = conn
			a.dataChs[connID] = make(chan []byte, 5)
			var once sync.Once

			cleanup := func() {
				once.Do(func() {
					conn.Close()
					close(a.dataChs[connID])
					delete(a.conns, connID)
					delete(a.dataChs, connID)
				})
			}

			resp.GetDialResponse().ConnectID = connID
			if err := a.stream.Send(resp); err != nil {
				glog.Warningf("stream send error: %v", err)
				continue
			}

			go a.remoteToProxy(conn, connID, cleanup)
			go a.proxyToRemote(conn, a.dataChs[connID], cleanup)

		case agent.PacketType_DATA:
			glog.Info("received DATA")
			data := pkt.GetData()
			glog.Infof("[tracing] %v", data)

			if dataCh, ok := a.dataChs[data.ConnectID]; ok {
				dataCh <- data.Data
			}

		default:
			glog.Warningf("unrecognized packet type: %+v", pkt)
		}
	}
}

func (a *AgentClient) remoteToProxy(conn net.Conn, connID int64, cleanup func()) {
	defer cleanup()

	var buf [1 << 12]byte
	resp := &agent.Packet{
		Type: agent.PacketType_DATA,
	}

	for {
		n, err := conn.Read(buf[:])
		if err == io.EOF {
			return
		} else if err != nil {
			glog.Errorf("connection read error: %v", err)
			resp.Payload = &agent.Packet_Data{Data: &agent.Data{
				Error:     err.Error(),
				ConnectID: connID,
			}}
			if err := a.stream.Send(resp); err != nil {
				glog.Warningf("stream send error: %v", err)
			}
		} else {
			resp.Payload = &agent.Packet_Data{Data: &agent.Data{
				Data:      buf[:n],
				ConnectID: connID,
			}}
			if err := a.stream.Send(resp); err != nil {
				glog.Warningf("stream send error: %v", err)
			}
		}
	}
}

func (a *AgentClient) proxyToRemote(conn net.Conn, dataCh <-chan []byte, cleanup func()) {
	defer cleanup()

	for d := range dataCh {
		pos := 0
		for {
			n, err := conn.Write(d[pos:])
			if err == nil {
				break
			} else if n > 0 {
				pos += n
			} else {
				glog.Errorf("conn write error: %v", err)
				return
			}
		}
	}
}
