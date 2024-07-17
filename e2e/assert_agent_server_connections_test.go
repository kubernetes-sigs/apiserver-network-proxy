package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/prometheus/common/expfmt"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

func getMetricsGaugeValue(url string, name string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, fmt.Errorf("could not get metrics: %w", err)
	}

	metricsParser := &expfmt.TextParser{}
	metricsFamilies, err := metricsParser.TextToMetricFamilies(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return 0, fmt.Errorf("could not parse metrics: %w", err)
	}

	metricFamily, exists := metricsFamilies[name]
	if !exists {
		return 0, fmt.Errorf("metric %v does not exist", name)
	}
	value := int(metricFamily.GetMetric()[0].GetGauge().GetValue())
	return value, nil
}

func assertAgentsAreConnected(expectedConnections int, serviceHost string, adminPort int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		var agentPods *corev1.PodList
		err := client.Resources().List(ctx, agentPods, resources.WithLabelSelector("k8s-app=konnectivity-agent"))
		if err != nil {
			t.Fatalf("couldn't get agent pods: %v", err)
		}

		for _, agentPod := range agentPods.Items {
			numConnections, err := getMetricsGaugeValue(fmt.Sprintf("%v-%v:%v/metrics", agentPod.Name, serviceHost, adminPort), "konnectivity_network_proxy_agent_open_server_connections")
			if err != nil {
				t.Fatalf("couldn't get agent metric 'konnectivity_network_proxy_agent_open_server_connections' for pod %v", agentPod.Name)
			}

			if numConnections != expectedConnections {
				t.Errorf("incorrect number of connected servers (want: %v, got: %v)", expectedConnections, numConnections)
			}
		}

		return ctx
	}
}

func assertServersAreConnected(expectedConnections int, serviceHost string, adminPort int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		var serverPods *corev1.PodList
		err := client.Resources().List(ctx, serverPods, resources.WithLabelSelector("k8s-app=konnectivity-server"))
		if err != nil {
			t.Fatalf("couldn't get server pods: %v", err)
		}

		for _, serverPod := range serverPods.Items {
			numConnections, err := getMetricsGaugeValue(fmt.Sprintf("%v-%v:%v/metrics", serverPod.Name, serviceHost, adminPort), "konnectivity_network_proxy_server_ready_backend_connections")
			if err != nil {
				t.Fatalf("couldn't get agent metric 'konnectivity_network_proxy_server_ready_backend_connections' for pod %v", serverPod.Name)
			}

			if numConnections != expectedConnections {
				t.Errorf("incorrect number of connected agents (want: %v, got: %v)", expectedConnections, numConnections)
			}
		}

		return ctx
	}
}
