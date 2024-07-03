package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSingleServer_SingleAgent_StaticCount(t *testing.T) {
	serviceHost := "konnectivity-server.kube-system.svc.cluster.local"
	adminPort := 8093

	serverDeploymentCfg := DeploymentConfig{
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
	serverDeployment, _, err := renderServerTemplate("deployment.yaml", serverDeploymentCfg)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentDeploymentCfg := DeploymentConfig{
		Replicas: 1,
		Image:    *agentImage,
		Args: []KeyValue{
			{Key: "logtostderr", Value: "true"},
			{Key: "ca-cert", Value: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
			{Key: "proxy-server-host", Value: serviceHost},
			{Key: "proxy-server-port", Value: "8091"},
			{Key: "sync-interval", Value: "1s"},
			{Key: "sync-interval-cap", Value: "10s"},
			{Key: "sync-forever"},
			{Key: "probe-interval", Value: "1s"},
			{Key: "service-account-token-path", Value: "/var/run/secrets/tokens/konnectivity-agent-token"},
			{Key: "server-count", Value: "1"},
		},
	}
	agentDeployment, _, err := renderAgentTemplate("deployment.yaml", agentDeploymentCfg)
	if err != nil {
		t.Fatalf("could not render agent deployment: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with single replica for each")
	feature.Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()
		err := client.Resources().Create(ctx, serverDeployment)
		if err != nil {
			t.Fatalf("could not create server deployment: %v", err)
		}

		err = client.Resources().Create(ctx, agentDeployment)
		if err != nil {
			t.Fatalf("could not create agent deployment: %v", err)
		}

		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(agentDeployment.GetName(), agentDeployment.GetNamespace()),
			wait.WithTimeout(1*time.Minute),
			wait.WithInterval(10*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for agent deployment failed: %v", err)
		}

		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(serverDeployment.GetName(), serverDeployment.GetNamespace()),
			wait.WithTimeout(1*time.Minute),
			wait.WithInterval(10*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for server deployment failed: %v", err)
		}

		return ctx
	})
	feature.Assess("konnectivity server has a connected client", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		resp, err := http.Get(fmt.Sprintf("%v:%v/metrics", serviceHost, adminPort))
		if err != nil {
			t.Fatalf("could not read server metrics: %v", err)
		}

		metricsParser := &expfmt.TextParser{}
		metricsFamilies, err := metricsParser.TextToMetricFamilies(resp.Body)
		defer resp.Body.Close()
		if err != nil {
			t.Fatalf("could not parse server metrics: %v", err)
		}

		connectionsMetric, exists := metricsFamilies["konnectivity_network_proxy_server_ready_backend_connections"]
		if !exists {
			t.Fatalf("couldn't find number of ready backend connections in metrics: %v", metricsFamilies)
		}

		numConnections := int(connectionsMetric.GetMetric()[0].GetGauge().GetValue())
		if numConnections != 1 {
			t.Errorf("incorrect number of connected agents (want: 1, got: %v)", numConnections)
		}

		return ctx
	})
}
