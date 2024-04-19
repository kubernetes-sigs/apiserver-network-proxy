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
	"fmt"
	"io"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"
	"k8s.io/klog/v2"

	commonmetrics "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics"
	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type ProxyStrategy string

const (
	// With this strategy the Proxy Server will randomly pick a backend from
	// the current healthy backends to establish the tunnel over which to
	// forward requests.
	ProxyStrategyDefault ProxyStrategy = "default"
	// With this strategy the Proxy Server will pick a backend that has the same
	// associated host as the request.Host to establish the tunnel.
	ProxyStrategyDestHost ProxyStrategy = "destHost"

	// ProxyStrategyDefaultRoute will only forward traffic to agents that have explicity advertised
	// they serve the default route through an agent identifier. Typically used in combination with destHost
	ProxyStrategyDefaultRoute ProxyStrategy = "defaultRoute"
)

// GenProxyStrategiesFromStr generates the list of proxy strategies from the
// comma-seperated string, i.e., destHost.
func GenProxyStrategiesFromStr(proxyStrategies string) ([]ProxyStrategy, error) {
	var ps []ProxyStrategy
	strs := strings.Split(proxyStrategies, ",")
	for _, s := range strs {
		switch s {
		case string(ProxyStrategyDestHost):
			ps = append(ps, ProxyStrategyDestHost)
		case string(ProxyStrategyDefault):
			ps = append(ps, ProxyStrategyDefault)
		case string(ProxyStrategyDefaultRoute):
			ps = append(ps, ProxyStrategyDefaultRoute)
		default:
			return nil, fmt.Errorf("Unknown proxy strategy %s", s)
		}
	}
	return ps, nil
}

// Backend abstracts a connected Konnectivity agent.
//
// In the only currently supported case (gRPC), it wraps an
// agent.AgentService_ConnectServer, provides synchronization and
// emits common stream metrics.
type Backend interface {
	Send(p *client.Packet) error
	Recv() (*client.Packet, error)
	Context() context.Context
	GetAgentID() string
	GetAgentIdentifiers() header.Identifiers
}

var _ Backend = &backend{}

type backend struct {
	sendLock sync.Mutex
	recvLock sync.Mutex
	conn     agent.AgentService_ConnectServer

	// cached from conn.Context()
	id     string
	idents header.Identifiers
}

func (b *backend) Send(p *client.Packet) error {
	b.sendLock.Lock()
	defer b.sendLock.Unlock()

	const segment = commonmetrics.SegmentToAgent
	metrics.Metrics.ObservePacket(segment, p.Type)
	err := b.conn.Send(p)
	if err != nil && err != io.EOF {
		metrics.Metrics.ObserveStreamError(segment, err, p.Type)
	}
	return err
}

func (b *backend) Recv() (*client.Packet, error) {
	b.recvLock.Lock()
	defer b.recvLock.Unlock()

	const segment = commonmetrics.SegmentFromAgent
	pkt, err := b.conn.Recv()
	if err != nil {
		if err != io.EOF {
			metrics.Metrics.ObserveStreamErrorNoPacket(segment, err)
		}
		return nil, err
	}
	metrics.Metrics.ObservePacket(segment, pkt.Type)
	return pkt, nil
}

func (b *backend) Context() context.Context {
	// TODO: does Context require lock protection?
	return b.conn.Context()
}

func (b *backend) GetAgentID() string {
	return b.id
}

func (b *backend) GetAgentIdentifiers() header.Identifiers {
	return b.idents
}

