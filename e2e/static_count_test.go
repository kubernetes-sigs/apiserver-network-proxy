package e2e

import (
	"strconv"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSingleServer_SingleAgent_StaticCount(t *testing.T) {
	adminPort := 8093

	serverDeploymentConfig := DeploymentConfig{
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
	serverDeployment, _, err := renderTemplate("server/deployment.yaml", serverDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentDeploymentConfig := DeploymentConfig{
		Replicas: 1,
		Image:    *agentImage,
		Args: []KeyValue{
			{"logtostderr", "true"},
			{"ca-cert", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
			{"proxy-server-host", "konnectivity-server.kube-system.svc.cluster.local"},
			{"proxy-server-port", "8091"},
			{"sync-interval", "1s"},
			{"sync-interval-cap", "10s"},
			{Key: "sync-forever"},
			{"probe-interval", "1s"},
			{"service-account-token-path", "/var/run/secrets/tokens/konnectivity-agent-token"},
			{"server-count", "1"},
		},
	}
	agentDeployment, _, err := renderTemplate("agent/deployment.yaml", agentDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render agent deployment: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with single replica for each")
	feature.Setup(deployAndWaitForDeployment(serverDeployment))
	feature.Setup(deployAndWaitForDeployment(agentDeployment))
	feature.Assess("konnectivity server has a connected client", assertServersAreConnected(1, adminPort))
	feature.Assess("konnectivity agent is connected to a server", assertAgentsAreConnected(1, adminPort))
}

func TestMultiServer_MultiAgent_StaticCount(t *testing.T) {
	adminPort := 8093
	replicas := 3

	serverDeploymentConfig := DeploymentConfig{
		Replicas: replicas,
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
			{"mode", *connectionMode},
			{"agent-namespace", "kube-system"},
			{"agent-service-account", "konnectivity-agent"},
			{"kubeconfig", "/etc/kubernetes/admin.conf"},
			{"authentication-audience", "system:konnectivity-server"},
			{"server-count", strconv.Itoa(replicas)},
		},
	}
	serverDeployment, _, err := renderTemplate("server/deployment.yaml", serverDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentDeploymentConfig := DeploymentConfig{
		Replicas: replicas,
		Image:    *agentImage,
		Args: []KeyValue{
			{"logtostderr", "true"},
			{"ca-cert", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
			{"proxy-server-host", "konnectivity-server.kube-system.svc.cluster.local"},
			{"proxy-server-port", "8091"},
			{"sync-interval", "1s"},
			{"sync-interval-cap", "10s"},
			{Key: "sync-forever"},
			{"probe-interval", "1s"},
			{"service-account-token-path", "/var/run/secrets/tokens/konnectivity-agent-token"},
			{"server-count", strconv.Itoa(replicas)},
		},
	}
	agentDeployment, _, err := renderTemplate("agent/deployment.yaml", agentDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render agent deployment: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with single replica for each")
	feature.Setup(deployAndWaitForDeployment(serverDeployment))
	feature.Setup(deployAndWaitForDeployment(agentDeployment))
	feature.Assess("all servers connected to all clients", assertServersAreConnected(replicas, adminPort))
	feature.Assess("all agents connected to all servers", assertAgentsAreConnected(replicas, adminPort))
}
