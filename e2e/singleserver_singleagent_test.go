package e2e

import (
	"context"
	"strconv"
	"testing"
	"time"

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
			{"log-file", "/var/log/konnectivity-server.log"},
			{"logtostderr", "true"},
			{"log-file-max-size", "0"},
			{"uds-name", "/etc/kubernetes/konnectivity-server/konnectivity-server.socket"},
			{Key: "delete-existing-uds-file"},
			{"cluster-cert", "/etc/kubernetes/pki/apiserver.crt"},
			{"cluster-key", "/etc/kubernetes/pki/apiserver.key"},
			{"server-port", "8090"},
			{"agent-port", "8091"},
			{"health-port", "8092"},
			{"admin-port", strconv.Itoa(adminPort)},
			{"keepalive-time", "1h"},
			{"mode", "grpc"},
			{"agent-namespace", "kube-system"},
			{"agent-service-account", "konnectivity-agent"},
			{"kubeconfig", "/etc/kubernetes/admin.conf"},
			{"authentication-audience", "system:konnectivity-server"},
			{"server-count", "1"},
		},
	}
	serverStatefulSet, _, err := renderTemplate("server/statefulset.yaml", serverStatefulSetCfg)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentStatefulSetConfig := StatefulSetConfig{
		Replicas: 1,
		Image:    *agentImage,
		Args: []KeyValue{
			{"logtostderr", "true"},
			{"ca-cert", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
			{"proxy-server-host", serverServiceHost},
			{"proxy-server-port", "8091"},
			{"sync-interval", "1s"},
			{"sync-interval-cap", "10s"},
			{Key: "sync-forever"},
			{"probe-interval", "1s"},
			{"service-account-token-path", "/var/run/secrets/tokens/konnectivity-agent-token"},
			{"server-count", "1"},
		},
	}
	agentStatefulSet, _, err := renderTemplate("agent/statefulset.yaml", agentStatefulSetConfig)
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
	feature.Assess("konnectivity server has a connected client", assertServersAreConnected(1, serverServiceHost, adminPort))
	feature.Assess("konnectivity agent is connected to a server", assertAgentsAreConnected(1, agentServiceHost, adminPort))
}