func getAgentID(stream agent.AgentService_ConnectServer) (string, error) {
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

func getAgentIdentifiers(conn agent.AgentService_ConnectServer) (header.Identifiers, error) {
	var agentIdentifiers header.Identifiers
	md, ok := metadata.FromIncomingContext(conn.Context())
	if !ok {
		return agentIdentifiers, fmt.Errorf("failed to get metadata from context")
	}
	agentIdent := md.Get(header.AgentIdentifiers)
	if len(agentIdent) > 1 {
		return agentIdentifiers, fmt.Errorf("expected at most one set of agent identifiers in the context, got %v", agentIdent)
	}
	if len(agentIdent) == 0 {
		return agentIdentifiers, nil
	}

	return header.GenAgentIdentifiers(agentIdent[0])
}

func NewBackend(conn agent.AgentService_ConnectServer) (Backend, error) {
	agentID, err := getAgentID(conn)
	if err != nil {
		return nil, err
	}
	agentIdentifiers, err := getAgentIdentifiers(conn)
	if err != nil {
		return nil, err
	}
	return &backend{conn: conn, id: agentID, idents: agentIdentifiers}, nil
}

// BackendStorage is an interface to manage the storage of the backend
// connections, i.e., get, add and remove
type BackendStorage interface {
	// addBackend adds a backend.
	addBackend(identifier string, idType header.IdentifierType, backend Backend)
	// removeBackend removes a backend.
	removeBackend(identifier string, idType header.IdentifierType, backend Backend)
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
	Backend(ctx context.Context) (Backend, error)
	// AddBackend adds a backend.
	AddBackend(backend Backend)
	// RemoveBackend adds a backend.
	RemoveBackend(backend Backend)
	BackendStorage
	ReadinessManager
}

var _ BackendManager = &DefaultBackendManager{}

// DefaultBackendManager is the default backend manager.
type DefaultBackendManager struct {
	*DefaultBackendStorage
}

func (dbm *DefaultBackendManager) Backend(_ context.Context) (Backend, error) {
	klog.V(5).InfoS("Get a random backend through the DefaultBackendManager")
	return dbm.DefaultBackendStorage.GetRandomBackend()
}

func (dbm *DefaultBackendManager) AddBackend(backend Backend) {
	agentID := backend.GetAgentID()
	klog.V(5).InfoS("Add the agent to DefaultBackendManager", "agentID", agentID)
	dbm.addBackend(agentID, header.UID, backend)
}

func (dbm *DefaultBackendManager) RemoveBackend(backend Backend) {
	agentID := backend.GetAgentID()
	klog.V(5).InfoS("Remove the agent from the DefaultBackendManager", "agentID", agentID)
	dbm.removeBackend(agentID, header.UID, backend)
}

// DefaultBackendStorage is the default backend storage.
type DefaultBackendStorage struct {
	mu sync.RWMutex //protects the following
	// A map between agentID and its grpc connections.
	// For a given agent, ProxyServer prefers backends[agentID][0] to send
	// traffic, because backends[agentID][1:] are more likely to be closed
	// by the agent to deduplicate connections to the same server.
	//
	// TODO: fix documentation. This is not always agentID, e.g. in
	// the case of DestHostBackendManager.
	backends map[string][]Backend
	// agentID is tracked in this slice to enable randomly picking an
	// agentID in the Backend() method. There is no reliable way to
	// randomly pick a key from a map (in this case, the backends) in
	// Golang.
	agentIDs []string
	random   *rand.Rand
	// idTypes contains the valid identifier types for this
	// DefaultBackendStorage. The DefaultBackendStorage may only tolerate certain
	// types of identifiers when associating to a specific BackendManager,
	// e.g., when associating to the DestHostBackendManager, it can only use the
	// identifiers of types, IPv4, IPv6 and Host.
	idTypes []header.IdentifierType
}

// NewDefaultBackendManager returns a DefaultBackendManager.
func NewDefaultBackendManager() *DefaultBackendManager {
	return &DefaultBackendManager{
		DefaultBackendStorage: NewDefaultBackendStorage(
			[]header.IdentifierType{header.UID})}
}

// NewDefaultBackendStorage returns a DefaultBackendStorage
func NewDefaultBackendStorage(idTypes []header.IdentifierType) *DefaultBackendStorage {
	// Set an explicit value, so that the metric is emitted even when
	// no agent ever successfully connects.
	metrics.Metrics.SetBackendCount(0)
	return &DefaultBackendStorage{
		backends: make(map[string][]Backend),
		random:   rand.New(rand.NewSource(time.Now().UnixNano())),
		idTypes:  idTypes,
	} /* #nosec G404 */
}

func containIDType(idTypes []header.IdentifierType, idType header.IdentifierType) bool {
	return slices.Contains(idTypes, idType)
}

// addBackend adds a backend.
func (s *DefaultBackendStorage) addBackend(identifier string, idType header.IdentifierType, backend Backend) {
	if !containIDType(s.idTypes, idType) {
		klog.V(4).InfoS("fail to add backend", "backend", identifier, "error", &ErrWrongIDType{idType, s.idTypes})
		return
	}
	klog.V(5).InfoS("Register backend for agent", "agentID", identifier)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.backends[identifier]
	if ok {
		for _, b := range s.backends[identifier] {
			if b == backend {
				klog.V(1).InfoS("This should not happen. Adding existing backend for agent", "agentID", identifier)
				return
			}
		}
		s.backends[identifier] = append(s.backends[identifier], backend)
		return
	}
	s.backends[identifier] = []Backend{backend}
	metrics.Metrics.SetBackendCount(len(s.backends))
	s.agentIDs = append(s.agentIDs, identifier)
}

// removeBackend removes a backend.
func (s *DefaultBackendStorage) removeBackend(identifier string, idType header.IdentifierType, backend Backend) {
	if !containIDType(s.idTypes, idType) {
		klog.ErrorS(&ErrWrongIDType{idType, s.idTypes}, "fail to remove backend")
		return
	}
	klog.V(5).InfoS("Remove connection for agent", "agentID", identifier)
	s.mu.Lock()
	defer s.mu.Unlock()
	backends, ok := s.backends[identifier]
	if !ok {
		klog.V(1).InfoS("Cannot find agent in backends", "identifier", identifier)
		return
	}
	var found bool
	for i, b := range backends {
		if b == backend {
			s.backends[identifier] = append(s.backends[identifier][:i], s.backends[identifier][i+1:]...)
			if i == 0 && len(s.backends[identifier]) != 0 {
				klog.V(1).InfoS("This should not happen. Removed connection that is not the first connection", "agentID", identifier)
			}
			found = true
		}
	}
	if len(s.backends[identifier]) == 0 {
		delete(s.backends, identifier)
		for i := range s.agentIDs {
			if s.agentIDs[i] == identifier {
				s.agentIDs[i] = s.agentIDs[len(s.agentIDs)-1]
				s.agentIDs = s.agentIDs[:len(s.agentIDs)-1]
				break
			}
		}
	}
	if !found {
		klog.V(1).InfoS("Could not find connection matching identifier to remove", "agentID", identifier, "idType", idType)
	}
	metrics.Metrics.SetBackendCount(len(s.backends))
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
	return "No agent available"
}

type ErrWrongIDType struct {
	got    header.IdentifierType
	expect []header.IdentifierType
}

func (e *ErrWrongIDType) Error() string {
	return fmt.Sprintf("incorrect id type: got %s, expect %s", e.got, e.expect)
}

func ignoreNotFound(err error) error {
	if _, ok := err.(*ErrNotFound); ok {
		return nil
	}
	return err
}

// GetRandomBackend returns a random backend connection from all connected agents.
func (s *DefaultBackendStorage) GetRandomBackend() (Backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.backends) == 0 {
		return nil, &ErrNotFound{}
	}
	agentID := s.agentIDs[s.random.Intn(len(s.agentIDs))]
	klog.V(5).InfoS("Pick agent as backend", "agentID", agentID)
	// always return the first connection to an agent, because the agent
	// will close later connections if there are multiple.
	return s.backends[agentID][0], nil
}
