package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSingleServer_SingleAgent_StaticCount(t *testing.T) {
	serverServiceHost := "konnectivity-server.kube-system.svc.cluster.local"
	agentServiceHost := "konnectivity-agent.kube-system.svc.cluster.local"
	adminPort := 8093

	serverStatefulSetCfg := StatefulSetConfig{
		Replicas: 1,
		Image:    *serverImage,
		Args: []KeyValue{
			{Key: "log-file", Value: "/var/log/konnectivity-server.log"},
			{Key: "logtostderr", Value: "true"},
			{Key: "log-file-max-size", Value: "0"},
			{Key: "uds-name", Value: "/etc/kubernetes/konnectivity-server/konnectivity-server.socket"},
			{Key: "delete-existing-uds-file"},
			{Key: "cluster-cert", Value: "/etc/kubernetes/pki/apiserver.crt"},
			{Key: "cluster-key", Value: "/etc/kubernetes/pki/apiserver.key"},
			{Key: "server-port", Value: "8090"},
			{Key: "agent-port", Value: "8091"},
			{Key: "health-port", Value: "8092"},
			{Key: "admin-port", Value: strconv.Itoa(adminPort)},
			{Key: "keepalive-time", Value: "1h"},
			{Key: "mode", Value: "grpc"},
			{Key: "agent-namespace", Value: "kube-system"},
			{Key: "agent-service-account", Value: "konnectivity-agent"},
			{Key: "kubeconfig", Value: "/etc/kubernetes/admin.conf"},
			{Key: "authentication-audience", Value: "system:konnectivity-server"},
			{Key: "server-count", Value: "1"},
		},
	}
	serverStatefulSet, _, err := renderServerTemplate("statefulset.yaml", serverStatefulSetCfg)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentStatefulSetConfig := StatefulSetConfig{
		Replicas: 1,
		Image:    *agentImage,
		Args: []KeyValue{
			{Key: "logtostderr", Value: "true"},
			{Key: "ca-cert", Value: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
			{Key: "proxy-server-host", Value: serverServiceHost},
			{Key: "proxy-server-port", Value: "8091"},
			{Key: "sync-interval", Value: "1s"},
			{Key: "sync-interval-cap", Value: "10s"},
			{Key: "sync-forever"},
			{Key: "probe-interval", Value: "1s"},
			{Key: "service-account-token-path", Value: "/var/run/secrets/tokens/konnectivity-agent-token"},
			{Key: "server-count", Value: "1"},
		},
	}
	agentStatefulSet, _, err := renderAgentTemplate("statefulset.yaml", agentStatefulSetConfig)
	if err != nil {
		t.Fatalf("could not render agent deployment: %v", err)
	}

	feature := features.New("konnectivity server and agent stateful set with single replica for each")
	feature.Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()
		err := client.Resources().Create(ctx, serverStatefulSet)
		if err != nil {
			t.Fatalf("could not create server deployment: %v", err)
		}

		err = client.Resources().Create(ctx, agentStatefulSet)
		if err != nil {
			t.Fatalf("could not create agent deployment: %v", err)
		}

		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(agentStatefulSet.GetName(), agentStatefulSet.GetNamespace()),
			wait.WithTimeout(1*time.Minute),
			wait.WithInterval(10*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for agent deployment failed: %v", err)
		}

		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(serverStatefulSet.GetName(), serverStatefulSet.GetNamespace()),
			wait.WithTimeout(1*time.Minute),
			wait.WithInterval(10*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for server deployment failed: %v", err)
		}

		return ctx
	})
	feature.Assess("konnectivity server has a connected client", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		metricsFamilies, err := getMetrics(fmt.Sprintf("%v:%v/metrics", serverServiceHost, adminPort))
		if err != nil {
			t.Fatalf("couldn't get server metrics")
		}
		connectionsMetric, exists := metricsFamilies["konnectivity_network_proxy_server_ready_backend_connections"]
		if !exists {
			t.Fatalf("couldn't find number of ready backend connections in metrics")
		}

		numConnections := int(connectionsMetric.GetMetric()[0].GetGauge().GetValue())
		if numConnections != 1 {
			t.Errorf("incorrect number of connected agents (want: 1, got: %v)", numConnections)
		}

		return ctx
	})
	feature.Assess("konnectivity agent is connected to a server", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		metricsFamilies, err := getMetrics(fmt.Sprintf("%v:%v/metrics", agentServiceHost, adminPort))
		if err != nil {
			t.Fatalf("couldn't get agent metrics")
		}
		connectionsMetric, exists := metricsFamilies["konnectivity_network_proxy_agent_open_server_connections"]
		if !exists {
			t.Fatalf("couldn't find number of open server connections in metrics")
		}

		numConnections := int(connectionsMetric.GetMetric()[0].GetGauge().GetValue())
		if numConnections != 1 {
			t.Errorf("incorrect number of connected agents (want: 1, got: %v)", numConnections)
		}

		return ctx
	})
}

func getMetrics(url string) (map[string]*io_prometheus_client.MetricFamily, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("could not get metrics: %w", err)
	}

	metricsParser := &expfmt.TextParser{}
	metricsFamilies, err := metricsParser.TextToMetricFamilies(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("could not parse metrics: %w", err)
	}

	return metricsFamilies, nil
}
