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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/wait"

	agentapp "sigs.k8s.io/apiserver-network-proxy/cmd/agent/app"
	agentopts "sigs.k8s.io/apiserver-network-proxy/cmd/agent/app/options"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
	agentmetrics "sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
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
	Drain()
	Stop()
	Metrics() metricstest.AgentTester
}
type InProcessAgentRunner struct{}

func (*InProcessAgentRunner) Start(t testing.TB, opts AgentOpts) (Agent, error) {
	a := &agentapp.Agent{}
	o, err := agentOptions(t, opts)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	drainCh := make(chan struct{})
	stopCh := make(chan struct{})
	go func() {
		if err := a.Run(o, drainCh, stopCh); err != nil {
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
		drainCh:    drainCh,
		stopCh:     stopCh,
		healthAddr: healthAddr,
	}
	t.Cleanup(pa.Stop)
	return pa, nil
}

type inProcessAgent struct {
	client *agent.ClientSet

	drainOnce sync.Once
	drainCh   chan struct{}

	stopOnce sync.Once
	stopCh   chan struct{}

	healthAddr string
}

func (a *inProcessAgent) Drain() {
	a.drainOnce.Do(func() {
		close(a.drainCh)
	})
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

func (a *inProcessAgent) Metrics() metricstest.AgentTester {
	return metricstest.DefaultTester
}

type ExternalAgentRunner struct {
	ExecutablePath string
}

func (r *ExternalAgentRunner) Start(t testing.TB, opts AgentOpts) (Agent, error) {
	o, err := agentOptions(t, opts)
	if err != nil {
		return nil, err
	}

	args := []string{}
	o.Flags().VisitAll(func(f *pflag.Flag) {
		args = append(args, fmt.Sprintf("--%s=%s", f.Name, f.Value.String()))
	})

	cmd := exec.Command(r.ExecutablePath, args...)
	cmd.Stdout = os.Stdout // Forward stdout & stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	healthAddr := net.JoinHostPort(o.HealthServerHost, strconv.Itoa(o.HealthServerPort))
	a := &externalAgent{
		cmd:        cmd,
		healthAddr: healthAddr,
		adminAddr:  net.JoinHostPort(o.AdminBindAddress, strconv.Itoa(o.AdminServerPort)),
		agentID:    opts.AgentID,
		metrics:    &metricstest.Tester{Endpoint: fmt.Sprintf("http://%s/metrics", healthAddr)},
	}
	t.Cleanup(a.Stop)

	a.waitForLiveness()
	return a, nil
}

type externalAgent struct {
	healthAddr, adminAddr string
	agentID               string
	cmd                   *exec.Cmd
	metrics               *metricstest.Tester

	drainOnce sync.Once
	stopOnce  sync.Once
}

func (a *externalAgent) Drain() {
	a.drainOnce.Do(func() {
		if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			log.Fatalf("Error draining agent process: %v", err)
		}
	})
}

func (a *externalAgent) Stop() {
	a.stopOnce.Do(func() {
		if err := a.cmd.Process.Kill(); err != nil {
			log.Fatalf("Error stopping agent process: %v", err)
		}
	})
}

var (
	agentConnectedServerCountMetric = prometheus.BuildFQName(agentmetrics.Namespace, agentmetrics.Subsystem, "open_server_connections")
)

func (a *externalAgent) GetConnectedServerCount() (int, error) {
	return readIntGauge(a.metrics.Endpoint, agentConnectedServerCountMetric)
}

func (a *externalAgent) Ready() bool {
	resp, err := http.Get(fmt.Sprintf("http://%s/readyz", a.healthAddr))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (a *externalAgent) Metrics() metricstest.AgentTester {
	return a.metrics
}

func (a *externalAgent) waitForLiveness() error {
	err := wait.PollImmediate(100*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", a.healthAddr))
		if err != nil {
			return false, nil
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for agent liveness check")
	}
	return nil
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
