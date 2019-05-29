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
	"fmt"
	"io"
	"net"

	"k8s.io/klog"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

// ProxyClientConnection...
type ProxyClientConnection struct {
	Mode      string
	Grpc      agent.ProxyService_ProxyServer
	Http      net.Conn
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

// Proxy handles incoming streams from gRPC frontend.
func (s *ProxyServer) Proxy(stream agent.ProxyService_ProxyServer) error {
	klog.Info("proxy request from client")

	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	go s.serveRecvFrontend(stream, recvCh)

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
				klog.Warningf("stream read error: %v", err)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

func (s *ProxyServer) serveRecvFrontend(stream agent.ProxyService_ProxyServer, recvCh <-chan *agent.Packet) {
	klog.Info("start serve recv ...")
	for pkt := range recvCh {
		switch pkt.Type {
		case agent.PacketType_DIAL_REQ:
			klog.Info("received DIAL_REQ")
			if s.Backend == nil {
				klog.Info("no backend found; drop")
				continue
			}

			if err := s.Backend.Send(pkt); err != nil {
				klog.Warningf("send packet to Backend failed: %v", err)
			}
			s.PendingDial[pkt.GetDialRequest().Random] = &ProxyClientConnection{
				Mode:      "grpc",
				Grpc:      stream,
				connected: make(chan struct{}),
			}
			klog.Info("DIAL_REQ sent to backend") // got this. but backend didn't receive anything.

		case agent.PacketType_CLOSE_REQ:
			klog.Infof("received CLOSE_REQ(id=%d)", pkt.GetCloseRequest().ConnectID)
			if s.Backend == nil {
				klog.Info("no backend found; drop")
				continue
			}

			if err := s.Backend.Send(pkt); err != nil {
				klog.Warningf("send packet to Backend failed: %v", err)
			}
			klog.Info("CLOSE_REQ sent to backend")

		case agent.PacketType_DATA:
			klog.Infof("received DATA(id=%d)", pkt.GetData().ConnectID)
			if s.Backend == nil {
				klog.Info("no backend found; drop")
				continue
			}

			if err := s.Backend.Send(pkt); err != nil {
				klog.Warningf("send packet to Backend failed: %v", err)
			}
			klog.Info("DATA sent to backend")

		default:
			klog.Infof("Ignore %v packet coming from frontend", pkt.Type)
		}
	}
}

// Ignored now
func (s *ProxyServer) serveSend(stream agent.ProxyService_ProxyServer, sendCh <-chan *agent.Packet) {
	klog.Info("start serve send ...")
	for pkt := range sendCh {
		err := stream.Send(pkt)
		if err != nil {
			klog.Warningf("stream write error: %v", err)
		}
	}
}

// Connect is for agent to connect to ProxyServer as next hop
func (s *ProxyServer) Connect(stream agent.AgentService_ConnectServer) error {
	klog.Info("connect request from Backend")

	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	klog.Infof("register Backend %v", stream)
	s.Backend = stream
	defer func() {
		klog.Infof("unregister Backend %v", stream)
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
				klog.Warningf("stream read error: %v", err)
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
			klog.Warningf("Received dial response for %d, connectID is %d", resp.Random, resp.ConnectID)
			if client, ok := s.PendingDial[resp.Random]; !ok {
				klog.Warning("DialResp not recognized; dropped")
			} else {
				err := client.send(pkt)
				delete(s.PendingDial, resp.Random)
				if err != nil {
					klog.Warningf("dial response send to client stream error: %v", err)
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
					klog.Warningf("data send to client stream error: %v", err)
				}
			}
		case agent.PacketType_CLOSE_RSP:
			resp := pkt.GetCloseResponse()
			if client, ok := s.Frontends[resp.ConnectID]; ok {
				if err := client.send(pkt); err != nil {
					klog.Warningf("close response send to client stream error: %v", err)
				}
			}
		default:
			klog.Warningf("unrecognized packet %+v", pkt)
		}
	}
}