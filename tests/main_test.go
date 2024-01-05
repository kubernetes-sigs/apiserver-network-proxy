/*
Copyright 2022 The Kubernetes Authors.

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

package tests

import (
	"flag"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"

	metricsclient "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client/metrics"
	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

var (
	agentPath = flag.String("agent-path", "", "Path to the agent binary to test against, or empty to run the in-process agent.")
)

var Framework = framework.Framework{
	ProxyServerRunner: &framework.InProcessProxyServerRunner{},
}

func TestMain(m *testing.M) {
	fs := flag.NewFlagSet("mock-flags", flag.PanicOnError)
	klog.InitFlags(fs)
	fs.Set("v", "1") // Set klog verbosity.
	metricsclient.Metrics.RegisterMetrics(prometheus.DefaultRegisterer)

	flag.Parse()
	if err := initFramework(); err != nil {
		log.Fatalf("Failed to initialize framework: %v", err)
	}

	if err := framework.InitCertsDir(); err != nil {
		log.Fatalf("Failed to write test certs: %v", err)
	}
	defer func() {
		os.RemoveAll(framework.CertsDir)
	}()

	m.Run()
}

func initFramework() error {
	if *agentPath == "" {
		log.Print("Running tests with in-process agent")
		Framework.AgentRunner = &framework.InProcessAgentRunner{}
	} else {
		info, err := os.Stat(*agentPath)
		if err != nil {
			return err
		}
		if info.Mode()&0111 == 0 { // Path must be executable
			return fmt.Errorf("%s is not an executable file", *agentPath)
		}

		log.Printf("Running tests with external agent %s", *agentPath)
		Framework.AgentRunner = &framework.ExternalAgentRunner{
			ExecutablePath: *agentPath,
		}
	}

	return nil
}
