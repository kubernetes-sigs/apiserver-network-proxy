package e2e

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"testing"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv     env.Environment
	agentImage  = flag.String("agent-image", "", "The proxy agent's docker image.")
	serverImage = flag.String("server-image", "", "The proxy server's docker image.")
)

func TestMain(m *testing.M) {
	flag.Parse()
	if *agentImage == "" {
		log.Fatalf("must provide agent image with -agent-image")
	}
	if *serverImage == "" {
		log.Fatalf("must provide server image with -server-image")
	}

	scheme.AddToScheme(scheme.Scheme)

	testenv = env.New()
	kindClusterName := "kind-test"
	kindCluster := kind.NewCluster(kindClusterName)

	testenv.Setup(
		envfuncs.CreateCluster(kindCluster, kindClusterName),
		envfuncs.LoadImageToCluster(kindClusterName, *agentImage),
		envfuncs.LoadImageToCluster(kindClusterName, *serverImage),
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			client := cfg.Client()

			// Render agent RBAC and Service templates.
			agentServiceAccount, _, err := renderAgentTemplate("serviceaccount.yaml", struct{}{})
			if err != nil {
				return nil, err
			}
			agentClusterRole, _, err := renderAgentTemplate("clusterrole.yaml", struct{}{})
			if err != nil {
				return nil, err
			}
			agentClusterRoleBinding, _, err := renderAgentTemplate("clusterrolebinding.yaml", struct{}{})
			if err != nil {
				return ctx, err
			}
			agentService, _, err := renderAgentTemplate("service.yaml", struct{}{})
			if err != nil {
				return ctx, err
			}

			// Submit agent RBAC templates to k8s.
			err = client.Resources().Create(ctx, agentServiceAccount)
			if err != nil {
				return ctx, err
			}
			err = client.Resources().Create(ctx, agentClusterRole)
			if err != nil {
				return ctx, err
			}
			err = client.Resources().Create(ctx, agentClusterRoleBinding)
			if err != nil {
				return ctx, err
			}
			err = client.Resources().Create(ctx, agentService)
			if err != nil {
				return ctx, err
			}

			// Render server RBAC and Service templates.
			serverClusterRoleBinding, _, err := renderServerTemplate("clusterrolebinding.yaml", struct{}{})
			if err != nil {
				return ctx, err
			}
			serverService, _, err := renderServerTemplate("service.yaml", struct{}{})
			if err != nil {
				return ctx, err
			}

			// Submit server templates to k8s.
			err = client.Resources().Create(ctx, serverClusterRoleBinding)
			if err != nil {
				return ctx, err
			}
			err = client.Resources().Create(ctx, serverService)
			if err != nil {
				return ctx, err
			}

			return ctx, nil
		},
	)

	testenv.Finish(envfuncs.DestroyCluster(kindClusterName))

	os.Exit(testenv.Run(m))
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
