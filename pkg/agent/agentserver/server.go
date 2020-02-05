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
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/header"
)

// ProxyClientConnection...
type ProxyClientConnection struct {
	Mode      string
	Grpc      agent.ProxyService_ProxyServer
	HTTP      net.Conn
	connected chan struct{}
	connectID int64
}

func (c *ProxyClientConnection) send(pkt *agent.Packet) error {
	if c.Mode == "grpc" {
		stream := c.Grpc
		return stream.Send(pkt)
	} else if c.Mode == "http-connect" {
		if pkt.Type == agent.PacketType_CLOSE_RSP {
			return c.HTTP.Close()
		} else if pkt.Type == agent.PacketType_DATA {
			_, err := c.HTTP.Write(pkt.GetData().Data)
			return err
		} else if pkt.Type == agent.PacketType_DIAL_RSP {
			return nil
		} else {
			return fmt.Errorf("attempt to send via unrecognized connection type %v", pkt.Type)
		}
	} else {
		return fmt.Errorf("attempt to send via unrecognized connection mode %q", c.Mode)
	}
}

// ProxyServer
type ProxyServer struct {
	mu sync.RWMutex //protects the following
	// A map between agentID and its grpc connections.
	// For a given agent, ProxyServer prefers backends[agentID][0] to send
	// traffic, because backends[agentID][1:] are more likely to be closed
	// by the agent to deduplicate connections to the same server.
	backends map[string][]agent.AgentService_ConnectServer
	agentIDs []string
	random   *rand.Rand

	// connID track
	Frontends   map[int64]*ProxyClientConnection
	PendingDial map[int64]*ProxyClientConnection

	serverID    string // unique ID of this server
	serverCount int    // Number of proxy server instances, should be 1 unless it is a HA server.

	// agent authentication
	AgentAuthenticationOptions *AgentTokenAuthenticationOptions
}

// AgentTokenAuthenticationOptions contains list of parameters required for agent token based authentication
type AgentTokenAuthenticationOptions struct {
	Enabled                bool
	AgentNamespace         string
	AgentServiceAccount    string
	AuthenticationAudience string
	KubernetesClient       kubernetes.Interface
}

var _ agent.AgentServiceServer = &ProxyServer{}

var _ agent.ProxyServiceServer = &ProxyServer{}

