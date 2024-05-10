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

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	serverapp "sigs.k8s.io/apiserver-network-proxy/cmd/server/app"
	serveropts "sigs.k8s.io/apiserver-network-proxy/cmd/server/app/options"
	"sigs.k8s.io/apiserver-network-proxy/pkg/server"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
)

type ProxyServerOpts struct {
	ServerCount int
	Mode        string
	AgentPort   int // Defaults to random port.
}

type ProxyServerRunner interface {
	Start(testing.TB, ProxyServerOpts) (ProxyServer, error)
}

type ProxyServer interface {
	ConnectedBackends() int
	AgentAddr() string
	FrontAddr() string
	Ready() bool
	Stop()
	Metrics() metricstest.ServerTester
}

type InProcessProxyServerRunner struct{}

func (*InProcessProxyServerRunner) Start(t testing.TB, opts ProxyServerOpts) (ProxyServer, error) {
	s := &serverapp.Proxy{}
	o, err := serverOptions(t, opts)
	if err != nil {
		return nil, fmt.Errorf("error building server options: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopCh := make(chan struct{})
	go func() {
		if err := s.Run(o, stopCh); err != nil {
			log.Printf("ERROR running proxy server: %v", err)
			cancel()
		}
	}()

	healthAddr := net.JoinHostPort(o.HealthBindAddress, strconv.Itoa(o.HealthPort))
	if err := wait.PollImmediateWithContext(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, func(context.Context) (bool, error) {
		return checkLiveness(healthAddr), nil
	}); err != nil {
		close(stopCh)
		return nil, fmt.Errorf("server never came up: %v", err)
	}

	ps := &inProcessProxyServer{
		proxyServer: s.ProxyServer(),
		stopCh:      stopCh,
		mode:        opts.Mode,
		healthAddr:  healthAddr,
		agentAddr:   net.JoinHostPort(o.AgentBindAddress, strconv.Itoa(o.AgentPort)),
		frontAddr:   o.UdsName,
	}
	t.Cleanup(ps.Stop)
	return ps, nil
}

type inProcessProxyServer struct {
	proxyServer *server.ProxyServer

	stopOnce sync.Once
	stopCh   chan struct{}

	mode string

	healthAddr string
	agentAddr  string
	frontAddr  string
}

func (ps *inProcessProxyServer) Stop() {
	ps.stopOnce.Do(func() {
		close(ps.stopCh)
	})
}

func (ps *inProcessProxyServer) AgentAddr() string {
	return ps.agentAddr
}

func (ps *inProcessProxyServer) FrontAddr() string {
	return ps.frontAddr
}

func (ps *inProcessProxyServer) ConnectedBackends() int {
	return ps.proxyServer.BackendManager.NumBackends()
}

func (ps *inProcessProxyServer) Ready() bool {
	return checkReadiness(ps.healthAddr)
}

func (ps *inProcessProxyServer) Metrics() metricstest.ServerTester {
	return metricstest.DefaultTester
}

func serverOptions(t testing.TB, opts ProxyServerOpts) (*serveropts.ProxyRunOptions, error) {
	t.Helper()
	o := serveropts.NewProxyRunOptions()

	o.ServerCount = uint(opts.ServerCount)
	o.Mode = opts.Mode

	uid := uuid.New().String()
	o.UdsName = filepath.Join(CertsDir, fmt.Sprintf("server-%s.sock", uid))
	o.ServerPort = 0 // Required for UDS

	o.ClusterCert = filepath.Join(CertsDir, TestServerCertFile)
	o.ClusterKey = filepath.Join(CertsDir, TestServerKeyFile)
	o.ClusterCaCert = filepath.Join(CertsDir, TestCAFile)

	const localhost = "127.0.0.1"
	o.AgentBindAddress = localhost
	o.HealthBindAddress = localhost
	o.AdminBindAddress = localhost

	ports, err := FreePorts(3)
	if err != nil {
		return nil, err
	}
	if opts.AgentPort != 0 {
		o.AgentPort = opts.AgentPort
	} else {
		o.AgentPort = ports[0]
	}
	o.HealthPort = ports[1]
	o.AdminPort = ports[2]

	return o, nil
}
