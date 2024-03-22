/*
Copyright 2021 The Kubernetes Authors.

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
	"sigs.k8s.io/apiserver-network-proxy/proto/header"
)

type DefaultRouteBackendManager struct {
	*DefaultBackendStorage
}

var _ BackendManager = &DefaultRouteBackendManager{}

func NewDefaultRouteBackendManager() *DefaultRouteBackendManager {
	return &DefaultRouteBackendManager{
		DefaultBackendStorage: NewDefaultBackendStorage(
			[]header.IdentifierType{header.DefaultRoute})}
}

// Backend tries to get a backend that advertises default route, with random selection.
func (dibm *DefaultRouteBackendManager) Backend(ctx context.Context) (Backend, error) {
	return dibm.GetRandomBackend()
}

func (dibm *DefaultRouteBackendManager) AddBackend(backend Backend) {
	agentID := backend.GetAgentID()
	agentIdentifiers := backend.GetAgentIdentifiers()
	if agentIdentifiers.DefaultRoute {
		klog.V(5).InfoS("Add the agent to DefaultRouteBackendManager", "agentID", agentID)
		dibm.addBackend(agentID, header.DefaultRoute, backend)
	}
}

func (dibm *DefaultRouteBackendManager) RemoveBackend(backend Backend) {
	agentID := backend.GetAgentID()
	agentIdentifiers := backend.GetAgentIdentifiers()
	if agentIdentifiers.DefaultRoute {
		klog.V(5).InfoS("Remove the agent from the DefaultRouteBackendManager", "agentID", agentID)
		dibm.removeBackend(agentID, header.DefaultRoute, backend)
	}
}
