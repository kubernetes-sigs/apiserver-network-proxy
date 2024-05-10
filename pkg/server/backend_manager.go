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
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc/metadata"
	commonmetrics "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/common/metrics"
	"sigs.k8s.io/apiserver-network-proxy/konnectivity-client/proto/client"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	"sigs.k8s.io/apiserver-network-proxy/proto/agent"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type ProxyStrategy int

const (
	// With this strategy the Proxy Server will randomly pick a backend from
	// the current healthy backends to establish the tunnel over which to
	// forward requests.
	ProxyStrategyDefault ProxyStrategy = iota + 1
	// With this strategy the Proxy Server will pick a backend that has the same
	// associated host as the request.Host to establish the tunnel.
	ProxyStrategyDestHost
	// ProxyStrategyDefaultRoute will only forward traffic to agents that have explicity advertised
	// they serve the default route through an agent identifier. Typically used in combination with destHost
	ProxyStrategyDefaultRoute
)

func (ps ProxyStrategy) String() string {
	switch ps {
	case ProxyStrategyDefault:
		return "default"
	case ProxyStrategyDestHost:
		return "destHost"
	case ProxyStrategyDefaultRoute:
		return "defaultRoute"
	}
	panic(fmt.Sprintf("unhandled ProxyStrategy: %d", ps))
}

func ParseProxyStrategy(s string) (ProxyStrategy, error) {
	switch s {
	case ProxyStrategyDefault.String():
		return ProxyStrategyDefault, nil
	case ProxyStrategyDestHost.String():
		return ProxyStrategyDestHost, nil
	case ProxyStrategyDefaultRoute.String():
		return ProxyStrategyDefaultRoute, nil
	default:
		return 0, fmt.Errorf("unknown proxy strategy: %s", s)
	}
}

// GenProxyStrategiesFromStr generates the list of proxy strategies from the
// comma-seperated string, i.e., destHost.
func ParseProxyStrategies(proxyStrategies string) ([]ProxyStrategy, error) {
	var result []ProxyStrategy

	strs := strings.Split(proxyStrategies, ",")
	for _, s := range strs {
		if len(s) == 0 {
			continue
		}
		ps, err := ParseProxyStrategy(s)
		if err != nil {
			return nil, err
		}
		result = append(result, ps)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("proxy strategies cannot be empty")
	}
	return result, nil
}

// Backend abstracts a connected Konnectivity agent.
//
// In the only currently supported case (gRPC), it wraps an
// agent.AgentService_ConnectServer, provides synchronization and
// emits common stream metrics.
type Backend struct {
	sendLock sync.Mutex
	recvLock sync.Mutex
	conn     agent.AgentService_ConnectServer

	// cached from conn.Context()
	id     string
	idents header.Identifiers
}

func (b *Backend) Send(p *client.Packet) error {
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

func (b *Backend) Recv() (*client.Packet, error) {
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

func (b *Backend) Context() context.Context {
	// TODO: does Context require lock protection?
	return b.conn.Context()
}

func (b *Backend) GetAgentID() string {
	return b.id
}

func (b *Backend) GetAgentIdentifiers() header.Identifiers {
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

func NewBackend(conn agent.AgentService_ConnectServer) (*Backend, error) {
	agentID, err := getAgentID(conn)
	if err != nil {
		return nil, err
	}
	agentIdentifiers, err := getAgentIdentifiers(conn)
	if err != nil {
		return nil, err
	}
	return &Backend{conn: conn, id: agentID, idents: agentIdentifiers}, nil
}

// BackendManager is an interface to manage backend connections, i.e.,
// connection to the proxy agents.
type BackendManager interface {
	// Backend returns a backend connection according to proxy strategies.
	Backend(addr string) (*Backend, error)
	// AddBackend adds a backend.
	AddBackend(backend *Backend)
	// RemoveBackend adds a backend.
	RemoveBackend(backend *Backend)
	// NumBackends returns the number of backends.
	NumBackends() int
	ReadinessManager
}

var _ BackendManager = &DefaultBackendManager{}

// DefaultBackendManager is the default backend manager.
type DefaultBackendManager struct {
	proxyStrategies []ProxyStrategy

	// All backends by agentID.
	all BackendStorage
	// All backends by host identifier(s). Only used with ProxyStrategyDestHost.
	byHost BackendStorage
	// All default-route backends, by agentID. Only used with ProxyStrategyDefaultRoute.
	byDefaultRoute BackendStorage
}

// NewDefaultBackendManager returns a DefaultBackendManager.
func NewDefaultBackendManager(proxyStrategies []ProxyStrategy) *DefaultBackendManager {
	metrics.Metrics.SetBackendCount(0)
	return &DefaultBackendManager{
		proxyStrategies: proxyStrategies,
		all:             NewDefaultBackendStorage(),
		byHost:          NewDefaultBackendStorage(),
		byDefaultRoute:  NewDefaultBackendStorage(),
	}
}

func (s *DefaultBackendManager) Backend(addr string) (*Backend, error) {
	for _, strategy := range s.proxyStrategies {
		var b *Backend
		var e error
		e = &ErrNotFound{}
		switch strategy {
		case ProxyStrategyDefault:
			b, e = s.all.RandomBackend()
		case ProxyStrategyDestHost:
			b, e = s.byHost.GetBackend(addr)
		case ProxyStrategyDefaultRoute:
			b, e = s.byDefaultRoute.RandomBackend()
		}
		if e == nil {
			return b, nil
		}
	}
	return nil, &ErrNotFound{}
}

func hostIdentifiers(backend *Backend) []string {
	hosts := []string{}
	hosts = append(hosts, backend.GetAgentIdentifiers().IPv4...)
	hosts = append(hosts, backend.GetAgentIdentifiers().IPv6...)
	hosts = append(hosts, backend.GetAgentIdentifiers().Host...)
	return hosts
}

func (s *DefaultBackendManager) AddBackend(backend *Backend) {
	agentID := backend.GetAgentID()
	count := s.all.AddBackend([]string{agentID}, backend)
	if slices.Contains(s.proxyStrategies, ProxyStrategyDestHost) {
		idents := hostIdentifiers(backend)
		s.byHost.AddBackend(idents, backend)
	}
	if slices.Contains(s.proxyStrategies, ProxyStrategyDefaultRoute) {
		if backend.GetAgentIdentifiers().DefaultRoute {
			s.byDefaultRoute.AddBackend([]string{agentID}, backend)
		}
	}
	metrics.Metrics.SetBackendCount(count)
}

func (s *DefaultBackendManager) RemoveBackend(backend *Backend) {
	agentID := backend.GetAgentID()
	count := s.all.RemoveBackend([]string{agentID}, backend)
	if slices.Contains(s.proxyStrategies, ProxyStrategyDestHost) {
		idents := hostIdentifiers(backend)
		s.byHost.RemoveBackend(idents, backend)
	}
	if slices.Contains(s.proxyStrategies, ProxyStrategyDefaultRoute) {
		if backend.GetAgentIdentifiers().DefaultRoute {
			s.byDefaultRoute.RemoveBackend([]string{agentID}, backend)
		}
	}
	metrics.Metrics.SetBackendCount(count)
}

func (s *DefaultBackendManager) NumBackends() int {
	return s.all.NumKeys()
}

// ErrNotFound indicates that no backend can be found.
type ErrNotFound struct{}

// Error returns the error message.
func (e *ErrNotFound) Error() string {
	return "No agent available"
}

func (s *DefaultBackendManager) Ready() (bool, string) {
	if s.NumBackends() == 0 {
		return false, "no connection to any proxy agent"
	}
	return true, ""
}
