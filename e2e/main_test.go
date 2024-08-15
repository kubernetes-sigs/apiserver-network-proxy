package e2e

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"testing"
	"text/template"
	"time"

	appsv1api "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/k8s"
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
		envfuncs.CreateClusterWithConfig(kindCluster, kindClusterName, "templates/kind.config"),
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

type CLIFlag struct {
	Flag       string
	Value      string
	EmptyValue bool
}

type DeploymentConfig struct {
	Replicas int
	Image    string
	Args     []CLIFlag
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

func createDeployment(obj client.Object) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		deployment, ok := obj.(*appsv1api.Deployment)
		if !ok {
			t.Fatalf("object %q is not a deployment", obj.GetName())
		}

		err := cfg.Client().Resources(deployment.Namespace).Create(ctx, deployment)
		if err != nil {
			t.Fatalf("could not create deployment %q: %v", deployment.Name, err)
		}

		newDeployment := &appsv1api.Deployment{}
		err = cfg.Client().Resources(deployment.Namespace).Get(ctx, deployment.Name, deployment.Namespace, newDeployment)
		if err != nil {
			t.Fatalf("could not get deployment %q after creation: %v", deployment.Name, err)
		}

		return ctx
	}
}

func deleteDeployment(obj client.Object) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		deployment, ok := obj.(*appsv1api.Deployment)
		if !ok {
			t.Fatalf("object %q is not a deployment", obj.GetName())
		}

		k8sClient := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
		pods, err := k8sClient.CoreV1().Pods(deployment.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.FormatLabels(deployment.Spec.Selector.MatchLabels)})
		if err != nil {
			t.Fatalf("could not get pods for deployment %q: %v", deployment.Name, err)
		}

		cfg.Client().Resources(deployment.Namespace).Delete(ctx, deployment)

		err = wait.For(
			conditions.New(cfg.Client().Resources(deployment.Namespace)).ResourcesDeleted(pods),
			wait.WithTimeout(60*time.Second),
			wait.WithInterval(5*time.Second),
		)
		if err != nil {
			pods, err := k8sClient.CoreV1().Pods(deployment.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.FormatLabels(deployment.Spec.Selector.MatchLabels)})
			if err != nil {
				t.Fatalf("could not get pods for deployment %q: %v", deployment.Name, err)
			}

			for _, pod := range pods.Items {
				logs, err := dumpPodLogs(ctx, k8sClient, pod.Namespace, pod.Name)
				if err != nil {
					t.Fatalf("could not dump logs for pod %q: %v", pod.Name, err)
				}
				t.Errorf("logs for pod %q: %v", pod.Name, logs)
			}
			t.Fatalf("waiting for deletion of pods for deployment %q failed, dumped pod logs", deployment.Name)
		}

		return ctx
	}
}

func sleepFor(duration time.Duration) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		time.Sleep(duration)
		return ctx
	}
}

func waitForDeployment(obj client.Object) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		deployment, ok := obj.(*appsv1api.Deployment)
		if !ok {
			t.Fatalf("object %q is not a deployment", obj.GetName())
		}

		k8sClient := kubernetes.NewForConfigOrDie(cfg.Client().RESTConfig())
		err := wait.For(
			conditions.New(cfg.Client().Resources(deployment.Namespace)).DeploymentAvailable(deployment.Name, deployment.Namespace),
			wait.WithTimeout(60*time.Second),
			wait.WithInterval(5*time.Second),
		)
		if err != nil {
			pods, err := k8sClient.CoreV1().Pods(deployment.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.FormatLabels(deployment.Spec.Selector.MatchLabels)})
			if err != nil {
				t.Fatalf("could not get pods for deployment %q: %v", deployment.Name, err)
			}

			for _, pod := range pods.Items {
				if isPodReady(&pod) {
					continue
				}

				logs, err := dumpPodLogs(ctx, k8sClient, pod.Namespace, pod.Name)
				if err != nil {
					t.Fatalf("could not dump logs for pod %q: %v", pod.Name, err)
				}
				t.Errorf("logs for pod %q: %v", pod.Name, logs)
			}
			t.Fatalf("waiting for deployment %q failed, dumped pod logs", deployment.Name)
		}

		return ctx
	}
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func dumpPodLogs(ctx context.Context, k8sClient kubernetes.Interface, namespace, name string) (string, error) {
	req := k8sClient.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("could not stream logs for pod %q in namespace %q: %w", name, namespace, err)
	}
	defer podLogs.Close()

	b, err := io.ReadAll(podLogs)
	if err != nil {
		return "", fmt.Errorf("could not read logs for pod %q in namespace %q from stream: %w", name, namespace, err)
	}

	return string(b), nil
}

func scaleDeployment(obj client.Object, replicas int) func(context.Context, *testing.T, *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		deployment, ok := obj.(*appsv1api.Deployment)
		if !ok {
			t.Fatalf("provided object is not a deployment")
		}

		err := cfg.Client().Resources().Get(ctx, deployment.Name, deployment.Namespace, deployment)
		if err != nil {
			t.Fatalf("could not get Deployment to update (name: %q, namespace: %q): %v", deployment.Name, deployment.Namespace, err)
		}

		newReplicas := int32(replicas)
		deployment.Spec.Replicas = &newReplicas

		client := cfg.Client()
		err = client.Resources().Update(ctx, deployment)
		if err != nil {
			t.Fatalf("could not update Deployment replicas: %v", err)
		}

		err = wait.For(
			conditions.New(cfg.Client().Resources(deployment.Namespace)).ResourceScaled(deployment, func(obj k8s.Object) int32 {
				deployment, ok := obj.(*appsv1api.Deployment)
				if !ok {
					t.Fatalf("provided object is not a deployment")
				}

				return deployment.Status.AvailableReplicas
			}, int32(replicas)),
			wait.WithTimeout(60*time.Second),
			wait.WithInterval(5*time.Second),
		)
		if err != nil {
			t.Fatalf("waiting for deployment %q to scale failed", deployment.Name)
		}

		return ctx
	}
}
