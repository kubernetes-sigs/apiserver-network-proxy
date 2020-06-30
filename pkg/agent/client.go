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

package agent

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

// connContext tracks a connection from agent to node network.
type connContext struct {
	conn      net.Conn
	cleanFunc func()
	dataCh    chan []byte
	cleanOnce sync.Once
}

func (c *connContext) cleanup() {
	c.cleanOnce.Do(c.cleanFunc)
}

type connectionManager struct {
	mu          sync.RWMutex
	connections map[int64]*connContext
}

func (cm *connectionManager) Add(connID int64, ctx *connContext) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.connections[connID] = ctx
}

func (cm *connectionManager) Get(connID int64) (*connContext, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctx, ok := cm.connections[connID]
	return ctx, ok
}

func (cm *connectionManager) Delete(connID int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.connections, connID)
}

func newConnectionManager() *connectionManager {
	return &connectionManager{
		connections: make(map[int64]*connContext),
	}
}

// AgentClient runs on the node network side. It connects to proxy server and establishes
// a stream connection from which it sends and receives network traffic.
type AgentClient struct {
	nextConnID int64

	connManager *connectionManager

	cs *ClientSet // the clientset that includes this AgentClient.

	stream   agent.AgentService_ConnectClient
	agentID  string
	serverID string // the id of the proxy server this client connects to.

	// connect opts
	address string
	opts    []grpc.DialOption
	conn    *grpc.ClientConn
	stopCh  chan struct{}
	// locks
	sendLock      sync.Mutex
	recvLock      sync.Mutex
	probeInterval time.Duration // interval between probe pings

	// file path contains service account token.
	// token's value is auto-rotated by kubernetes, based on projected volume configuration.
	serviceAccountTokenPath string
}

func newAgentClient(address, agentID string, cs *ClientSet, opts ...grpc.DialOption) (*AgentClient, int, error) {
	a := &AgentClient{
		cs:                      cs,
		address:                 address,
		agentID:                 agentID,
		opts:                    opts,
		probeInterval:           cs.probeInterval,
		stopCh:                  make(chan struct{}),
		serviceAccountTokenPath: cs.serviceAccountTokenPath,
		connManager:             newConnectionManager(),
	}
	serverCount, err := a.Connect()
	if err != nil {
		return nil, 0, err
	}
	return a, serverCount, nil
}

// Connect makes the grpc dial to the proxy server. It returns the serverID
// it connects to.
func (a *AgentClient) Connect() (int, error) {
	conn, err := grpc.Dial(a.address, a.opts...)
	if err != nil {
		return 0, err
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), header.AgentID, a.agentID)
	if a.serviceAccountTokenPath != "" {
		if ctx, err = a.initializeAuthContext(ctx); err != nil {
			conn.Close()
			return 0, err
		}
	}
	stream, err := agent.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		conn.Close()
		return 0, err
	}
	serverID, err := serverID(stream)
	if err != nil {
		conn.Close()
		return 0, err
	}
	serverCount, err := serverCount(stream)
	if err != nil {
		conn.Close()
		return 0, err
	}
	a.conn = conn
	a.stream = stream
	a.serverID = serverID
	klog.Infof("Connect to server %s", serverID)
	return serverCount, nil
}

// Close closes the underlying connection.
func (a *AgentClient) Close() {
	if a.conn == nil {
		klog.Warning("Unexpected empty AgentClient.conn")
	}
	a.conn.Close()
	close(a.stopCh)
}

func (a *AgentClient) Send(pkt *client.Packet) error {
	a.sendLock.Lock()
	defer a.sendLock.Unlock()

	err := a.stream.Send(pkt)
	if err != nil && err != io.EOF {
		metrics.Metrics.ObserveFailure(metrics.DirectionToServer)
		a.cs.RemoveClient(a.serverID)
	}
	return err
}

func (a *AgentClient) Recv() (*client.Packet, error) {
	a.recvLock.Lock()
	defer a.recvLock.Unlock()

	pkt, err := a.stream.Recv()
	if err != nil && err != io.EOF {
		metrics.Metrics.ObserveFailure(metrics.DirectionFromServer)
		a.cs.RemoveClient(a.serverID)
	}
	return pkt, err
}

func serverCount(stream agent.AgentService_ConnectClient) (int, error) {
	md, err := stream.Header()
	if err != nil {
		return 0, err
	}
	scounts := md.Get(header.ServerCount)
	if len(scounts) != 1 {
		return 0, fmt.Errorf("expected one server count, got %d", len(scounts))
	}
	scount := scounts[0]
	return strconv.Atoi(scount)
}

func serverID(stream agent.AgentService_ConnectClient) (string, error) {
	// TODO: this is a blocking call. Add a timeout?
	md, err := stream.Header()
	if err != nil {
		return "", err
	}
	sids := md.Get(header.ServerID)
	if len(sids) != 1 {
		return "", fmt.Errorf("expected one server ID in the context, got %v", sids)
	}
	return sids[0], nil
}

func (a *AgentClient) initializeAuthContext(ctx context.Context) (context.Context, error) {
	var err error
	var b []byte

	// load current service account's token value
	if b, err = ioutil.ReadFile(a.serviceAccountTokenPath); err != nil {
		klog.Errorf("Failed to read token from %q. err: %v", a.serviceAccountTokenPath, err)
		return nil, err
	}
	ctx = metadata.AppendToOutgoingContext(ctx, header.AuthenticationTokenContextKey, header.AuthenticationTokenContextSchemePrefix+string(b))

	return ctx, nil
}

// Connect connects to proxy server to establish a gRPC stream,
// on which the proxied traffic is multiplexed through the stream
// and piped to the local connection. It register itself as a
// backend from proxy server, so proxy server will route traffic
// to this agent.
//
// The caller needs to call Serve to start serving proxy requests
// coming from proxy server.

