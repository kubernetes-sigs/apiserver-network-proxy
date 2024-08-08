package e2e

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type leaseController struct {
	labels        map[string]string
	validLeases   []*coordinationv1.Lease
	expiredLeases []*coordinationv1.Lease
}

func NewLeaseController(labels map[string]string) *leaseController {
	return &leaseController{
		labels:        labels,
		validLeases:   []*coordinationv1.Lease{},
		expiredLeases: []*coordinationv1.Lease{},
	}
}

func (lc *leaseController) PublishValidLease() func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		var duration int32 = 999999999
		acquireTime := metav1.NewMicroTime(time.Now())

		newLease := &coordinationv1.Lease{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:   uuid.New().String(),
				Labels: lc.labels,
			},
			Spec: coordinationv1.LeaseSpec{
				LeaseDurationSeconds: &duration,
				AcquireTime:          &acquireTime,
			},
		}

		err := cfg.Client().Resources().Create(ctx, newLease)
		if err != nil {
			t.Fatalf("could not publish valid lease: %v", err)
		}

		lc.validLeases = append(lc.validLeases, newLease)
		return ctx
	}
}

func (lc *leaseController) DeleteValidLease() func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		leaseToDelete := lc.validLeases[len(lc.validLeases)-1]

		err := cfg.Client().Resources().Delete(ctx, leaseToDelete)
		if err != nil {
			t.Fatalf("could not delete valid lease: %v", err)
		}

		lc.validLeases = lc.validLeases[:len(lc.validLeases)-1]
		return ctx
	}
}

func (lc *leaseController) PublishExpiredLease() func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		var duration int32 = 1
		acquireTime := metav1.NewMicroTime(time.Now().Add(-time.Second * 99999999))

		newLease := &coordinationv1.Lease{
			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:   uuid.New().String(),
				Labels: lc.labels,
			},
			Spec: coordinationv1.LeaseSpec{
				LeaseDurationSeconds: &duration,
				AcquireTime:          &acquireTime,
			},
		}

		err := cfg.Client().Resources().Create(ctx, newLease)
		if err != nil {
			t.Fatalf("could not publish expired lease: %v", err)
		}

		lc.expiredLeases = append(lc.expiredLeases, newLease)
		return ctx
	}
}

func (lc *leaseController) DeleteExpiredLease() func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		leaseToDelete := lc.expiredLeases[len(lc.expiredLeases)-1]

		err := cfg.Client().Resources().Delete(ctx, leaseToDelete)
		if err != nil {
			t.Fatalf("could not delete expired lease: %v", err)
		}

		lc.expiredLeases = lc.expiredLeases[:len(lc.expiredLeases)-1]
		return ctx
	}
}

func TestLeaseCount(t *testing.T) {
	adminPort := 8093
	initialServerReplicas := 6
	initialAgentReplicas := 2
	leaseLabels := map[string]string{"aTestLabel": "aTestValue"}
	leaseLabelSelector := "aTestLabel=aTestValue"

	serverDeploymentConfig := DeploymentConfig{
		Replicas: initialServerReplicas,
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
			{"server-count", strconv.Itoa(initialServerReplicas)},
		},
	}
	serverDeployment, _, err := renderTemplate("server/deployment.yaml", serverDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentDeploymentConfig := DeploymentConfig{
		Replicas: initialAgentReplicas,
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
			{"server-count-lease-selector", leaseLabelSelector},
		},
	}
	agentDeployment, _, err := renderTemplate("agent/deployment.yaml", agentDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render agent deployment: %v", err)
	}

	lc := NewLeaseController(leaseLabels)

	feature := features.New("konnectivity agent lease counting system")
	feature.Setup(deployAndWaitForDeployment(serverDeployment))
	feature.Setup(deployAndWaitForDeployment(agentDeployment))
	// We start off by publishing two valid leases and one expired lease.
	feature.Setup(lc.PublishValidLease())
	feature.Setup(lc.PublishValidLease())
	feature.Setup(lc.PublishExpiredLease())
	feature.Assess("agents correctly count 2 leases (2 valid, 1 expired)", assertAgentKnownServerCount(2, adminPort))
	// Publishing additional expired leases should not change the server count.
	feature.Setup(lc.PublishExpiredLease())
	feature.Setup(lc.PublishExpiredLease())
	feature.Assess("agents correctly count 2 leases (2 valid, 3 expired)", assertAgentKnownServerCount(2, adminPort))
	// Publishing additional valid leases should increase the server count.
	feature.Setup(lc.PublishValidLease())
	feature.Setup(lc.PublishValidLease())
	feature.Assess("agents correctly count 4 leases (4 valid, 3 expired)", assertAgentKnownServerCount(4, adminPort))
	// Deleting a valid lease should reduce the server count.
	feature.Setup(lc.DeleteValidLease())
	feature.Assess("agents correctly count 3 leases (3 valid, 3 expired)", assertAgentKnownServerCount(3, adminPort))
}

func TestSingleServer_SingleAgent_LeaseCount(t *testing.T) {
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
			{"lease-gc-interval", "10s"},
			{"lease-renewal-interval", "5s"},
			{"lease-label-selector", "k8s-app=konnectivity-server"},
			{"lease-namespace", "kube-system"},
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
			{"server-lease-selector", "k8s-app=konnectivity-server"},
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

func TestMultiServer_MultiAgent_LeaseCount(t *testing.T) {
	adminPort := 8093
	initialAgentReplicas := 2
	initialServerReplicas := 2

	serverDeploymentConfig := DeploymentConfig{
		Replicas: initialServerReplicas,
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
			{"server-count", strconv.Itoa(initialServerReplicas)},
			{"lease-gc-interval", "10s"},
			{"lease-renewal-interval", "5s"},
			{"lease-label-selector", "k8s-app=konnectivity-server"},
			{"lease-namespace", "kube-system"},
		},
	}
	serverDeployment, _, err := renderTemplate("server/deployment.yaml", serverDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render server deployment: %v", err)
	}

	agentDeploymentConfig := DeploymentConfig{
		Replicas: initialAgentReplicas,
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
			{"server-lease-selector", "k8s-app=konnectivity-server"},
		},
	}
	agentDeployment, _, err := renderTemplate("agent/deployment.yaml", agentDeploymentConfig)
	if err != nil {
		t.Fatalf("could not render agent deployment: %v", err)
	}

	feature := features.New("konnectivity server and agent deployment with single replica for each")
	feature.Setup(deployAndWaitForDeployment(serverDeployment))
	feature.Setup(deployAndWaitForDeployment(agentDeployment))
	feature.Assess("all servers connected to all clients", assertServersAreConnected(initialAgentReplicas, adminPort))
	feature.Assess("all agents connected to all servers", assertAgentsAreConnected(initialServerReplicas, adminPort))
	feature.Setup(scaleDeployment(serverDeployment, 4))
	feature.Setup(scaleDeployment(agentDeployment, 4))
	feature.Assess("all servers connected to all clients after scale up", assertServersAreConnected(4, adminPort))
	feature.Assess("all agents connected to all servers after scale up", assertAgentsAreConnected(4, adminPort))
	feature.Setup(scaleDeployment(serverDeployment, 3))
	feature.Setup(scaleDeployment(agentDeployment, 3))
	feature.Assess("all servers connected to all clients after scale down", assertServersAreConnected(3, adminPort))
	feature.Assess("all agents connected to all servers after scale down", assertAgentsAreConnected(3, adminPort))
}
