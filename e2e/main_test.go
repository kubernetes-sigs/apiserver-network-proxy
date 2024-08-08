package e2e

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"testing"
	"text/template"
	"time"

	appsv1api "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv        env.Environment
	agentImage     = flag.String("agent-image", "", "The proxy agent's docker image.")
	serverImage    = flag.String("server-image", "", "The proxy server's docker image.")
	kindImage      = flag.String("kind-image", "kindest/node", "Image to use for kind nodes.")
	connectionMode = flag.String("mode", "grpc", "Connection mode to use during e2e tests.")
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
	kindCluster := kind.NewCluster(kindClusterName).WithOpts(kind.WithImage(*kindImage))

	testenv.Setup(
		envfuncs.CreateCluster(kindCluster, kindClusterName),
		envfuncs.LoadImageToCluster(kindClusterName, *agentImage),
		envfuncs.LoadImageToCluster(kindClusterName, *serverImage),
		renderAndApplyManifests,
	)

	testenv.Finish(envfuncs.DestroyCluster(kindClusterName))

	os.Exit(testenv.Run(m))
}

// renderTemplate renders a template from e2e/templates into a kubernetes object.
// Template paths are relative to e2e/templates.
func renderTemplate(file string, params any) (client.Object, *schema.GroupVersionKind, error) {
	b := &bytes.Buffer{}

	tmp, err := template.ParseFiles(path.Join("templates/", file))
	if err != nil {
		return nil, nil, fmt.Errorf("could not parse template %v: %w", file, err)
	}

	err = tmp.Execute(b, params)
	if err != nil {
		return nil, nil, fmt.Errorf("could not execute template %v: %w", file, err)
	}

	decoder := scheme.Codecs.UniversalDeserializer()

	obj, gvk, err := decoder.Decode(b.Bytes(), nil, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("could not decode rendered yaml into kubernetes object: %w", err)
	}

	return obj.(client.Object), gvk, nil
}

type KeyValue struct {
	Key   string
	Value string
}

type DeploymentConfig struct {
	Replicas int
	Image    string
	Args     []KeyValue
}

func renderAndApplyManifests(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	client := cfg.Client()

	// Render agent RBAC and Service templates.
	agentServiceAccount, _, err := renderTemplate("agent/serviceaccount.yaml", struct{}{})
	if err != nil {
		return nil, err
	}
	agentClusterRole, _, err := renderTemplate("agent/clusterrole.yaml", struct{}{})
	if err != nil {
		return nil, err
	}
	agentClusterRoleBinding, _, err := renderTemplate("agent/clusterrolebinding.yaml", struct{}{})
	if err != nil {
		return ctx, err
	}
	agentService, _, err := renderTemplate("agent/service.yaml", struct{}{})
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
	serverClusterRoleBinding, _, err := renderTemplate("server/clusterrolebinding.yaml", struct{}{})
	if err != nil {
		return ctx, err
	}
	serverService, _, err := renderTemplate("server/service.yaml", struct{}{})
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
}

func deployAndWaitForDeployment(obj client.Object) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		client := cfg.Client()
		err := client.Resources().Create(ctx, obj)
		if err != nil {
			t.Fatalf("could not create Deployment: %v", err)
		}

		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(obj.GetName(), obj.GetNamespace()),
			wait.WithTimeout(1*time.Minute),
			wait.WithInterval(10*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for Deployment failed: %v", err)
		}

		return ctx
	}
}

func scaleDeployment(obj client.Object, replicas int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		deployment, ok := obj.(*appsv1api.Deployment)
		if !ok {
			t.Fatalf("provided object is not a deployment")
		}

		newReplicas := int32(replicas)
		deployment.Spec.Replicas = &newReplicas

		client := cfg.Client()
		err := client.Resources().Update(ctx, deployment)
		if err != nil {
			t.Fatalf("could not update Deployment replicas: %v", err)
		}

		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(deployment.GetName(), deployment.GetNamespace()),
			wait.WithTimeout(1*time.Minute),
			wait.WithInterval(10*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for Deployment failed: %v", err)
		}

		return ctx
	}
}
