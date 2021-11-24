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
	pkgagent "sigs.k8s.io/apiserver-network-proxy/pkg/agent"
)

type DefaultRouteBackendManager struct {
	*DefaultBackendStorage
}

var _ BackendManager = &DefaultRouteBackendManager{}

func NewDefaultRouteBackendManager() *DefaultRouteBackendManager {
	return &DefaultRouteBackendManager{
		DefaultBackendStorage: NewDefaultBackendStorage(
			[]pkgagent.IdentifierType{pkgagent.DefaultRoute},
			"DefaultRouteBackendManager")}
}

// Backend tries to get a backend associating to the request destination host.
func (drbm *DefaultRouteBackendManager) Backend(ctx context.Context) (Backend, error) {
	drbm.mu.RLock()
	defer drbm.mu.RUnlock()
	if len(drbm.backends) == 0 {
		return nil, &ErrNotFound{}
	}
	if len(drbm.defaultRouteAgentIDs) == 0 {
		return nil, &ErrNotFound{}
	}
	agentID := drbm.defaultRouteAgentIDs[drbm.random.Intn(len(drbm.defaultRouteAgentIDs))]
	klog.V(4).InfoS("Picked agent as backend", "agentID", agentID)
	return drbm.backends[agentID][0], nil
}
