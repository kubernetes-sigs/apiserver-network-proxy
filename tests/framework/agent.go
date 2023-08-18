/*
Copyright 2023 The Kubernetes Authors.

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

package framework

import (
	"time"

	"google.golang.org/grpc"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
)

type AgentOpts struct {
	AgentID    string
	ServerAddr string
}

type AgentRunner interface {
	Start(AgentOpts) (Agent, error)
}

type Agent interface {
	GetConnectedServerCount() (int, error)
	Ready() bool
	Stop() error
}

type InProcessAgentRunner struct{}

func (*InProcessAgentRunner) Start(opts AgentOpts) (Agent, error) {
	cc := agent.ClientSetConfig{
		Address:       opts.ServerAddr,
		AgentID:       opts.AgentID,
		SyncInterval:  100 * time.Millisecond,
		ProbeInterval: 100 * time.Millisecond,
		DialOptions:   []grpc.DialOption{grpc.WithInsecure()},
	}

	stopCh := make(chan struct{})
	client := cc.NewAgentClientSet(stopCh)
	client.Serve()

	return &inProcessAgent{
		client: client,
		stopCh: stopCh,
	}, nil
}

type inProcessAgent struct {
	client *agent.ClientSet
	stopCh chan struct{}
}

func (a *inProcessAgent) Stop() error {
	close(a.stopCh)
	return nil
}

func (a *inProcessAgent) GetConnectedServerCount() (int, error) {
	return a.client.HealthyClientsCount(), nil
}

func (a *inProcessAgent) Ready() bool {
	return a.client.Ready()
}
