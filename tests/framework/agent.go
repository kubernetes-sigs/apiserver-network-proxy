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
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	agentapp "sigs.k8s.io/apiserver-network-proxy/cmd/agent/app"
	agentopts "sigs.k8s.io/apiserver-network-proxy/cmd/agent/app/options"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
)

type AgentOpts struct {
	AgentID    string
	ServerAddr string
}

type AgentRunner interface {
	Start(testing.TB, AgentOpts) (Agent, error)
}

type Agent interface {
	GetConnectedServerCount() (int, error)
	Ready() bool
	Stop()
}
type InProcessAgentRunner struct{}

func (*InProcessAgentRunner) Start(t testing.TB, opts AgentOpts) (Agent, error) {
	a := &agentapp.Agent{}
	o, err := agentOptions(t, opts)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopCh := make(chan struct{})
	go func() {
		if err := a.Run(o, stopCh); err != nil {
			log.Printf("ERROR running agent: %v", err)
			cancel()
		}
	}()

	healthAddr := net.JoinHostPort(o.HealthServerHost, strconv.Itoa(o.HealthServerPort))
	if err := wait.PollImmediateWithContext(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, func(context.Context) (bool, error) {
		return checkLiveness(healthAddr), nil
	}); err != nil {
		close(stopCh)
		return nil, fmt.Errorf("agent never came up: %v", err)
	}

	pa := &inProcessAgent{
		client:     a.ClientSet(),
		stopCh:     stopCh,
		healthAddr: healthAddr,
	}
	t.Cleanup(pa.Stop)
	return pa, nil
}

type inProcessAgent struct {
	client *agent.ClientSet

	stopOnce sync.Once
	stopCh   chan struct{}

	healthAddr string
}

func (a *inProcessAgent) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
	})
}

func (a *inProcessAgent) GetConnectedServerCount() (int, error) {
	return a.client.HealthyClientsCount(), nil
}

func (a *inProcessAgent) Ready() bool {
	return checkReadiness(a.healthAddr)
}

func agentOptions(t testing.TB, opts AgentOpts) (*agentopts.GrpcProxyAgentOptions, error) {
	t.Helper()
	o := agentopts.NewGrpcProxyAgentOptions()

	host, port, err := net.SplitHostPort(opts.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ServerAddr: %w", err)
	}
	o.ProxyServerHost = host
	if o.ProxyServerPort, err = strconv.Atoi(port); err != nil {
		return nil, fmt.Errorf("invalid server port: %w", err)
	}

	o.AgentID = opts.AgentID
	o.SyncInterval = 100 * time.Millisecond
	o.SyncIntervalCap = 1 * time.Second
	o.ProbeInterval = 100 * time.Millisecond

	o.AgentCert = filepath.Join(CertsDir, TestAgentCertFile)
	o.AgentKey = filepath.Join(CertsDir, TestAgentKeyFile)
	o.CaCert = filepath.Join(CertsDir, TestCAFile)

	const localhost = "127.0.0.1"
	o.HealthServerHost = localhost
	o.AdminBindAddress = localhost

	ports, err := FreePorts(2)
	if err != nil {
		return nil, err
	}
	o.HealthServerPort = ports[0]
	o.AdminServerPort = ports[1]

	return o, nil
}
