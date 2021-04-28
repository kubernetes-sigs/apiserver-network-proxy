package tests

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/apiserver-network-proxy/pkg/agent"
)

func TestPodConditionUpdate(t *testing.T) {
	stopCh := make(chan struct{})
	defer close(stopCh)

	pod := &corev1.Pod{}
	pod.Name = "test-pod"
	pod.Namespace = "default"
	k8sClient := fake.NewSimpleClientset()
	pod, err := k8sClient.CoreV1().Pods(pod.Namespace).Create(pod)
	if err != nil {
		t.Fatal(err)
	}

	proxy, cleanups := setupHAProxyServer(t)
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()

	lb := tcpLB{
		backends: []string{
			proxy[0].agent,
			proxy[1].agent,
			proxy[2].agent,
		},
		t: t,
	}
	lbAddr := lb.serve(stopCh)

	csc := agent.ClientSetConfig{
		Address:            lbAddr,
		AgentID:            uuid.New().String(),
		SyncInterval:       50 * time.Millisecond,
		ProbeInterval:      50 * time.Millisecond,
		DialOptions:        []grpc.DialOption{grpc.WithInsecure()},
		EnablePodCondition: true,
		PodNamespace:       pod.Namespace,
		PodName:            pod.Name,
	}
	clientset := csc.NewAgentClientSet(stopCh, k8sClient)
	clientset.Serve()

	waitForTransition := func(target corev1.ConditionStatus, reason string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			time.Sleep(time.Millisecond * 25)
			pod, err = k8sClient.CoreV1().Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == "k8s.io/apiserver-network-proxy-connected" &&
					cond.Status == target && cond.Reason == reason {
					return
				}
			}
		}
		t.Fatalf("timeout while waiting for transition to state %s", target)
	}

	// Connected to all servers
	waitForTransition(corev1.ConditionTrue, "Connected")

	// Connected to 2 servers
	lb.removeBackend(proxy[0].agent)
	cleanups[0]()
	waitForTransition(corev1.ConditionFalse, "Partition")

	// Connected to 0 servers
	lb.removeBackend(proxy[1].agent)
	cleanups[1]()
	lb.removeBackend(proxy[2].agent)
	cleanups[2]()
	waitForTransition(corev1.ConditionFalse, "Disconnected")
}
