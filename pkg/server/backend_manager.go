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

package server

import (
	"context"
	"io"
	"math/rand"
	"sync"
	"time"

	"k8s.io/klog/v2"
	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
)

var BOT time.Time = time.Unix(0, 0)

type Backend interface {
	Send(p *client.Packet) error
	Context() context.Context
}

var _ Backend = &backend{}
var _ Backend = agent.AgentService_ConnectServer(nil)

type backend struct {
	// TODO: this is a multi-writer single-reader pattern, it's tricky to
	// write it using channel. Let's worry about performance later.
	mu   sync.Mutex // mu protects conn
	conn agent.AgentService_ConnectServer
	taintExpires time.Time
	agentID string
}

func (b *backend) Send(p *client.Packet) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conn.Send(p)
}

func (b *backend) Context() context.Context {
	// TODO: does Context require lock protection?
	return b.conn.Context()
}

func (b *backend) GetAgentID() string {
	return b.agentID
}

func newBackend(conn agent.AgentService_ConnectServer, agentID string) *backend {
	return &backend{conn: conn, taintExpires: BOT, agentID: agentID}
}

// BackendStorage is an interface to manage the storage of the backend
// connections, i.e., get, add and remove
type BackendStorage interface {
	// AddBackend adds a backend.
	AddBackend(agentID string, conn agent.AgentService_ConnectServer) Backend
	// TaintBackend indicates an error occurred on a backend and allows to BackendManager to act based on the error.
	TaintBackend(agentID string, err error)
	// RemoveBackend removes a backend.
	RemoveBackend(agentID string, conn agent.AgentService_ConnectServer)
	// NumBackends returns the number of backends.
	NumBackends() int
}

// BackendManager is an interface to manage backend connections, i.e.,
// connection to the proxy agents.
type BackendManager interface {
	// Backend returns a single backend.
	// WARNING: the context passed to the function should be a session-scoped
	// context instead of a request-scoped context, as the backend manager will
	// pick a backend for every tunnel session and each tunnel session may
	// contains multiple requests.
	Backend(ctx context.Context) (Backend, string, error)
	BackendStorage
}

var _ BackendManager = &DefaultBackendManager{}

// DefaultBackendManager is the default backend manager.
type DefaultBackendManager struct {
	*DefaultBackendStorage
}

func (dbm *DefaultBackendManager) Backend(_ context.Context) (Backend, string, error) {
	return dbm.DefaultBackendStorage.GetRandomBackend()
}

// DefaultBackendStorage is the default backend storage.
type DefaultBackendStorage struct {
	mu sync.RWMutex //protects the following
	// A map between agentID and its grpc connections.
	// For a given agent, ProxyServer prefers backends[agentID][0] to send
	// traffic, because backends[agentID][1:] are more likely to be closed
	// by the agent to deduplicate connections to the same server.
	backends map[string][]*backend
	// agentIDs is a slice of agentIDs to enable randomly picking one
	// in the Backend() method. There is no reliable way to randomly
	// pick a key from a map (in this case, the backends) in Golang.
	agentIDs []string
	// untaintedIDs is a slice of agentIDs which are not currently
	// marked as tainted to enable randomly picking a known good ID
	// in the Backend() method. There is no reliable way to randomly
	// pick a key from a map (in this case, the backends) in Golang.
	// Consider switching this to a map[string]bool to get set behavior
	// A little nasty as length where true is yuck.
	untaintedIDs []string
	// untaintedIDsmu protects untaintedIDs. To prevent deadlocks,
	// we use order locking. You should be already holding mu, prior
	// to obtaining untaintedIDsmu.
	untaintedIDsmu sync.RWMutex
	random   *rand.Rand
	keepaliveTime time.Duration
}

// NewDefaultBackendManager returns a DefaultBackendManager.
func NewDefaultBackendManager(keepaliveTime time.Duration) *DefaultBackendManager {
	return &DefaultBackendManager{DefaultBackendStorage: NewDefaultBackendStorage(keepaliveTime)}
}

// NewDefaultBackendStorage returns a DefaultBackendStorage
func NewDefaultBackendStorage(keepaliveTime time.Duration) *DefaultBackendStorage {
	return &DefaultBackendStorage{
		backends: 	   make(map[string][]*backend),
		random:        rand.New(rand.NewSource(time.Now().UnixNano())),
		keepaliveTime: keepaliveTime,
	}
}

