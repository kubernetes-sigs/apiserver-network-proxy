/*
Copyright 2024 The Kubernetes Authors.

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
package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

const (
	// The agent serves its metrics from the health server; 8094 is its admin
	// server. The server serves metrics from its admin server.
	agentHealthPort = 8093
	serverAdminPort = 8095

	agentSelector  = "k8s-app=konnectivity-agent"
	serverSelector = "k8s-app=konnectivity-server"

	agentOpenServerConnectionsMetric    = "konnectivity_network_proxy_agent_open_server_connections"
	agentKnownServerCountMetric         = "konnectivity_network_proxy_agent_known_server_count"
	serverReadyBackendConnectionsMetric = "konnectivity_network_proxy_server_ready_backend_connections"
)

func getMetricsGaugeValue(ctx context.Context, restCfg *rest.Config, namespace string, podName string, adminPort int, metricName string) (int, error) {
	client, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return 0, fmt.Errorf("could not create http client from RESTConfig: %w", err)
	}
	url := fmt.Sprintf(
		"%v/api/v1/namespaces/%v/pods/%v:%v/proxy/metrics",
		restCfg.Host,
		namespace,
		podName,
		adminPort,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("could not create metrics request for URL %q: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not get metrics from URL %q: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("got invalid response from metrics endpoint %q, status code %v: %v", url, resp.StatusCode, string(body))
	}

	metricsParser := expfmt.NewTextParser(model.LegacyValidation)
	metricsFamilies, err := metricsParser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("could not parse metrics: %w", err)
	}

	metricFamily, exists := metricsFamilies[metricName]
	if !exists {
		return 0, fmt.Errorf("metric %v does not exist", metricName)
	}
	value := metricFamily.GetMetric()[0].GetGauge().GetValue()
	fmt.Printf("int: %v, float64: %v\n", int(value), value)
	return int(value), nil
}

func assertAgentsAreConnected(expectedConnections int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		agentPods := &corev1.PodList{}
		err := client.Resources().List(ctx, agentPods, resources.WithLabelSelector(agentSelector))
		if err != nil {
			t.Fatalf("couldn't get agent pods (label selector %q): %v", agentSelector, err)
		}

		for _, agentPod := range agentPods.Items {
			numConnections, err := getMetricsGaugeValue(ctx, cfg.Client().RESTConfig(), agentPod.Namespace, agentPod.Name, agentHealthPort, agentOpenServerConnectionsMetric)
			if err != nil {
				t.Fatalf("couldn't get agent metric %q for pod %v: %v", agentOpenServerConnectionsMetric, agentPod.Name, err)
			}

			if numConnections != expectedConnections {
				t.Errorf("incorrect number of connected servers (want: %d, got: %d)", expectedConnections, numConnections)
			}
		}

		return ctx
	}
}

func assertServersAreConnected(expectedConnections int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		serverPods := &corev1.PodList{}
		err := client.Resources().List(ctx, serverPods, resources.WithLabelSelector(serverSelector))
		if err != nil {
			t.Fatalf("couldn't get server pods (label selector %q): %v", serverSelector, err)
		}

		for _, serverPod := range serverPods.Items {
			numConnections, err := getMetricsGaugeValue(ctx, cfg.Client().RESTConfig(), serverPod.Namespace, serverPod.Name, serverAdminPort, serverReadyBackendConnectionsMetric)
			if err != nil {
				t.Fatalf("couldn't get server metric %q for pod %v: %v", serverReadyBackendConnectionsMetric, serverPod.Name, err)
			}

			if numConnections != expectedConnections {
				t.Errorf("incorrect number of connected agents (want: %d, got: %d)", expectedConnections, numConnections)
			}
		}

		return ctx
	}
}

func assertAgentKnownServerCount(expectedServerCount int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		agentPods := &corev1.PodList{}
		err := client.Resources().List(ctx, agentPods, resources.WithLabelSelector(agentSelector))
		if err != nil {
			t.Fatalf("couldn't get server pods: %v", err)
		}

		for _, agentPod := range agentPods.Items {
			knownServerCount, err := getMetricsGaugeValue(ctx, cfg.Client().RESTConfig(), agentPod.Namespace, agentPod.Name, agentHealthPort, agentKnownServerCountMetric)
			if err != nil {
				t.Fatalf("couldn't get agent metric %q for pod %v", agentKnownServerCountMetric, agentPod.Name)
			}

			if knownServerCount != expectedServerCount {
				t.Errorf("incorrect known server count (want: %v, got: %v)", expectedServerCount, knownServerCount)
			}
		}

		return ctx
	}
}
