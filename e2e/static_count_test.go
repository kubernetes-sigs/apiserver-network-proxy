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
	"strconv"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func renderStaticCountDeployments(serverReplicas, agentReplicas int) (serverDeployment client.Object, agentDeployment client.Object, err error) {
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
			{Flag: "admin-bind-address", EmptyValue: true},
			{Flag: "server-count", Value: strconv.Itoa(serverReplicas)},
			{Flag: "mode", Value: *connectionMode},
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

func TestSingleServer_SingleAgent_StaticCount(t *testing.T) {
	serverDeployment, agentDeployment, err := renderStaticCountDeployments(1, 1)
	if err != nil {
		t.Fatalf("could not render static count deployments: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with single replica for each")
	feature = feature.Setup(createDeployment(agentDeployment))
	feature = feature.Setup(createDeployment(serverDeployment))
	feature = feature.Setup(waitForDeployment(agentDeployment))
	feature = feature.Setup(waitForDeployment(serverDeployment))
	feature = feature.Assess("konnectivity server has a connected client", assertServersAreConnected(1))
	feature = feature.Assess("konnectivity agent is connected to a server", assertAgentsAreConnected(1))
	feature = feature.Assess("agents correctly count 1 server", assertAgentKnownServerCount(1))
	feature = feature.Teardown(deleteDeployment(agentDeployment))
	feature = feature.Teardown(deleteDeployment(serverDeployment))

	testenv.Test(t, feature.Feature())
}

func TestMultiServer_MultiAgent_StaticCount(t *testing.T) {
	replicas := 3
	serverDeployment, agentDeployment, err := renderStaticCountDeployments(replicas, replicas)
	if err != nil {
		t.Fatalf("could not render static count deployments: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with multiple replicas")
	feature = feature.Setup(createDeployment(agentDeployment))
	feature = feature.Setup(createDeployment(serverDeployment))
	feature = feature.Setup(waitForDeployment(agentDeployment))
	feature = feature.Setup(waitForDeployment(serverDeployment))
	feature = feature.Assess("all servers connected to all clients", assertServersAreConnected(replicas))
	feature = feature.Assess("all agents connected to all servers", assertAgentsAreConnected(replicas))
	feature = feature.Assess("agents correctly count all servers", assertAgentKnownServerCount(replicas))
	feature = feature.Teardown(deleteDeployment(agentDeployment))
	feature = feature.Teardown(deleteDeployment(serverDeployment))

	testenv.Test(t, feature.Feature())
}
