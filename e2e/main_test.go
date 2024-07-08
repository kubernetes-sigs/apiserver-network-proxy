package e2e

import (
	"context"
	"flag"
	"log"
	"os"
	"testing"

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

			// Render agent RBAC templates.
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

func TestSingleAgentAndServer(t *testing.T) {

}
