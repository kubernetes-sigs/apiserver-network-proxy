package agentclient

import (
	"context"
	"io"
	"net"
	"sync/atomic"

	"github.com/anfernee/proxy-service/proto/agent"
	"github.com/golang/glog"
	"google.golang.org/grpc"
)

type AgentClient struct {
	nextConnID int64
	conns      map[int64]net.Conn
	address    string

	stream agent.AgentService_ConnectClient
}

func NewAgentClient(address string) *AgentClient {
	a := &AgentClient{
		conns:   make(map[int64]net.Conn),
		address: address,
	}

	return a
}

func (a *AgentClient) Connect() error {
	c, err := grpc.Dial(a.address, grpc.WithInsecure())
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
			return
		default:
		}

		pkt, err := a.stream.Recv()
		if err == io.EOF {
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

			resp.GetDialResponse().ConnectID = connID
			if err := a.stream.Send(resp); err != nil {
				glog.Warningf("stream send error: %v", err)
			}

			go a.pipe(conn, connID)

		case agent.PacketType_DATA:
			glog.Info("received DATA")
			data := pkt.GetData()

			if conn, ok := a.conns[data.ConnectID]; ok {
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

		default:
			glog.Warningf("unreconginzed packet type: %+v", pkt)
		}
	}
}

// TODO: use send channel
func (a *AgentClient) pipe(conn net.Conn, connID int64) {
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
