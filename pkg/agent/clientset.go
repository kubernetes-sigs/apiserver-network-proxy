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
	fromLeases    = "KNP lease count"
	fromFallback  = "fallback to 1"
)

// ClientSet consists of clients connected to each instance of an HA proxy server.
type ClientSet struct {
	// mu guards access to the clients map
	mu sync.Mutex

	// clients is a map between serverID and the client
	// connected to this server.
	clients map[string]*Client

	// agentID is "our ID" - the ID of this agent.
	agentID string

	// Address is the proxy server address.  Assuming HA proxy server
	address string

	// serverCounter counts number of proxy server leases
	serverCounter ServerCounter

	// lastReceivedServerCount is the last serverCount value received when connecting to a proxy server
	lastReceivedServerCount int

	// syncInterval is the interval at which the agent periodically checks
	// that it has connections to all instances of the proxy server.
	syncInterval time.Duration

	//  The maximum interval for the syncInterval to back off to when unable to connect to the proxy server
	syncIntervalCap time.Duration

	// 	syncForever is true if we should continue syncing (support dynamic server count).
	syncForever bool

	// probeInterval is the interval at which the agent
	// periodically checks if its connections to the proxy server is ready.
	probeInterval time.Duration

	dialOptions []grpc.DialOption

	// serviceAccountTokenPath is the file path to our kubernetes service account token
	serviceAccountTokenPath string

	// channel to signal that the agent is pending termination.
	drainCh <-chan struct{}

	// channel to signal shutting down the client set. Primarily for test.
	stopCh <-chan struct{}

	// agentIdentifiers is the identifiers of the agent, which will be used
	// by the server when choosing agent
	agentIdentifiers string

	warnOnChannelLimit bool
	xfrChannelSize     int

	// serverCountSource controls how we compute the server count.
	// The proxy server sends the serverCount header to each connecting agent,
	// and the agent figures out from these observations how many
	// agent-to-proxy-server connections it should maintain.
	serverCountSource string
}

func (cs *ClientSet) ClientsCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.clients)
}

// SetServerCounter sets the strategy for determining the server count.
func (cs *ClientSet) SetServerCounter(counter ServerCounter) {
	cs.serverCounter = counter
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

// HasID returns true if the ClientSet has a client to the specified serverID.
func (cs *ClientSet) HasID(serverID string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	_, exists := cs.clients[serverID]
	return exists
}

type DuplicateServerError struct {
	ServerID string
}

func (dse *DuplicateServerError) Error() string {
	return "duplicate server: " + dse.ServerID
}

// AddClient adds the specified client to our set of clients.
// If we already have a connection with the same serverID, we will return *DuplicateServerError.
func (cs *ClientSet) AddClient(serverID string, c *Client) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	_, exists := cs.clients[serverID]
	if exists {
		return &DuplicateServerError{ServerID: serverID}
	}
	cs.clients[serverID] = c
	metrics.Metrics.SetServerConnectionsCount(len(cs.clients))
	return nil
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

// determineServerCount determines the number of proxy servers by delegating to its configured counter strategy.
func (cs *ClientSet) determineServerCount() int {
	serverCount := cs.serverCounter.Count()
	metrics.Metrics.SetServerCount(serverCount)
	return serverCount
}

// sync manages the backoff and the connection attempts to the proxy server.
// sync runs until stopCh is closed
func (cs *ClientSet) sync() {
	defer cs.shutdown()
	backoff := cs.resetBackoff()
	var duration time.Duration
	for {
		if err := cs.connectOnce(); err != nil {
			if dse, ok := err.(*DuplicateServerError); ok {
				klog.V(4).InfoS("duplicate server connection attempt", "serverID", dse.ServerID)
				// We connected to a server we already have a connection to.
				// This is expected in syncForever mode. We just wait for the
				// next sync period to try again. No need for backoff.
				backoff = cs.resetBackoff()
				duration = wait.Jitter(backoff.Duration, backoff.Jitter)
			} else {
				// A 'real' error, so we backoff.
				klog.ErrorS(err, "cannot connect once")
				duration = backoff.Step()
			}
		} else {
			// A successful connection was made, or no new connection was needed.
			// Reset the backoff and wait for the next sync period.
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

func (cs *ClientSet) connectOnce() error {
	serverCount := cs.determineServerCount()

	// If not in syncForever mode, we only connect if we have fewer connections than the server count.
	if !cs.syncForever && cs.ClientsCount() >= serverCount && serverCount > 0 {
		return nil // Nothing to do.
	}

	// In syncForever mode, we always try to connect, to discover new servers.
	c, receivedServerCount, err := cs.newAgentClient()
	if err != nil {
		return err
	}

	if err := cs.AddClient(c.serverID, c); err != nil {
		c.Close()
		return err // likely *DuplicateServerError
	}
	// SUCCESS: We connected to a new, unique server.
	// Only now do we update our view of the server count.
	cs.lastReceivedServerCount = receivedServerCount
	klog.V(2).InfoS("successfully connected to new proxy server", "serverID", c.serverID, "newServerCount", receivedServerCount)

	labels := runpprof.Labels(
		"agentIdentifiers", cs.agentIdentifiers,
		"serverAddress", cs.address,
		"serverID", c.serverID,
	)
	go runpprof.Do(context.Background(), labels, func(context.Context) { c.Serve() })

	return nil
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
