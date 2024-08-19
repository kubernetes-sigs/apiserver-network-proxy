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
	"fmt"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func renderLeaseCountDeployments(serverReplicas, agentReplicas int) (serverDeployment client.Object, agentDeployment client.Object, err error) {
	serverDeploymentConfig := DeploymentConfig{
		Replicas: serverReplicas,
		Image:    *serverImage,
		Args: []CLIFlag{
			{Flag: "log-file", Value: "/var/log/konnectivity-server.log"},
			{Flag: "logtostderr", Value: "true"},
			{Flag: "log-file-max-size", Value: "0"},
			{Flag: "uds-name", Value: "/etc/kubernetes/konnectivity-server/konnectivity-server.socket"},
			{Flag: "delete-existing-uds-file"},
			{Flag: "cluster-cert", Value: "/etc/kubernetes/pki/apiserver.crt"},
			{Flag: "cluster-key", Value: "/etc/kubernetes/pki/apiserver.key"},
			{Flag: "server-port", Value: "0"},
			{Flag: "kubeconfig", Value: "/etc/kubernetes/admin.conf"},
			{Flag: "keepalive-time", Value: "1h"},
			{Flag: "mode", Value: "grpc"},
			{Flag: "agent-namespace", Value: "kube-system"},
			{Flag: "agent-service-account", Value: "konnectivity-agent"},
			{Flag: "authentication-audience", Value: "system:konnectivity-server"},
			{Flag: "enable-lease-controller"},
			{Flag: "admin-bind-address", EmptyValue: true},
		},
	}
	serverDeployment, _, err = renderTemplate("server/deployment.yaml", serverDeploymentConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not render server deployment: %w", err)
	}

	agentDeploymentConfig := DeploymentConfig{
		Replicas: agentReplicas,
		Image:    *agentImage,
		Args: []CLIFlag{
			{Flag: "logtostderr", Value: "true"},
			{Flag: "ca-cert", Value: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"},
			{Flag: "proxy-server-host", Value: "konnectivity-server.kube-system.svc.cluster.local"},
			{Flag: "proxy-server-port", Value: "8091"},
			{Flag: "sync-interval", Value: "1s"},
			{Flag: "sync-interval-cap", Value: "10s"},
			{Flag: "sync-forever"},
			{Flag: "probe-interval", Value: "1s"},
			{Flag: "service-account-token-path", Value: "/var/run/secrets/tokens/konnectivity-agent-token"},
			{Flag: "count-server-leases"},
			{Flag: "agent-identifiers", Value: "ipv4=${HOST_IP}"},
			{Flag: "admin-bind-address", EmptyValue: true},
		},
	}
	agentDeployment, _, err = renderTemplate("agent/deployment.yaml", agentDeploymentConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("could not render agent deployment: %w", err)
	}

	return serverDeployment, agentDeployment, nil
}

func TestSingleServer_SingleAgent_LeaseCount(t *testing.T) {
	serverDeployment, agentDeployment, err := renderLeaseCountDeployments(1, 1)
	if err != nil {
		t.Fatalf("could not render lease count deployments: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with single replica for each")
	feature = feature.Setup(createDeployment(agentDeployment))
	feature = feature.Setup(createDeployment(serverDeployment))
	feature = feature.Setup(waitForDeployment(serverDeployment))
	feature = feature.Setup(waitForDeployment(agentDeployment))
	feature = feature.Assess("konnectivity server has a connected client", assertServersAreConnected(1))
	feature = feature.Assess("konnectivity agent is connected to a server", assertAgentsAreConnected(1))
	feature = feature.Assess("agent correctly counts 1 lease", assertAgentKnownServerCount(1))
	feature = feature.Teardown(deleteDeployment(agentDeployment))
	feature = feature.Teardown(deleteDeployment(serverDeployment))

	testenv.Test(t, feature.Feature())
}

func TestMultiServer_MultiAgent_LeaseCount(t *testing.T) {
	serverDeployment, agentDeployment, err := renderLeaseCountDeployments(2, 2)
	if err != nil {
		t.Fatalf("could not render lease count deployments: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with multiple replicas")
	feature = feature.Setup(createDeployment(serverDeployment))
	feature = feature.Setup(createDeployment(agentDeployment))
	feature = feature.Setup(waitForDeployment(serverDeployment))
	feature = feature.Setup(waitForDeployment(agentDeployment))
	feature = feature.Setup(scaleDeployment(serverDeployment, 4))
	feature = feature.Setup(scaleDeployment(agentDeployment, 4))
	feature = feature.Setup(waitForDeployment(agentDeployment))
	feature = feature.Setup(waitForDeployment(serverDeployment))
	feature = feature.Assess("all servers connected to all clients after scale up", assertServersAreConnected(4))
	feature = feature.Assess("all agents connected to all servers after scale up", assertAgentsAreConnected(4))
	feature = feature.Assess("agents correctly count 4 leases after scale up", assertAgentKnownServerCount(4))
	feature = feature.Teardown(deleteDeployment(agentDeployment))
	feature = feature.Teardown(deleteDeployment(serverDeployment))

	testenv.Test(t, feature.Feature())
}
