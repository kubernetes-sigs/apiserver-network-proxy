package agent

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var syncPodStatusTable = []struct {
	Name            string
	ExpectCondition bool
	MutateClientSet func(*ClientSet)
}{
	{
		Name:            "happy-path",
		ExpectCondition: true,
	},
	{
		Name:            "missing-kubeconfig",
		ExpectCondition: false,
		MutateClientSet: func(cs *ClientSet) {
			cs.k8sClient = nil
		},
	},
	{
		Name:            "disabled",
		ExpectCondition: false,
		MutateClientSet: func(cs *ClientSet) {
			cs.enablePodCondition = false
		},
	},
	{
		Name:            "missing-pod-name",
		ExpectCondition: false,
		MutateClientSet: func(cs *ClientSet) {
			cs.podName = ""
		},
	},
	{
		Name:            "missing-pod-namespace",
		ExpectCondition: false,
		MutateClientSet: func(cs *ClientSet) {
			cs.podNamespace = ""
		},
	},
}

func TestClientSetSyncPodStatus(t *testing.T) {
	for _, tc := range syncPodStatusTable {
		t.Run(tc.Name, func(t *testing.T) {
			k8sClient := fake.NewSimpleClientset()

			pod := &corev1.Pod{}
			pod.Name = "test-pod"
			pod.Namespace = "default"
			pod.Status.Conditions = []corev1.PodCondition{{
				Type: "AnotherType",
			}}
			_, err := k8sClient.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}

			cs := &ClientSet{
				k8sClient:          k8sClient,
				enablePodCondition: true,
				podNamespace:       pod.Namespace,
				podName:            pod.Name,
			}
			if tc.MutateClientSet != nil {
				tc.MutateClientSet(cs)
			}
			cs.syncPodStatus()

			actual, err := k8sClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			expected := len(pod.Status.Conditions)
			if tc.ExpectCondition {
				expected++
			}
			if l := len(actual.Status.Conditions); l != expected {
				t.Fatalf("expected %d conditions, got %d", expected, l)
			}
			firstUpdateVersion := actual.ResourceVersion

			cs.syncPodStatus()
			actual, err = k8sClient.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if actual.ResourceVersion != firstUpdateVersion {
				t.Fatal("expected syncPodStatus to be idempotent")
			}
		})
	}
}

func TestClientSetAppendPodCondition(t *testing.T) {
	cs := &ClientSet{}
	initialPod := &corev1.Pod{}
	condition := &corev1.PodCondition{Type: "test"}

	var pod *corev1.Pod
	assert := func() {
		if initialPod == pod {
			t.Error("expected a copy of the pod to be returned")
		}
		if l := len(pod.Status.Conditions); l != 1 {
			t.Fatalf("expected one condition, got %d", l)
		}
		if !reflect.DeepEqual(&pod.Status.Conditions[0], condition) {
			t.Errorf("expected condition to be in sync, got: %+v", pod.Status.Conditions[0])
		}
	}

	t.Run("insert", func(t *testing.T) {
		pod = cs.appendPodCondition(initialPod, condition)
		assert()
	})

	t.Run("noop", func(t *testing.T) {
		if cs.appendPodCondition(pod, condition) != nil {
			t.Error("expected nil when condition is in sync")
		}
	})

	t.Run("update-status", func(t *testing.T) {
		condition.Status = "test-status"
		pod = cs.appendPodCondition(pod, condition)
		assert()
	})

	t.Run("update-reason", func(t *testing.T) {
		condition.Reason = "test-reason"
		pod = cs.appendPodCondition(pod, condition)
		assert()
	})

	t.Run("update-message", func(t *testing.T) {
		condition.Message = "test-reason"
		pod = cs.appendPodCondition(pod, condition)
		assert()
	})
}

var getPodConditionTests = []struct {
	Name    string
	Current int
	Desired int
	Status  corev1.ConditionStatus
	Reason  string
}{
	{
		Name:    "zeros",
		Current: 0,
		Desired: 0,
		Status:  corev1.ConditionFalse,
		Reason:  disconnectedPodConditionReason,
	},
	{
		Name:    "connected",
		Current: 1,
		Desired: 1,
		Status:  corev1.ConditionTrue,
		Reason:  connectedPodConditionReason,
	},
	{
		Name:    "disconnected",
		Current: 0,
		Desired: 1,
		Status:  corev1.ConditionFalse,
		Reason:  disconnectedPodConditionReason,
	},
	{
		Name:    "ha-connected",
		Current: 3,
		Desired: 3,
		Status:  corev1.ConditionTrue,
		Reason:  connectedPodConditionReason,
	},
	{
		Name:    "ha-partial",
		Current: 2,
		Desired: 3,
		Status:  corev1.ConditionFalse,
		Reason:  partiallyConnectedPodConditionReason,
	},
	{
		Name:    "ha-disconnected",
		Current: 0,
		Desired: 3,
		Status:  corev1.ConditionFalse,
		Reason:  disconnectedPodConditionReason,
	},
}

func TestGetPodCondition(t *testing.T) {
	for _, tc := range getPodConditionTests {
		t.Run(tc.Name, func(t *testing.T) {
			condition := getPodCondition(tc.Current, tc.Desired)

			zeroTime := time.Time{}
			if condition.LastTransitionTime.Time == zeroTime {
				t.Errorf("expected last transition time to be set")
			}
			condition.LastTransitionTime = metav1.Time{}

			if condition.Status != tc.Status {
				t.Errorf("got status %s, expected %s", condition.Status, tc.Status)
			}
			if condition.Reason != tc.Reason {
				t.Errorf("got reason %s, expected %s", condition.Reason, tc.Reason)
			}
		})
	}
}