func (s *ProxyServer) addBackend(agentID string, conn agent.AgentService_ConnectServer) {
	klog.Infof("register Backend %v for agentID %s", conn, agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backends[agentID]
	if ok {
		s.backends[agentID] = append(s.backends[agentID], conn)
		return
	}
	s.backends[agentID] = []agent.AgentService_ConnectServer{conn}
	s.agentIDs = append(s.agentIDs, agentID)
}

func (s *ProxyServer) removeBackend(agentID string, conn agent.AgentService_ConnectServer) {
	klog.Infof("remove Backend %v for agentID %s", conn, agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	backends, ok := s.backends[agentID]
	if !ok {
		klog.Warningf("can't find agentID %s in the backends", agentID)
		return
	}
	var found bool
	for i, c := range backends {
		if c == conn {
			s.backends[agentID] = append(s.backends[agentID][:i], s.backends[agentID][i+1:]...)
			found = true
		}
	}
	if len(s.backends[agentID]) == 0 {
		delete(s.backends, agentID)
		for i := range s.agentIDs {
			if s.agentIDs[i] == agentID {
				s.agentIDs[i] = s.agentIDs[len(s.agentIDs)-1]
				s.agentIDs = s.agentIDs[:len(s.agentIDs)-1]
				break
			}
		}
	}
	if !found {
		klog.Warningf("can't find conn %v for agentID %s in the backends", conn, agentID)
	}
}

func (s *ProxyServer) randomBackend() (agent.AgentService_ConnectServer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.backends) == 0 {
		return nil, fmt.Errorf("No backend available")
	}
	agentID := s.agentIDs[s.random.Intn(len(s.agentIDs))]
	return s.backends[agentID][0], nil
}

// NewProxyServer creates a new ProxyServer instance
func NewProxyServer(serverID string, serverCount int, agentAuthenticationOptions *AgentTokenAuthenticationOptions) *ProxyServer {
	return &ProxyServer{
		Frontends:                  make(map[int64]*ProxyClientConnection),
		PendingDial:                make(map[int64]*ProxyClientConnection),
		serverID:                   serverID,
		serverCount:                serverCount,
		backends:                   make(map[string][]agent.AgentService_ConnectServer),
		random:                     rand.New(rand.NewSource(time.Now().UTC().UnixNano())),
		AgentAuthenticationOptions: agentAuthenticationOptions,
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

	// Start goroutine to receive packets from frontend and push to recvCh
	go func() {
		for {
			in, err := stream.Recv()
			if err == io.EOF {
				close(stopCh)
				return
			}
			if err != nil {
				klog.Warningf(">>> Stream read from frontend error: %v", err)
				close(stopCh)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

func (s *ProxyServer) serveRecvFrontend(stream agent.ProxyService_ProxyServer, recvCh <-chan *agent.Packet) {
	klog.Info("start serving frontend stream")

	var firstConnID int64
	// The first packet should be a DIAL_REQ, we will randomly get a
	// backend from s.backends then.
	var backend agent.AgentService_ConnectServer
	var err error

	for pkt := range recvCh {
		switch pkt.Type {
		case agent.PacketType_DIAL_REQ:
			klog.Info(">>> Received DIAL_REQ")
			// TODO: if we track what agent has historically served
			// the address, then we can send the Dial_REQ to the
			// same agent. That way we save the agent from creating
			// a new connection to the address.
			backend, err = s.randomBackend()
			if err != nil {
				klog.Errorf(">>> failed to get a backend: %v", err)
				continue
			}
			if err := backend.Send(pkt); err != nil {
				klog.Warningf(">>> DIAL_REQ to Backend failed: %v", err)
			}
			s.PendingDial[pkt.GetDialRequest().Random] = &ProxyClientConnection{
				Mode:      "grpc",
				Grpc:      stream,
				connected: make(chan struct{}),
			}
			klog.Info(">>> DIAL_REQ sent to backend") // got this. but backend didn't receive anything.

		case agent.PacketType_CLOSE_REQ:
			connID := pkt.GetCloseRequest().ConnectID
			klog.Infof(">>> Received CLOSE_REQ(id=%d)", connID)
			if backend == nil {
				klog.Errorf("backend has not been initialized for connID %d. Client should send a Dial Request first.", connID)
				continue
			}
			if err := backend.Send(pkt); err != nil {
				// TODO: retry with other backends connecting to this agent.
				klog.Warningf(">>> CLOSE_REQ to Backend failed: %v", err)
			}
			klog.Info(">>> CLOSE_REQ sent to backend")

		case agent.PacketType_DATA:
			connID := pkt.GetData().ConnectID
			klog.Infof(">>> Received DATA(id=%d)", connID)
			if firstConnID == 0 {
				firstConnID = connID
			} else if firstConnID != connID {
				klog.Warningf(">>> Data(id=%d) doesn't match first connection id %d", firstConnID, connID)
			}

			if backend == nil {
				klog.Errorf("backend has not been initialized for connID %d. Client should send a Dial Request first.", connID)
				continue
			}
			if err := backend.Send(pkt); err != nil {
				// TODO: retry with other backends connecting to this agent.
				klog.Warningf(">>> DATA to Backend failed: %v", err)
			}
			klog.Info(">>> DATA sent to backend")

		default:
			klog.Infof(">>> Ignore %v packet coming from frontend", pkt.Type)
		}
	}

	klog.Infof(">>> Close streaming (id=%d)", firstConnID)

	pkt := &agent.Packet{
		Type: agent.PacketType_CLOSE_REQ,
		Payload: &agent.Packet_CloseRequest{
			CloseRequest: &agent.CloseRequest{
				ConnectID: firstConnID,
			},
		},
	}

	if backend == nil {
		klog.Errorf("backend has not been initialized for connID %d. Client should send a Dial Request first.", firstConnID)
		return
	}
	if err := backend.Send(pkt); err != nil {
		klog.Warningf(">>> CLOSE_REQ to Backend failed: %v", err)
	}
}

func (s *ProxyServer) serveSend(stream agent.ProxyService_ProxyServer, sendCh <-chan *agent.Packet) {
	klog.Info("start serve send ...")
	for pkt := range sendCh {
		err := stream.Send(pkt)
		if err != nil {
			klog.Warningf("stream write error: %v", err)
		}
	}
}

func agentID(stream agent.AgentService_ConnectServer) (string, error) {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		return "", fmt.Errorf("failed to get context")
	}
	agentIDs := md.Get(header.AgentID)
	if len(agentIDs) != 1 {
		return "", fmt.Errorf("expected one agent ID in the context, got %v", agentIDs)
	}
	return agentIDs[0], nil
}

func (s *ProxyServer) validateAuthToken(token string) error {
	trReq := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{s.AgentAuthenticationOptions.AuthenticationAudience},
		},
	}
	r, err := s.AgentAuthenticationOptions.KubernetesClient.AuthenticationV1().TokenReviews().Create(trReq)
	if err != nil {
		return fmt.Errorf("Failed to authenticate request. err:%v", err)
	}

	if r.Status.Error != "" {
		return fmt.Errorf("lookup failed: %s", r.Status.Error)
	}

	if !r.Status.Authenticated {
		return fmt.Errorf("lookup failed: service account jwt not valid")
	}

	// The username is of format: system:serviceaccount:(NAMESPACE):(SERVICEACCOUNT)
	parts := strings.Split(r.Status.User.Username, ":")
	if len(parts) != 4 {
		return fmt.Errorf("lookup failed: unexpected username format")
	}
	// Validate the user that comes back from token review is a service account
	if parts[0] != "system" || parts[1] != "serviceaccount" {
		return fmt.Errorf("lookup failed: username returned is not a service account")
	}

	ns := parts[2]
	sa := parts[3]
	if s.AgentAuthenticationOptions.AgentNamespace != ns {
		return fmt.Errorf("lookup failed: incoming request from %q namespace. Expected %q", ns, s.AgentAuthenticationOptions.AgentNamespace)
	}

	if s.AgentAuthenticationOptions.AgentServiceAccount != sa {
		return fmt.Errorf("lookup failed: incoming request from %q service account. Expected %q", sa, s.AgentAuthenticationOptions.AgentServiceAccount)
	}

	return nil
}

func (s *ProxyServer) authenticateAgentViaToken(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return fmt.Errorf("Failed to retrieve metadata from context")
	}

	authContext := md.Get(header.AuthenticationTokenContextKey)
	if len(authContext) == 0 {
		return fmt.Errorf("Authentication context was not found in metadata")
	}

	if len(authContext) > 1 {
		return fmt.Errorf("too many (%d) tokens are received", len(authContext))
	}

	if !strings.HasPrefix(authContext[0], header.AuthenticationTokenContextSchemePrefix) {
		return fmt.Errorf("received token does not have %q prefix", header.AuthenticationTokenContextSchemePrefix)
	}

	if err := s.validateAuthToken(strings.TrimPrefix(authContext[0], header.AuthenticationTokenContextSchemePrefix)); err != nil {
		return fmt.Errorf("Failed to validate authentication token, err:%v", err)
	}

	klog.Infof("Client successfully authenticated via token")
	return nil
}

// Connect is for agent to connect to ProxyServer as next hop
func (s *ProxyServer) Connect(stream agent.AgentService_ConnectServer) error {
	agentID, err := agentID(stream)
	if err != nil {
		return err
	}
	klog.Infof("Connect request from agent %s", agentID)
	s.addBackend(agentID, stream)
	defer s.removeBackend(agentID, stream)

	h := metadata.Pairs(header.ServerID, s.serverID, header.ServerCount, strconv.Itoa(s.serverCount))
	if err := stream.SendHeader(h); err != nil {
		return err
	}

	recvCh := make(chan *agent.Packet, 10)
	stopCh := make(chan error)

	if s.AgentAuthenticationOptions.Enabled {
		if err := s.authenticateAgentViaToken(stream.Context()); err != nil {
			klog.Infof("Client authentication failed. err:%v", err)
			return err
		}
	}

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
				close(stopCh)
				return
			}

			recvCh <- in
		}
	}()

	return <-stopCh
}

