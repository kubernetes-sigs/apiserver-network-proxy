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

package agentclient

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog"
)

// ClientSet consists of clients connected to each instance of an HA proxy server.
type ClientSet struct {
	mu      sync.Mutex              //protects the following
	clients map[string]*AgentClient // map between serverID and the client
	// connects to this server.

	agentID     string // ID of this agent
	address     string // proxy server address. Assuming HA proxy server
	serverCount int    // number of proxy server instances, should be 1
	// unless it is an HA server. Initialized when the ClientSet creates
	// the first client.
	syncInterval time.Duration // The interval by which the agent
	// periodically checks that it has connections to all instances of the
	// proxy server.
	dialOption grpc.DialOption
}

func (cs *ClientSet) clientsCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.clients)
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

func (cs *ClientSet) addClientLocked(serverID string, c *AgentClient) error {
	if cs.hasIDLocked(serverID) {
		return fmt.Errorf("client for proxy server %s already exists", serverID)
	}
	cs.clients[serverID] = c
	return nil

}

func (cs *ClientSet) AddClient(serverID string, c *AgentClient) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.addClientLocked(serverID, c)
}

func (cs *ClientSet) RemoveClient(serverID string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.clients, serverID)
}

func NewAgentClientSet(address, agentID string, syncInterval time.Duration, dialOption grpc.DialOption) *ClientSet {
	return &ClientSet{
		clients:      make(map[string]*AgentClient),
		agentID:      agentID,
		address:      address,
		syncInterval: syncInterval,
		dialOption:   dialOption,
	}
}

func (cs *ClientSet) newAgentClient() (*AgentClient, error) {
	return newAgentClient(cs.address, cs.agentID, cs, cs.dialOption)
}

// sync makes sure that #clients >= #proxy servers
func (cs *ClientSet) sync() {
	jitter := float64(0.2)
	for {
		if cs.serverCount != 0 {
			sleep := cs.syncInterval + time.Duration(rand.Float64()*jitter*float64(cs.syncInterval))
			time.Sleep(sleep)
		}
		if cs.serverCount == 0 || cs.clientsCount() < cs.serverCount {
			c, err := cs.newAgentClient()
			if err != nil {
				klog.Error(err)
				continue
			}
			cs.serverCount = c.serverCount
			if err := cs.AddClient(c.serverID, c); err != nil {
				klog.Infof("closing connection: %v", err)
				c.Close()
				continue
			}
			klog.Infof("sync added client connecting to proxy server %s", c.serverID)
			go c.Serve()
		}
	}
}

func (cs *ClientSet) Serve() {
	go cs.sync()
}
