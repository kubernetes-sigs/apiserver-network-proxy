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

package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	httpstreamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	certutil "k8s.io/client-go/util/cert"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"sigs.k8s.io/apiserver-network-proxy/tests/framework"
)

const (
	tlsRotationNamespace   = "kube-system"
	tlsRotationServerName  = "konnectivity-server.kube-system.svc.cluster.local"
	frontendTLSSecretName  = "konnectivity-frontend-tls-rotation"
	clusterTLSSecretName   = "konnectivity-cluster-tls-rotation"
	agentTLSSecretName     = "konnectivity-agent-tls-rotation"
	frontendTLSMountPath   = "/var/run/konnectivity/frontend-tls"
	clusterTLSMountPath    = "/var/run/konnectivity/cluster-tls"
	agentTLSMountPath      = "/var/run/konnectivity/agent-tls"
	tlsRotationWaitTimeout = 2 * time.Minute

	tlsRotationServerReplicas = 2
	tlsRotationAgentReplicas  = 1
	tlsRotationFrontendPort   = 8090
	tlsRotationAgentPort      = 8091
)

// TestTLSCertificateRotation is a smoke test: it verifies that a certificate
// update to a Secret reaches the proxy server through the projected volume and
// is served without a restart. The semantics of a staged CA rotation are
// covered by the in-process integration test in tests/tls_rotation_test.go.
func TestTLSCertificateRotation(t *testing.T) {
	material := newTLSRotationMaterial(t)
	frontendSecret := newTLSRotationSecret(frontendTLSSecretName, material.ca.PEM, material.frontendOld)
	clusterSecret := newTLSRotationSecret(clusterTLSSecretName, material.ca.PEM, material.clusterOld)
	agentSecret := newTLSRotationSecret(agentTLSSecretName, material.ca.PEM, material.agent)

	serverDeployment, _, err := renderTemplate("server/deployment.yaml", DeploymentConfig{
		Replicas: tlsRotationServerReplicas,
		Image:    *serverImage,
		Args: []CLIFlag{
			{Flag: "logtostderr", Value: "true"},
			{Flag: "server-ca-cert", Value: frontendTLSMountPath + "/ca.crt"},
			{Flag: "server-cert", Value: frontendTLSMountPath + "/tls.crt"},
			{Flag: "server-key", Value: frontendTLSMountPath + "/tls.key"},
			{Flag: "cluster-ca-cert", Value: clusterTLSMountPath + "/ca.crt"},
			{Flag: "cluster-cert", Value: clusterTLSMountPath + "/tls.crt"},
			{Flag: "cluster-key", Value: clusterTLSMountPath + "/tls.key"},
			{Flag: "enable-tls-certificate-reload"},
			{Flag: "server-count", Value: strconv.Itoa(tlsRotationServerReplicas)},
			{Flag: "mode", Value: *connectionMode},
			{Flag: "admin-bind-address", EmptyValue: true},
		},
		SecretVolumes: []SecretVolumeConfig{
			{Name: "frontend-tls", SecretName: frontendTLSSecretName, MountPath: frontendTLSMountPath},
			{Name: "cluster-tls", SecretName: clusterTLSSecretName, MountPath: clusterTLSMountPath},
		},
	})
	if err != nil {
		t.Fatalf("could not render TLS rotation server deployment: %v", err)
	}

	agentDeployment, _, err := renderTemplate("agent/deployment.yaml", DeploymentConfig{
		Replicas: tlsRotationAgentReplicas,
		Image:    *agentImage,
		Args: []CLIFlag{
			{Flag: "logtostderr", Value: "true"},
			{Flag: "ca-cert", Value: agentTLSMountPath + "/ca.crt"},
			{Flag: "agent-cert", Value: agentTLSMountPath + "/tls.crt"},
			{Flag: "agent-key", Value: agentTLSMountPath + "/tls.key"},
			{Flag: "enable-tls-certificate-reload"},
			{Flag: "proxy-server-host", Value: tlsRotationServerName},
			{Flag: "proxy-server-port", Value: strconv.Itoa(tlsRotationAgentPort)},
			{Flag: "sync-interval", Value: "1s"},
			{Flag: "sync-interval-cap", Value: "10s"},
			{Flag: "sync-forever"},
			{Flag: "probe-interval", Value: "1s"},
			{Flag: "agent-identifiers", Value: "ipv4=${HOST_IP}"},
			{Flag: "admin-bind-address", EmptyValue: true},
		},
		SecretVolumes: []SecretVolumeConfig{
			{Name: "agent-tls", SecretName: agentTLSSecretName, MountPath: agentTLSMountPath},
		},
	})
	if err != nil {
		t.Fatalf("could not render TLS rotation agent deployment: %v", err)
	}

	feature := features.New("server reloads rotated TLS certificates")
	feature = feature.Setup(createTLSRotationSecret(frontendSecret))
	feature = feature.Setup(createTLSRotationSecret(clusterSecret))
	feature = feature.Setup(createTLSRotationSecret(agentSecret))
	feature = feature.Setup(createDeployment(serverDeployment))
	feature = feature.Setup(createDeployment(agentDeployment))
	feature = feature.Setup(waitForDeployment(serverDeployment))
	feature = feature.Setup(waitForDeployment(agentDeployment))
	feature = feature.Assess("TLS material rotates without restarting the workloads", assessTLSRotation(material))
	feature = feature.Teardown(deleteDeployment(agentDeployment))
	feature = feature.Teardown(deleteDeployment(serverDeployment))
	feature = feature.Teardown(deleteTLSRotationSecret(agentSecret))
	feature = feature.Teardown(deleteTLSRotationSecret(clusterSecret))
	feature = feature.Teardown(deleteTLSRotationSecret(frontendSecret))

	testenv.Test(t, feature.Feature())
}