// AddBackend adds a backend.
func (s *DefaultBackendStorage) AddBackend(agentID string, conn agent.AgentService_ConnectServer) Backend {
	klog.V(2).InfoS("Register backend for agent", "connection", conn, "agentID", agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backends[agentID]
	addedBackend := newBackend(conn, agentID)
	if ok {
		for _, v := range s.backends[agentID] {
			if v.conn == conn {
				klog.V(1).InfoS("This should not happen. Adding existing backend for agent", "connection", conn, "agentID", agentID)
				return v
			}
		}
		s.backends[agentID] = append(s.backends[agentID], addedBackend)
		return addedBackend
	}
	s.backends[agentID] = []*backend{addedBackend}
	s.agentIDs = append(s.agentIDs, agentID)
	s.appendUntaintedSafe(agentID)
	return addedBackend
}

// TaintBackend will find the matching tunnel and taint it for the configured duration.
func (s *DefaultBackendStorage)TaintBackend(agentID string, err error) {
	klog.V(2).InfoS("Tainting connection for agent", "agentID", agentID, "error", err)
	if err == io.EOF {
		// No point in tainting something which is going away cleanly.
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	backends, ok := s.backends[agentID]
	if !ok {
		klog.V(4).InfoS("Cannot taint agent in backends", "agentID", agentID)
		return
	}
	// GetBackend always returns conn 0 for an agentID so assuming this is the correct conn.
	s.removeUntaintedSafe(agentID)
	backends[0].taintExpires = time.Now().Add(s.keepaliveTime)
	klog.V(5).InfoS("Setting taint expiry time",
		"agentID", agentID,
		"taintExpires", backends[0].taintExpires)
}

// RemoveBackend removes a backend.
func (s *DefaultBackendStorage) RemoveBackend(agentID string, conn agent.AgentService_ConnectServer) {
	klog.V(2).InfoS("Remove connection for agent", "connection", conn, "agentID", agentID)
	s.mu.Lock()
	defer s.mu.Unlock()
	backends, ok := s.backends[agentID]
	if !ok {
		klog.V(1).InfoS("Cannot find agent in backends", "agentID", agentID)
		return
	}
	var found bool
	for i, c := range backends {
		if c.conn == conn {
			s.backends[agentID] = append(s.backends[agentID][:i], s.backends[agentID][i+1:]...)
			if i == 0 && len(s.backends[agentID]) != 0 {
				klog.V(1).InfoS("This should not happen. Removed connection that is not the first connection", "connection", conn, "remainingConnections", s.backends[agentID])
			}
			found = true
		}
	}
	if len(s.backends[agentID]) == 0 {
		delete(s.backends, agentID)
		for i, id := range s.agentIDs {
			if id == agentID {
				s.agentIDs[i] = s.agentIDs[len(s.agentIDs)-1]
				s.agentIDs = s.agentIDs[:len(s.agentIDs)-1]
				break
			}
		}
		s.removeUntaintedSafe(agentID)
	}
	if !found {
		klog.V(1).InfoS("Cannot find connection for agent in backends", "connection", conn, "agentID", agentID)
	}
}

// NumBackends resturns the number of available backends
func (s *DefaultBackendStorage) NumBackends() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.backends)
}

// ErrNotFound indicates that no backend can be found.
type ErrNotFound struct{}

// Error returns the error message.
func (e *ErrNotFound) Error() string {
	return "No backend available"
}

// GetRandomBackend returns a random backend.
func (s *DefaultBackendStorage) GetRandomBackend() (Backend, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.backends) == 0 {
		return nil, "", &ErrNotFound{}
	}
	// First try find a connection from the complete list.
	// This is how we give tainted backends a chance to untaint.
	agentID := s.agentIDs[s.random.Intn(len(s.agentIDs))]
	now := time.Now()
	taintExpires := s.backends[agentID][0].taintExpires
	untaintedLen := s.getUntaintedLengthSafe()
	klog.V(5).InfoS("Checking backend for taint",
		"agentID", agentID,
		"taintExpires", taintExpires,
		"now", now,
		"untaintedLen", untaintedLen,
		"BOT", BOT)
	if taintExpires.After(now) && untaintedLen > 0 {
		// Got a still tainted tunnel and there are untainted tunnels.
		// No luck on complete list. Now attempting to get from the untainted list.
		agentID = s.getUntaintedAgentIDSafe(s.random.Intn(untaintedLen))
		klog.V(5).InfoS("Found taint",
			"newAgentID", agentID)
	} else  if taintExpires != BOT && taintExpires.Before(now) {
		// The taint has expired and the tunnel has not yet been cleaned up.
		// Given that add the tunnel back to the untainted list.
		// This is a write operation so must occur under a write lock.
		s.appendUntaintedSafe(agentID)
		klog.V(5).InfoS("Removed taint",
			"backend", s.backends[agentID][0])
	}
	klog.V(4).InfoS("Pick agent as backend", "agentID", agentID)

	// always return the first connection to an agent, because the agent
	// will close later connections if there are multiple.
	return s.backends[agentID][0], agentID, nil
}

func (s *DefaultBackendStorage) getUntaintedLengthSafe() int {
	s.untaintedIDsmu.RLock()
	defer s.untaintedIDsmu.RUnlock()
	return len(s.untaintedIDs)
}

func (s *DefaultBackendStorage) getUntaintedAgentIDSafe(index int) string {
	s.untaintedIDsmu.RLock()
	defer s.untaintedIDsmu.RUnlock()
	return s.untaintedIDs[index]
}

func (s *DefaultBackendStorage) appendUntaintedSafe(agentID string) {
	s.untaintedIDsmu.Lock()
	defer s.untaintedIDsmu.Unlock()
	s.untaintedIDs = append(s.untaintedIDs, agentID)
	s.backends[agentID][0].taintExpires = BOT
}

func (s *DefaultBackendStorage) removeUntaintedSafe(agentID string) {
	s.untaintedIDsmu.Lock()
	defer s.untaintedIDsmu.Unlock()
	for i, id := range s.untaintedIDs {
		if id == agentID {
			s.untaintedIDs[i] = s.untaintedIDs[len(s.untaintedIDs)-1]
			s.untaintedIDs = s.untaintedIDs[:len(s.untaintedIDs)-1]
			break
		}
	}
}