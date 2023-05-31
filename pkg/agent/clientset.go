/*
Copyright 2020 The Kubernetes Authors.

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
	"math"
	runpprof "runtime/pprof"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"crypto/tls"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
	"net"
)

const HAConnectionManager = "HA-LB"
const IPListConnectionManager = "IPList"

// type proxyConnectionManager interface { controls connections to the APIServer Network Proxy Server.
// It isolates logic such as the HA Load Balancer case vs know list of Proxy Servers.
type proxyConnectionManager interface {
	ensureConnectivity() error
	setClientSet(*ClientSet)
}

// haProxyConnectionManager makes sure that #clients >= #proxy servers
type haProxyConnectionManager struct {
	clientSet *ClientSet

	KeepaliveTime time.Duration
	dialOptions   []grpc.DialOption

	agentID     string // ID of this agent
	address     string // proxy server address. Assuming HA proxy server
	serverCount int    // number of proxy server instances, should be

	CaCert     string
	AgentCert  string
	AgentKey   string
	AlpnProtos []string

	syncForever bool // Continue syncing (support dynamic server count).
}

func (m *haProxyConnectionManager) setClientSet(cs *ClientSet) {
	m.clientSet = cs
}

// sync makes sure that #clients >= #proxy servers
func (m *haProxyConnectionManager) ensureConnectivity() error {
	defer m.clientSet.shutdown()
	backoff := m.clientSet.resetBackoff()
	var duration time.Duration

	var tlsConfig *tls.Config

	host, _, err := net.SplitHostPort(m.address)
	if err != nil {
		return err
	}
	if tlsConfig, err = util.GetClientTLSConfig(m.CaCert, m.AgentCert, m.AgentKey, host, m.AlpnProtos); err != nil {
		return err
	}
	m.dialOptions = []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                m.KeepaliveTime,
			PermitWithoutStream: true,
		}),
	}

	for {
		if err := m.connectOnce(); err != nil {
			if dse, ok := err.(*DuplicateServerError); ok {
				klog.V(4).InfoS("duplicate server", "serverID", dse.ServerID, "serverCount", m.serverCount, "clientsCount", m.clientSet.ClientsCount())
				if m.serverCount != 0 && m.clientSet.ClientsCount() >= m.serverCount {
					duration = backoff.Step()
				}
			} else {
				klog.ErrorS(err, "cannot connect once")
				duration = backoff.Step()
			}
		} else {
			backoff = m.clientSet.resetBackoff()
			duration = wait.Jitter(backoff.Duration, backoff.Jitter)
		}
		time.Sleep(duration)
		select {
		case <-m.clientSet.stopCh:
			return nil
		default:
		}
	}
	return nil
}

func (m *haProxyConnectionManager) connectOnce() error {
	if !m.syncForever && m.serverCount != 0 && m.clientSet.ClientsCount() >= m.serverCount {
		return nil
	}
	c, serverCount, err := m.clientSet.newAgentClient(m.address, m.agentID, m.dialOptions)
	if err != nil {
		return err
	}
	if m.serverCount != 0 && m.serverCount != serverCount {
		klog.V(2).InfoS("Server count change suggestion by server",
			"current", m.serverCount, "serverID", c.serverID, "actual", serverCount)

	}
	m.serverCount = serverCount
	if err := m.clientSet.AddClient(c.serverID, c); err != nil {
		c.Close()
		return err
	}
	klog.V(2).InfoS("sync added client connecting to proxy server", "serverID", c.serverID)

	labels := runpprof.Labels(
		"agentIdentifiers", m.clientSet.agentIdentifiers,
		"serverAddress", m.address,
		"serverID", c.serverID,
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { c.Serve() })
	return nil
}

// listProxyConnectionManager makes sure that #clients >= #proxy servers
type listProxyConnectionManager struct {
	clientSet *ClientSet

	KeepaliveTime time.Duration

	agentID     string   // ID of this agent
	addresses   []string // proxy server addresses.
	dialOptions [][]grpc.DialOption

	CaCert     string
	AgentCert  string
	AgentKey   string
	AlpnProtos []string
}

func (m *listProxyConnectionManager) ensureConnectivity() error {
	defer m.clientSet.shutdown()
	//backoff := m.clientSet.resetBackoff()

	m.dialOptions = make([][]grpc.DialOption, len(m.addresses))
	for index, address := range m.addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		tlsConfig, err := util.GetClientTLSConfig(m.CaCert, m.AgentCert, m.AgentKey, host, m.AlpnProtos)
		if err != nil {
			return err
		}
		m.dialOptions[index] = []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                m.KeepaliveTime,
				PermitWithoutStream: true,
			}),
		}
	}
	for index, address := range m.addresses {
		c, _, err := m.clientSet.newAgentClient(address, m.agentID, m.dialOptions[index])
		if err != nil {
			return err
		}
		if err := m.clientSet.AddClient(c.serverID, c); err != nil {
			c.Close()
			return err
		}
		klog.V(2).InfoS("sync added client connecting to proxy server", "serverID", c.serverID)
		labels := runpprof.Labels(
			"agentIdentifiers", m.clientSet.agentIdentifiers,
			"serverAddress", address,
			"serverID", c.serverID,
		)
		go runpprof.Do(context.Background(), labels, func(context.Context) { c.Serve() })
	}
	// TODO: Ensure clients remain valid
	return nil
}

func (m *listProxyConnectionManager) setClientSet(cs *ClientSet) {
	m.clientSet = cs
}

// ClientSet consists of clients connected to each instance of an HA proxy server.
type ClientSet struct {
	mu      sync.Mutex         //protects the clients.
	clients map[string]*Client // map between serverID and the client

	// connects to this server.
	//agentID     string // ID of this agent
	//address     string // proxy server address. Assuming HA proxy server
	//serverCount int    // number of proxy server instances, should be 1
	proxyConnectionManager proxyConnectionManager

	// unless it is an HA server. Initialized when the ClientSet creates
	// the first client. When syncForever is set, it will be the most recently seen.
	syncInterval time.Duration // The interval by which the agent
	// periodically checks that it has connections to all instances of the
	// proxy server.
	probeInterval time.Duration // The interval by which the agent
	// periodically checks if its connections to the proxy server is ready.
	syncIntervalCap time.Duration // The maximum interval
	// for the syncInterval to back off to when unable to connect to the proxy server

	//dialOptions []grpc.DialOption
	// file path contains service account token
	serviceAccountTokenPath string
	// channel to signal shutting down the client set. Primarily for test.
	stopCh <-chan struct{}

	agentIdentifiers string // The identifiers of the agent, which will be used
	// by the server when choosing agent

	warnOnChannelLimit bool
}

func (cs *ClientSet) ClientsCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.clients)
}

func (cs *ClientSet) HealthyClientsCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var count int
	for _, c := range cs.clients {
		if c.conn.GetState() == connectivity.Ready {
			count++
		}
	}
	return count

}

func (cs *ClientSet) hasIDLocked(serverID string) bool {
	_, ok := cs.clients[serverID]
	return ok
}

func (cs *ClientSet) HasID(serverID string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.hasIDLocked(serverID)
}

type DuplicateServerError struct {
	ServerID string
}

func (dse *DuplicateServerError) Error() string {
	return "duplicate server: " + dse.ServerID
}

func (cs *ClientSet) addClientLocked(serverID string, c *Client) error {
	if cs.hasIDLocked(serverID) {
		return &DuplicateServerError{ServerID: serverID}
	}
	cs.clients[serverID] = c
	metrics.Metrics.SetServerConnectionsCount(len(cs.clients))
	return nil

}

func (cs *ClientSet) AddClient(serverID string, c *Client) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.addClientLocked(serverID, c)
}

func (cs *ClientSet) RemoveClient(serverID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.clients[serverID] == nil {
		return
	}
	cs.clients[serverID].Close()
	delete(cs.clients, serverID)
	metrics.Metrics.SetServerConnectionsCount(len(cs.clients))
}

type ClientSetConfig struct {
	ConnectionManager       string
	Address                 string
	Addresses               []string
	AgentID                 string
	AgentIdentifiers        string
	CaCert                  string
	AgentCert               string
	AgentKey                string
	AlpnProtos              []string
	SyncInterval            time.Duration
	ProbeInterval           time.Duration
	SyncIntervalCap         time.Duration
	KeepaliveTime           time.Duration
	DialOptions             []grpc.DialOption
	ServiceAccountTokenPath string
	WarnOnChannelLimit      bool
	SyncForever             bool
}

func (cc *ClientSetConfig) NewAgentClientSet(stopCh <-chan struct{}) *ClientSet {
	var proxyConnectionManager proxyConnectionManager
	if cc.ConnectionManager == HAConnectionManager {
		proxyConnectionManager = &haProxyConnectionManager{
			KeepaliveTime: cc.KeepaliveTime,
			agentID:       cc.AgentID,
			address:       cc.Address,
			syncForever:   cc.SyncForever,
			CaCert:        cc.CaCert,
			AgentCert:     cc.AgentCert,
			AgentKey:      cc.AgentKey,
			AlpnProtos:    cc.AlpnProtos,
		}
	} else if cc.ConnectionManager == IPListConnectionManager {
		proxyConnectionManager = &listProxyConnectionManager{
			KeepaliveTime: cc.KeepaliveTime,

			agentID:   cc.AgentID,
			addresses: cc.Addresses,

			CaCert:     cc.CaCert,
			AgentCert:  cc.AgentCert,
			AgentKey:   cc.AgentKey,
			AlpnProtos: cc.AlpnProtos,
		}
	}

	cs := &ClientSet{
		clients:                make(map[string]*Client),
		proxyConnectionManager: proxyConnectionManager,
		agentIdentifiers:       cc.AgentIdentifiers,
		syncInterval:           cc.SyncInterval,
		probeInterval:          cc.ProbeInterval,
		syncIntervalCap:        cc.SyncIntervalCap,
		//dialOptions:             cc.DialOptions,
		serviceAccountTokenPath: cc.ServiceAccountTokenPath,
		warnOnChannelLimit:      cc.WarnOnChannelLimit,
		stopCh:                  stopCh,
	}
	proxyConnectionManager.setClientSet(cs)
	return cs
}

func (cs *ClientSet) newAgentClient(address string, agentID string, dialOptions []grpc.DialOption) (*Client, int, error) {
	return newAgentClient(address, agentID, cs.agentIdentifiers, cs, dialOptions...)
}

func (cs *ClientSet) resetBackoff() *wait.Backoff {
	return &wait.Backoff{
		Steps:    math.MaxInt32,
		Jitter:   0.1,
		Factor:   1.5,
		Duration: cs.syncInterval,
		Cap:      cs.syncIntervalCap,
	}
}

func (cs *ClientSet) Serve() {
	labels := runpprof.Labels(
		"agentIdentifiers", cs.agentIdentifiers,
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { cs.proxyConnectionManager.ensureConnectivity() })
}

func (cs *ClientSet) shutdown() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for serverID, client := range cs.clients {
		client.Close()
		delete(cs.clients, serverID)
	}
}