func assessTLSRotation(material tlsRotationMaterial) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		parentCtx := ctx
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		restConfig := cfg.Client().RESTConfig()
		clientset := kubernetes.NewForConfigOrDie(restConfig)
		// The gRPC frontend advertises h2 and the http-connect frontend
		// advertises http/1.1.
		frontendALPN := "http/1.1"
		if *connectionMode == "grpc" {
			frontendALPN = "h2"
		}

		serverState := snapshotPodState(ctx, t, clientset, serverSelector, tlsRotationServerReplicas)
		agentState := snapshotPodState(ctx, t, clientset, agentSelector, tlsRotationAgentReplicas)

		waitForAgentConnections := func(expected int) {
			waitForGaugeValue(ctx, t, restConfig, clientset, agentSelector, tlsRotationAgentReplicas, agentHealthPort, agentOpenServerConnectionsMetric, expected)
		}

		waitForAgentConnections(2)
		waitForServerTLS(ctx, t, restConfig, clientset, tlsRotationFrontendPort, material.ca.PEM, material.frontendClient, frontendALPN, "frontend-old")
		waitForServerTLS(ctx, t, restConfig, clientset, tlsRotationAgentPort, material.ca.PEM, material.agent, "h2", "cluster-old")

		// Rotate both serving identities under the same CA. The servers must
		// pick them up through the projected Secret volumes without a restart.
		updateTLSRotationSecret(ctx, t, clientset, frontendTLSSecretName, material.ca.PEM, material.frontendNew)
		updateTLSRotationSecret(ctx, t, clientset, clusterTLSSecretName, material.ca.PEM, material.clusterNew)
		waitForServerTLS(ctx, t, restConfig, clientset, tlsRotationFrontendPort, material.ca.PEM, material.frontendClient, frontendALPN, "frontend-new")
		waitForServerTLS(ctx, t, restConfig, clientset, tlsRotationAgentPort, material.ca.PEM, material.agent, "h2", "cluster-new")

		waitForAgentConnections(2)
		assertPodStateUnchanged(ctx, t, clientset, serverSelector, serverState)
		assertPodStateUnchanged(ctx, t, clientset, agentSelector, agentState)
		return parentCtx
	}
}

type tlsRotationMaterial struct {
	ca             *framework.TestCA
	frontendOld    tlsKeyPair
	frontendNew    tlsKeyPair
	clusterOld     tlsKeyPair
	clusterNew     tlsKeyPair
	frontendClient tlsKeyPair
	agent          tlsKeyPair
}

type tlsKeyPair struct {
	cert []byte
	key  []byte
}

func newTLSRotationMaterial(t *testing.T) tlsRotationMaterial {
	t.Helper()
	ca := framework.NewTestCA(t, "rotation-ca")
	issueServing := func(commonName string) tlsKeyPair {
		cert, key := ca.IssueServing(t, commonName, tlsRotationServerName)
		return tlsKeyPair{cert: cert, key: key}
	}
	issueClient := func(commonName string) tlsKeyPair {
		cert, key := ca.IssueClient(t, commonName)
		return tlsKeyPair{cert: cert, key: key}
	}
	return tlsRotationMaterial{
		ca:             ca,
		frontendOld:    issueServing("frontend-old"),
		frontendNew:    issueServing("frontend-new"),
		clusterOld:     issueServing("cluster-old"),
		clusterNew:     issueServing("cluster-new"),
		frontendClient: issueClient("frontend-client"),
		agent:          issueClient("agent"),
	}
}

