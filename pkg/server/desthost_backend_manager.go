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

	"k8s.io/klog/v2"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies"
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type DestHostBackendManager struct {
	*DefaultBackendStorage
	// nonDrainingBackends tracks non-draining backends per destination host.
	// This avoids scanning all candidate backends on each request.
	nonDrainingBackends map[string][]*Backend
	// nonDrainingIndex tracks backend index positions for O(1) removals.
	nonDrainingIndex map[string]map[*Backend]int
}

var _ BackendManager = &DestHostBackendManager{}

func NewDestHostBackendManager() *DestHostBackendManager {
	return &DestHostBackendManager{
		DefaultBackendStorage: NewDefaultBackendStorage(
			[]header.IdentifierType{header.IPv4, header.IPv6, header.Host}, proxystrategies.ProxyStrategyDestHost),
		nonDrainingBackends: make(map[string][]*Backend),
		nonDrainingIndex:    make(map[string]map[*Backend]int),
	}
}

func (dibm *DestHostBackendManager) addNonDrainingBackendLocked(identifier string, backend *Backend) {
	backendIndexByIdentifier, ok := dibm.nonDrainingIndex[identifier]
	if !ok {
		backendIndexByIdentifier = make(map[*Backend]int)
		dibm.nonDrainingIndex[identifier] = backendIndexByIdentifier
	}
	if _, ok := backendIndexByIdentifier[backend]; ok {
		return
	}
	dibm.nonDrainingBackends[identifier] = append(dibm.nonDrainingBackends[identifier], backend)
	backendIndexByIdentifier[backend] = len(dibm.nonDrainingBackends[identifier]) - 1
}

func (dibm *DestHostBackendManager) removeNonDrainingBackendLocked(identifier string, backend *Backend) {
	backendIndexByIdentifier, ok := dibm.nonDrainingIndex[identifier]
	if !ok {
		return
	}
	idx, ok := backendIndexByIdentifier[backend]
	if !ok {
		return
	}

	lastIdx := len(dibm.nonDrainingBackends[identifier]) - 1
	if idx != lastIdx {
		lastBackend := dibm.nonDrainingBackends[identifier][lastIdx]
		dibm.nonDrainingBackends[identifier][idx] = lastBackend
		backendIndexByIdentifier[lastBackend] = idx
	}
	dibm.nonDrainingBackends[identifier] = dibm.nonDrainingBackends[identifier][:lastIdx]
	delete(backendIndexByIdentifier, backend)

	if len(dibm.nonDrainingBackends[identifier]) == 0 {
		delete(dibm.nonDrainingBackends, identifier)
		delete(dibm.nonDrainingIndex, identifier)
	}
}

func (dibm *DestHostBackendManager) addBackendForIdentifierLocked(identifier string, idType header.IdentifierType, backend *Backend, isDraining bool) {
	dibm.addBackendLocked(identifier, idType, backend)
	if !isDraining {
		dibm.addNonDrainingBackendLocked(identifier, backend)
	}
}

func (dibm *DestHostBackendManager) removeBackendForIdentifierLocked(identifier string, idType header.IdentifierType, backend *Backend) {
	dibm.removeBackendLocked(identifier, idType, backend)
	dibm.removeNonDrainingBackendLocked(identifier, backend)
}

func (dibm *DestHostBackendManager) AddBackend(backend *Backend) {
	isDraining := backend.IsDraining()
	agentIdentifiers := backend.GetAgentIdentifiers()
	dibm.mu.Lock()
	defer dibm.mu.Unlock()
	for _, ipv4 := range agentIdentifiers.IPv4 {
		klog.V(5).InfoS("Add the agent to DestHostBackendManager", "agent address", ipv4)
		dibm.addBackendForIdentifierLocked(ipv4, header.IPv4, backend, isDraining)
	}
	for _, ipv6 := range agentIdentifiers.IPv6 {
		klog.V(5).InfoS("Add the agent to DestHostBackendManager", "agent address", ipv6)
		dibm.addBackendForIdentifierLocked(ipv6, header.IPv6, backend, isDraining)
	}
	for _, host := range agentIdentifiers.Host {
		klog.V(5).InfoS("Add the agent to DestHostBackendManager", "agent address", host)
		dibm.addBackendForIdentifierLocked(host, header.Host, backend, isDraining)
	}
}

func (dibm *DestHostBackendManager) RemoveBackend(backend *Backend) {
	agentIdentifiers := backend.GetAgentIdentifiers()
	dibm.mu.Lock()
	defer dibm.mu.Unlock()
	for _, ipv4 := range agentIdentifiers.IPv4 {
		klog.V(5).InfoS("Remove the agent from the DestHostBackendManager", "agentHost", ipv4)
		dibm.removeBackendForIdentifierLocked(ipv4, header.IPv4, backend)
	}
	for _, ipv6 := range agentIdentifiers.IPv6 {
		klog.V(5).InfoS("Remove the agent from the DestHostBackendManager", "agentHost", ipv6)
		dibm.removeBackendForIdentifierLocked(ipv6, header.IPv6, backend)
	}
	for _, host := range agentIdentifiers.Host {
		klog.V(5).InfoS("Remove the agent from the DestHostBackendManager", "agentHost", host)
		dibm.removeBackendForIdentifierLocked(host, header.Host, backend)
	}
}

// Backend tries to get a backend associating to the request destination host.
func (dibm *DestHostBackendManager) Backend(ctx context.Context) (*Backend, error) {
	dibm.mu.Lock()
	defer dibm.mu.Unlock()
	if len(dibm.backends) == 0 {
		return nil, &ErrNotFound{}
	}
	destHost, _ := ctx.Value(destHostKey).(string)
	if destHost != "" {
		// Prefer random selection from known non-draining backends.
		// Remove stale entries lazily when backends transition to draining.
		for len(dibm.nonDrainingBackends[destHost]) > 0 {
			idx := dibm.random.Intn(len(dibm.nonDrainingBackends[destHost]))
			backend := dibm.nonDrainingBackends[destHost][idx]
			if backend.IsDraining() {
				dibm.removeNonDrainingBackendLocked(destHost, backend)
				continue
			}
			klog.V(5).InfoS("Get the backend through the DestHostBackendManager", "destHost", destHost)
			return backend, nil
		}

		// All backends for this destination are draining, use one as fallback.
		bes, exist := dibm.backends[destHost]
		if exist && len(bes) > 0 {
			backend := bes[dibm.random.Intn(len(bes))]
			klog.V(3).InfoS("All backends for destination host are draining, using one as fallback", "destHost", destHost)
			return backend, nil
		}
	}
	return nil, &ErrNotFound{}
}
