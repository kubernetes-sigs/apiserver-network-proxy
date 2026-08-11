/*
Copyright 2026 The Kubernetes Authors.

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

package testing_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	agent "sigs.k8s.io/apiserver-network-proxy/pkg/agent/metrics"
	server "sigs.k8s.io/apiserver-network-proxy/pkg/server/metrics"
	metricstest "sigs.k8s.io/apiserver-network-proxy/pkg/testing/metrics"
)

func TestTLSReloadMetrics(t *testing.T) {
	const (
		serverSuccess = "konnectivity_network_proxy_server_tls_certificate_last_reload_success_timestamp_seconds"
		agentSuccess  = "konnectivity_network_proxy_agent_tls_certificate_last_reload_success_timestamp_seconds"
		cluster       = `{purpose="cluster"}`
		frontend      = `{purpose="frontend"}`
	)
	tester := metricstest.DefaultTester

	server.Metrics.Reset()
	agent.Metrics.Reset()
	t.Cleanup(server.Metrics.Reset)
	t.Cleanup(agent.Metrics.Reset)
	for name := range gatherSamples(t) {
		if strings.Contains(name, "tls_certificate") {
			t.Fatalf("%s is exported before the first reload attempt", name)
		}
	}

	// The first load exports the failure counts at 0.
	start := time.Now().Unix()
	server.Metrics.ObserveTLSReloadSuccess(server.TLSPurposeCluster)
	agent.Metrics.ObserveTLSReloadSuccess()
	end := time.Now().Unix() + 1
	if err := tester.ExpectServerTLSReloadFailures(map[string]int{server.TLSPurposeCluster: 0}); err != nil {
		t.Error(err)
	}
	if err := tester.ExpectAgentTLSReloadFailures(0); err != nil {
		t.Error(err)
	}
	got := gatherSamples(t)
	for _, name := range []string{serverSuccess + cluster, agentSuccess} {
		if got[name] < float64(start) || got[name] >= float64(end) {
			t.Errorf("%s = %v, want a timestamp in [%d, %d)", name, got[name], start, end)
		}
	}

	server.Metrics.ObserveTLSReloadFailure(server.TLSPurposeCluster)
	server.Metrics.ObserveTLSReloadFailure(server.TLSPurposeFrontend)
	server.Metrics.ObserveTLSReloadFailure(server.TLSPurposeFrontend)
	agent.Metrics.ObserveTLSReloadFailure()
	if err := tester.ExpectServerTLSReloadFailures(map[string]int{server.TLSPurposeCluster: 1, server.TLSPurposeFrontend: 2}); err != nil {
		t.Error(err)
	}
	if err := tester.ExpectAgentTLSReloadFailures(1); err != nil {
		t.Error(err)
	}
	if _, ok := gatherSamples(t)[serverSuccess+frontend]; ok {
		t.Error("frontend success recorded after a failed reload")
	}
}

func gatherSamples(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	samples, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{}, families...)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]float64, len(samples))
	for _, sample := range samples {
		values[sample.Metric.String()] = float64(sample.Value)
	}
	return values
}