func newTLSRotationSecret(name string, ca []byte, pair tlsKeyPair) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tlsRotationNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       tlsRotationSecretData(ca, pair),
	}
}

func tlsRotationSecretData(ca []byte, pair tlsKeyPair) map[string][]byte {
	return map[string][]byte{
		"ca.crt":                append([]byte(nil), ca...),
		corev1.TLSCertKey:       append([]byte(nil), pair.cert...),
		corev1.TLSPrivateKeyKey: append([]byte(nil), pair.key...),
	}
}

func createTLSRotationSecret(secret *corev1.Secret) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		clientset := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
		if _, err := clientset.CoreV1().Secrets(secret.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			t.Fatalf("could not create TLS rotation Secret %q: %v", secret.Name, err)
		}
		return ctx
	}
}

func deleteTLSRotationSecret(secret *corev1.Secret) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		clientset := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
		if err := clientset.CoreV1().Secrets(secret.Namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil {
			t.Errorf("could not delete TLS rotation Secret %q: %v", secret.Name, err)
		}
		return ctx
	}
}

func updateTLSRotationSecret(ctx context.Context, t *testing.T, clientset kubernetes.Interface, name string, ca []byte, pair tlsKeyPair) {
	t.Helper()
	secret, err := clientset.CoreV1().Secrets(tlsRotationNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("could not get TLS rotation Secret %q: %v", name, err)
	}
	secret.Data = tlsRotationSecretData(ca, pair)
	if _, err := clientset.CoreV1().Secrets(tlsRotationNamespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("could not update TLS rotation Secret %q: %v", name, err)
	}
}

type podState struct {
	uid          types.UID
	restartCount int32
}

func snapshotPodState(ctx context.Context, t *testing.T, clientset kubernetes.Interface, selector string, expectedCount int) map[string]podState {
	t.Helper()
	pods := waitForReadyPods(ctx, t, clientset, selector, expectedCount)
	result := make(map[string]podState, len(pods))
	for _, pod := range pods {
		result[pod.Name] = podState{uid: pod.UID, restartCount: podRestartCount(pod)}
	}
	return result
}

func assertPodStateUnchanged(ctx context.Context, t *testing.T, clientset kubernetes.Interface, selector string, expected map[string]podState) {
	t.Helper()
	pods := waitForReadyPods(ctx, t, clientset, selector, len(expected))
	if len(pods) != len(expected) {
		t.Fatalf("pod count for %q = %d, want %d", selector, len(pods), len(expected))
	}
	for _, pod := range pods {
		state, ok := expected[pod.Name]
		if !ok {
			t.Fatalf("pod %q for %q was replaced", pod.Name, selector)
		}
		if pod.UID != state.uid || podRestartCount(pod) != state.restartCount {
			t.Fatalf("pod %q changed during TLS rotation: uid=%q restarts=%d, want uid=%q restarts=%d", pod.Name, pod.UID, podRestartCount(pod), state.uid, state.restartCount)
		}
	}
}

func podRestartCount(pod corev1.Pod) int32 {
	var restarts int32
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}
	return restarts
}

func waitForReadyPods(ctx context.Context, t *testing.T, clientset kubernetes.Interface, selector string, expectedCount int) []corev1.Pod {
	t.Helper()
	var ready []corev1.Pod
	err := utilwait.PollUntilContextTimeout(ctx, time.Second, tlsRotationWaitTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(tlsRotationNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, nil
		}
		if len(pods.Items) != expectedCount {
			return false, nil
		}
		ready = ready[:0]
		for _, pod := range pods.Items {
			if isPodReady(&pod) {
				ready = append(ready, pod)
			}
		}
		return len(ready) == expectedCount, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for %d ready pods matching %q: %v", expectedCount, selector, err)
	}
	return ready
}