// route the packet back to the correct client
func (s *ProxyServer) serveRecvBackend(stream agent.AgentService_ConnectServer, recvCh <-chan *agent.Packet) {
	var firstConnID int64

	for pkt := range recvCh {
		switch pkt.Type {
		case agent.PacketType_DIAL_RSP:
			resp := pkt.GetDialResponse()
			firstConnID = resp.ConnectID
			klog.Infof("<<< Received DIAL_RSP(rand=%d, id=%d)", resp.Random, resp.ConnectID)

			if client, ok := s.PendingDial[resp.Random]; !ok {
				klog.Warning("<<< DialResp not recognized; dropped")
			} else {
				err := client.send(pkt)
				delete(s.PendingDial, resp.Random)
				if err != nil {
					klog.Warningf("<<< DIAL_RSP send to client stream error: %v", err)
				} else {
					client.connectID = resp.ConnectID
					s.Frontends[resp.ConnectID] = client
					close(client.connected)
				}
			}

		case agent.PacketType_DATA:
			resp := pkt.GetData()
			klog.Infof("<<< Received DATA(id=%d)", resp.ConnectID)
			if client, ok := s.Frontends[resp.ConnectID]; ok {
				if err := client.send(pkt); err != nil {
					klog.Warningf("<<< DATA send to client stream error: %v", err)
				} else {
					klog.Infof("<<< DATA sent to frontend")
				}
			}

		case agent.PacketType_CLOSE_RSP:
			resp := pkt.GetCloseResponse()
			klog.Infof("<<< Received CLOSE_RSP(id=%d)", resp.ConnectID)
			if client, ok := s.Frontends[resp.ConnectID]; ok {
				if err := client.send(pkt); err != nil {
					// Normal when frontend closes it.
					klog.Warningf("<<< CLOSE_RSP send to client stream error: %v", err)
				} else {
					klog.Infof("<<< CLOSE_RSP sent to frontend")
				}
			}

		default:
			klog.Warningf("<<< Unrecognized packet %+v", pkt)
		}
	}

	klog.Infof("<<< Close streaming (id=%d)", firstConnID)
}
