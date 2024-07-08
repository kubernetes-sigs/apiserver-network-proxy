package e2e

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestMultiServer_MultiAgent_StaticCount(t *testing.T) {
	serverServiceHost := "konnectivity-server.kube-system.svc.cluster.local"
	agentServiceHost := "konnectivity-agent.kube-system.svc.cluster.local"
	adminPort := 8093
	replicas := 3

	serverStatefulSetCfg := StatefulSetConfig{
		Replicas: 3,
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
		Replicas: 3,
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
			{Key: "server-count", Value: "3"},
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
	feature.Assess("all servers connected to all clients", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		var serverPods *corev1.PodList
		err := client.Resources().List(ctx, serverPods, resources.WithLabelSelector("k8s-app=konnectivity-server"))
		if err != nil {
			t.Fatalf("couldn't get server pods: %v", err)
		}

		for _, serverPod := range serverPods.Items {

			metricsFamilies, err := getMetrics(fmt.Sprintf("%v-%v:%v/metrics", serverPod.Name, serverServiceHost, adminPort))
			if err != nil {
				t.Fatalf("couldn't get server metrics for pod %v", serverPod.Name)
			}
			connectionsMetric, exists := metricsFamilies["konnectivity_network_proxy_server_ready_backend_connections"]
			if !exists {
				t.Fatalf("couldn't find number of ready backend connections in metrics")
			}

			numConnections := int(connectionsMetric.GetMetric()[0].GetGauge().GetValue())
			if numConnections != replicas {
				t.Errorf("incorrect number of connected agents (want: %v, got: %v)", replicas, numConnections)
			}
		}

		return ctx
	})
	feature.Assess("all agents connected to all servers", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()

		var agentPods *corev1.PodList
		err := client.Resources().List(ctx, agentPods, resources.WithLabelSelector("k8s-app=konnectivity-agent"))
		if err != nil {
			t.Fatalf("couldn't get agent pods: %v", err)
		}

		for _, agentPod := range agentPods.Items {

			metricsFamilies, err := getMetrics(fmt.Sprintf("%v-%v:%v/metrics", agentPod.Name, agentServiceHost, adminPort))
			if err != nil {
				t.Fatalf("couldn't get agent metrics for pod %v", agentPod.Name)
			}
			connectionsMetric, exists := metricsFamilies["konnectivity_network_proxy_agent_open_server_connections"]
			if !exists {
				t.Fatalf("couldn't find number of ready server connections in metrics")
			}

			numConnections := int(connectionsMetric.GetMetric()[0].GetGauge().GetValue())
			if numConnections != replicas {
				t.Errorf("incorrect number of connected servers (want: %v, got: %v)", replicas, numConnections)
			}
		}

		return ctx
	})
}