// waitForServerTLS waits until every server pod presents expectedCommonName to
// clientPair on port and negotiates expectedALPN.
func waitForServerTLS(ctx context.Context, t *testing.T, restConfig *rest.Config, clientset kubernetes.Interface, port int, roots []byte, clientPair tlsKeyPair, expectedALPN, expectedCommonName string) {
	t.Helper()
	clientConfig := rotationClientConfig(t, roots, clientPair)

	var lastErr error
	err := utilwait.PollUntilContextTimeout(ctx, time.Second, tlsRotationWaitTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(tlsRotationNamespace).List(ctx, metav1.ListOptions{LabelSelector: serverSelector})
		if err != nil || len(pods.Items) != tlsRotationServerReplicas {
			lastErr = err
			return false, nil
		}
		for _, pod := range pods.Items {
			if !isPodReady(&pod) {
				return false, nil
			}
			target := clientset.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("portforward").URL()
			commonName, err := podTLSHandshake(ctx, restConfig, target, port, clientConfig, expectedALPN)
			if err != nil {
				lastErr = fmt.Errorf("pod %s: %w", pod.Name, err)
				return false, nil
			}
			if commonName != expectedCommonName {
				lastErr = fmt.Errorf("pod %s presented common name %q, want %q", pod.Name, commonName, expectedCommonName)
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("server TLS on port %d did not converge to %q: %v (last error: %v)", port, expectedCommonName, err, lastErr)
	}
}

// rotationClientConfig parses the PEM material once, so that a poll loop does
// not rebuild the same config for every handshake attempt.
func rotationClientConfig(t *testing.T, rootsPEM []byte, clientPair tlsKeyPair) *tls.Config {
	t.Helper()
	rootCAs, err := certutil.NewPoolFromBytes(rootsPEM)
	if err != nil {
		t.Fatalf("could not parse root CA bundle: %v", err)
	}
	certificate, err := tls.X509KeyPair(clientPair.cert, clientPair.key)
	if err != nil {
		t.Fatalf("could not parse client certificate: %v", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{certificate},
		ServerName:   tlsRotationServerName,
		// Offer both protocols so the same client works against either
		// listener: the gRPC frontend and the agent listener advertise only
		// h2, and the http-connect frontend advertises only http/1.1.
		NextProtos: []string{"h2", "http/1.1"},
	}
}

// podPortForwardDialer closes the API connection when the probe ends, including
// when the API server has accepted the socket but has yet to answer the upgrade.
func podPortForwardDialer(ctx context.Context, config *rest.Config, target *url.URL) (httpstream.Dialer, error) {
	apiTLS, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, err
	}
	if apiTLS == nil {
		apiTLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	upgrader, err := httpstreamspdy.NewRoundTripperWithConfig(httpstreamspdy.RoundTripperConfig{
		UpgradeTransport: &http.Transport{
			TLSClientConfig: apiTLS,
			DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err == nil {
					context.AfterFunc(ctx, func() { conn.Close() })
				}
				return conn, err
			},
		},
	})
	if err != nil {
		return nil, err
	}
	transport, err := rest.HTTPWrappersForConfig(config, upgrader)
	if err != nil {
		return nil, err
	}
	return spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target), nil
}

func podTLSHandshake(ctx context.Context, restConfig *rest.Config, target *url.URL, remotePort int, tlsConfig *tls.Config, expectedALPN string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer, err := podPortForwardDialer(ctx, restConfig, target)
	if err != nil {
		return "", err
	}
	ready := make(chan struct{})
	forwarder, err := portforward.NewOnAddressesWithContext(ctx, dialer, []string{"127.0.0.1"}, []string{":" + strconv.Itoa(remotePort)}, ready, nil, nil)
	if err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- forwarder.ForwardPorts() }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-done:
		return "", fmt.Errorf("port forward stopped before it was ready: %v", err)
	case <-ready:
	}
	ports, err := forwarder.GetPorts()
	if err != nil {
		return "", err
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local)))
	rawConnection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", err
	}
	connection := tls.Client(rawConnection, tlsConfig)
	defer connection.Close()
	if err := connection.HandshakeContext(ctx); err != nil {
		return "", err
	}
	state := connection.ConnectionState()
	if state.NegotiatedProtocol != expectedALPN {
		return "", fmt.Errorf("negotiated ALPN %q, want %q", state.NegotiatedProtocol, expectedALPN)
	}
	if len(state.PeerCertificates) == 0 {
		return "", fmt.Errorf("server presented no certificate")
	}
	return state.PeerCertificates[0].Subject.CommonName, nil
}

// waitForGaugeValue waits until every pod matching selector reports expected
// for metric on its metrics port.
func waitForGaugeValue(ctx context.Context, t *testing.T, restConfig *rest.Config, clientset kubernetes.Interface, selector string, podCount, metricsPort int, metric string, expected int) {
	t.Helper()
	var lastErr error
	err := utilwait.PollUntilContextTimeout(ctx, time.Second, tlsRotationWaitTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(tlsRotationNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil || len(pods.Items) != podCount {
			lastErr = err
			return false, nil
		}
		for _, pod := range pods.Items {
			value, err := getMetricsGaugeValue(ctx, restConfig, pod.Namespace, pod.Name, metricsPort, metric)
			if err != nil || value != expected {
				lastErr = fmt.Errorf("pod %s reported %s=%d, want %d: %v", pod.Name, metric, value, expected, err)
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("%s did not reach %d for %q: %v (last error: %v)", metric, expected, selector, err, lastErr)
	}
}
