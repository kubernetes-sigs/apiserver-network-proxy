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
)

const (
	fromResponses = "KNP server response headers"
	fromLeases = "KNP lease count"
	fromFallback = "fallback to 1"
)

// ClientSet consists of clients connected to each instance of an HA proxy server.
type ClientSet struct {
	mu      sync.Mutex         //protects the clients.
	clients map[string]*Client // map between serverID and the client
	// connects to this server.

	agentID string // ID of this agent
	address string // proxy server address. Assuming HA proxy server

	leaseCounter            ServerCounter // counts number of proxy server leases
	lastReceivedServerCount int                 // last server count received from a proxy server
	lastServerCount         int                 // last server count value from either lease system or proxy server, former takes priority

	// unless it is an HA server. Initialized when the ClientSet creates
	// the first client. When syncForever is set, it will be the most recently seen.
	syncInterval time.Duration // The interval by which the agent
	// periodically checks that it has connections to all instances of the
	// proxy server.
	probeInterval time.Duration // The interval by which the agent
	// periodically checks if its connections to the proxy server is ready.
	syncIntervalCap time.Duration // The maximum interval
	// for the syncInterval to back off to when unable to connect to the proxy server

	dialOptions []grpc.DialOption
	// file path contains service account token
	serviceAccountTokenPath string
	// channel to signal that the agent is pending termination.
	drainCh <-chan struct{}
	// channel to signal shutting down the client set. Primarily for test.
	stopCh <-chan struct{}

	agentIdentifiers string // The identifiers of the agent, which will be used
	// by the server when choosing agent

	warnOnChannelLimit bool
	xfrChannelSize     int

	syncForever bool // Continue syncing (support dynamic server count).
	serverCountSource string
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
	Address                 string
	AgentID                 string
	AgentIdentifiers        string
	SyncInterval            time.Duration
	ProbeInterval           time.Duration
	SyncIntervalCap         time.Duration
	DialOptions             []grpc.DialOption
	ServiceAccountTokenPath string
	WarnOnChannelLimit      bool
	SyncForever             bool
	XfrChannelSize          int
	ServerLeaseCounter      ServerCounter
	ServerCountSource       string
}

func (cc *ClientSetConfig) NewAgentClientSet(drainCh, stopCh <-chan struct{}) *ClientSet {
	return &ClientSet{
		clients:                 make(map[string]*Client),
		agentID:                 cc.AgentID,
		agentIdentifiers:        cc.AgentIdentifiers,
		address:                 cc.Address,
		syncInterval:            cc.SyncInterval,
		probeInterval:           cc.ProbeInterval,
		syncIntervalCap:         cc.SyncIntervalCap,
		dialOptions:             cc.DialOptions,
		serviceAccountTokenPath: cc.ServiceAccountTokenPath,
		warnOnChannelLimit:      cc.WarnOnChannelLimit,
		syncForever:             cc.SyncForever,
		drainCh:                 drainCh,
		xfrChannelSize:          cc.XfrChannelSize,
		stopCh:                  stopCh,
		leaseCounter:            cc.ServerLeaseCounter,
		serverCountSource:       cc.ServerCountSource,
	}
}

func (cs *ClientSet) newAgentClient() (*Client, int, error) {
	return newAgentClient(cs.address, cs.agentID, cs.agentIdentifiers, cs, cs.dialOptions...)
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

// sync makes sure that #clients >= #proxy servers
func (cs *ClientSet) sync() {
	defer cs.shutdown()
	backoff := cs.resetBackoff()
	var duration time.Duration
	for {
		if serverCount, err := cs.connectOnce(); err != nil {
			if dse, ok := err.(*DuplicateServerError); ok {
				clientsCount := cs.ClientsCount()
				klog.V(4).InfoS("duplicate server", "serverID", dse.ServerID, "serverCount", serverCount, "clientsCount", clientsCount)
				if serverCount != 0 && clientsCount >= serverCount {
					duration = backoff.Step()
				} else {
					backoff = cs.resetBackoff()
					duration = wait.Jitter(backoff.Duration, backoff.Jitter)
				}
			} else {
				klog.ErrorS(err, "cannot connect once")
				duration = backoff.Step()
			}
		} else {
			backoff = cs.resetBackoff()
			duration = wait.Jitter(backoff.Duration, backoff.Jitter)
		}
		time.Sleep(duration)
		select {
		case <-cs.stopCh:
			return
		default:
		}
	}
}

func (cs *ClientSet) ServerCount() int {

	var serverCount int
	var countSourceLabel string

	switch cs.serverCountSource {
	case "", "default":
		if cs.leaseCounter != nil {
			serverCount = cs.leaseCounter.Count()
			countSourceLabel = fromLeases
		} else {
			serverCount = cs.lastReceivedServerCount
			countSourceLabel = fromResponses
		}
	case "max":
		countFromLeases := 0
		if cs.leaseCounter != nil {
			countFromLeases = cs.leaseCounter.Count()
		}
		countFromResponses := cs.lastReceivedServerCount

		serverCount = countFromLeases
		countSourceLabel = fromLeases
		if countFromResponses > serverCount {
			serverCount = countFromResponses
			countSourceLabel = fromResponses
		}
		if serverCount == 0 {
			serverCount = 1
			countSourceLabel = fromFallback
		}

	}

	if serverCount != cs.lastServerCount {
		klog.Warningf("change detected in proxy server count (was: %d, now: %d, source: %q)", cs.lastServerCount, serverCount, countSourceLabel)
		cs.lastServerCount = serverCount
	}

	metrics.Metrics.SetServerCount(serverCount)
	return serverCount
}

func (cs *ClientSet) connectOnce() (int, error) {
	serverCount := cs.ServerCount()

	if !cs.syncForever && serverCount != 0 && cs.ClientsCount() >= serverCount {
		return serverCount, nil
	}
	c, receivedServerCount, err := cs.newAgentClient()
	if err != nil {
		return serverCount, err
	}
	cs.lastReceivedServerCount = receivedServerCount
	if err := cs.AddClient(c.serverID, c); err != nil {
		c.Close()
		return serverCount, err
	}
	klog.V(2).InfoS("sync added client connecting to proxy server", "serverID", c.serverID)

	labels := runpprof.Labels(
		"agentIdentifiers", cs.agentIdentifiers,
		"serverAddress", cs.address,
		"serverID", c.serverID,
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { c.Serve() })
	return serverCount, nil
}

func (cs *ClientSet) Serve() {
	labels := runpprof.Labels(
		"agentIdentifiers", cs.agentIdentifiers,
		"serverAddress", cs.address,
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { cs.sync() })
}

func (cs *ClientSet) shutdown() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for serverID, client := range cs.clients {
		client.Close()
		delete(cs.clients, serverID)
	}
}