// Serve starts to serve proxied requests from proxy server over the
// gRPC stream. Successful Connect is required before Serve. The
// The requests include things like opening a connection to a server,
// streaming data and close the connection.
func (a *AgentClient) Serve() {
	klog.Infof("Start serving for serverID %s", a.serverID)
	go a.probe()
	for {
		select {
		case <-a.stopCh:
			klog.Info("stop agent client.")
			return
		default:
		}

		pkt, err := a.Recv()
		if err != nil {
			if err == io.EOF {
				klog.Info("received EOF, exit")
				return
			}
			klog.Warningf("stream read error: %v", err)
			return
		}

		klog.Infof("[tracing] recv packet, type: %s", pkt.Type)

		if pkt == nil {
			klog.Warningf("empty packet received")
			continue
		}

		switch pkt.Type {
		case client.PacketType_DIAL_REQ:
			klog.Info("received DIAL_REQ")
			resp := &client.Packet{
				Type:    client.PacketType_DIAL_RSP,
				Payload: &client.Packet_DialResponse{DialResponse: &client.DialResponse{}},
			}

			dialReq := pkt.GetDialRequest()
			resp.GetDialResponse().Random = dialReq.Random

			conn, err := net.Dial(dialReq.Protocol, dialReq.Address)
			if err != nil {
				resp.GetDialResponse().Error = err.Error()
				if err := a.Send(resp); err != nil {
					klog.Warningf("stream send error: %v", err)
				}
				continue
			}

			connID := atomic.AddInt64(&a.nextConnID, 1)
			dataCh := make(chan []byte, 5)
			ctx := &connContext{
				conn:   conn,
				dataCh: dataCh,
				cleanFunc: func() {
					klog.Infof("close connection(id=%d)", connID)
					resp := &client.Packet{
						Type:    client.PacketType_CLOSE_RSP,
						Payload: &client.Packet_CloseResponse{CloseResponse: &client.CloseResponse{}},
					}
					resp.GetCloseResponse().ConnectID = connID

					err := conn.Close()
					if err != nil {
						resp.GetCloseResponse().Error = err.Error()
					}

					if err := a.Send(resp); err != nil {
						klog.Warningf("close response send error: %v", err)
					}

					close(dataCh)
					a.connManager.Delete(connID)
				},
			}
			a.connManager.Add(connID, ctx)

			resp.GetDialResponse().ConnectID = connID
			if err := a.Send(resp); err != nil {
				klog.Warningf("stream send error: %v", err)
				continue
			}

			go a.remoteToProxy(connID, ctx)
			go a.proxyToRemote(connID, ctx)

		case client.PacketType_DATA:
			data := pkt.GetData()
			klog.Infof("received DATA(id=%d)", data.ConnectID)

			ctx, ok := a.connManager.Get(data.ConnectID)
			if ok {
				ctx.dataCh <- data.Data
			}

		case client.PacketType_CLOSE_REQ:
			closeReq := pkt.GetCloseRequest()
			connID := closeReq.ConnectID

			klog.Infof("received CLOSE_REQ(id=%d)", connID)

			ctx, ok := a.connManager.Get(connID)
			if ok {
				ctx.cleanup()
			} else {
				resp := &client.Packet{
					Type:    client.PacketType_CLOSE_RSP,
					Payload: &client.Packet_CloseResponse{CloseResponse: &client.CloseResponse{}},
				}
				resp.GetCloseResponse().ConnectID = connID
				resp.GetCloseResponse().Error = "Unknown connectID"
				if err := a.Send(resp); err != nil {
					klog.Warningf("close response send error: %v", err)
					continue
				}
			}

		default:
			klog.Warningf("unrecognized packet type: %+v", pkt)
		}
	}
}

func (a *AgentClient) remoteToProxy(connID int64, ctx *connContext) {
	defer ctx.cleanup()

	var buf [1 << 12]byte
	resp := &client.Packet{
		Type: client.PacketType_DATA,
	}

	for {
		n, err := ctx.conn.Read(buf[:])
		klog.Infof("received %d bytes from remote for connID[%d]", n, connID)

		if err == io.EOF {
			klog.Info("connection EOF")
			return
		} else if err != nil {
			// Normal when receive a CLOSE_REQ
			klog.Warningf("connection read error: %v", err)
			return
		} else {
			resp.Payload = &client.Packet_Data{Data: &client.Data{
				Data:      buf[:n],
				ConnectID: connID,
			}}
			if err := a.Send(resp); err != nil {
				klog.Warningf("stream send error: %v", err)
			}
		}
	}
}

func (a *AgentClient) proxyToRemote(connID int64, ctx *connContext) {
	defer ctx.cleanup()

	for d := range ctx.dataCh {
		pos := 0
		for {
			n, err := ctx.conn.Write(d[pos:])
			if err == nil {
				klog.Infof("[connID: %d] write last %d data to remote", connID, n)
				break
			} else if n > 0 {
				klog.Infof("[connID: %d] write %d data to remote with error: %v", connID, n, err)
				pos += n
			} else {
				klog.Errorf("conn write error: %v", err)
				return
			}
		}
	}
}

func (a *AgentClient) probe() {
	for {
		select {
		case <-a.stopCh:
			return
		case <-time.After(a.probeInterval):
			if a.conn == nil {
				continue
			}
			// health check
			if a.conn.GetState() == connectivity.Ready {
				continue
			}
		}
		klog.Infof("Connection state %v, removing client used to connect to %v", a.conn.GetState(), a.serverID)
		a.cs.RemoveClient(a.serverID)
		return
	}
}
