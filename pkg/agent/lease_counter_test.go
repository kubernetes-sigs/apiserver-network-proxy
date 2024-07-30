package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1listers "k8s.io/client-go/listers/coordination/v1"
)

type leaseTemplate struct {
	durationSecs     int32
	timeSinceAcquire time.Duration
	timeSinceRenew   time.Duration
	labels           map[string]string
}

type controlledTime struct {
	t time.Time
}

func (ct *controlledTime) Now() time.Time {
	return ct.t
}

func (ct *controlledTime) Advance(d time.Duration) {
	ct.t = ct.t.Add(d)
}

func newLeaseFromTemplate(template leaseTemplate) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:   uuid.New().String(),
			Labels: template.labels,
		},
		Spec: coordinationv1.LeaseSpec{},
	}

	if template.durationSecs != 0 {
		lease.Spec.LeaseDurationSeconds = &template.durationSecs
	}
	if template.timeSinceAcquire != time.Duration(0) {
		acquireTime := metav1.NewMicroTime(timeNow().Add(-template.timeSinceAcquire))
		lease.Spec.AcquireTime = &acquireTime
	}
	if template.timeSinceRenew != time.Duration(0) {
		renewTime := metav1.NewMicroTime(timeNow().Add(-template.timeSinceRenew))
		lease.Spec.RenewTime = &renewTime
	}

	return lease
}

func TestIsLeaseValid(t *testing.T) {
	testCases := []struct {
		name     string
		template leaseTemplate
		want     bool
	}{
		{
			name: "freshly acquired lease is valid",
			template: leaseTemplate{
				durationSecs:     1000,
				timeSinceAcquire: time.Second,
			},
			want: true,
		}, {
			name: "freshly renewed lease is valid",
			template: leaseTemplate{
				durationSecs:     1000,
				timeSinceAcquire: 10000 * time.Second,
				timeSinceRenew:   time.Second,
			},
			want: true,
		}, {
			name: "lease with neither acquisition nor renewal time is invalid",
			template: leaseTemplate{
				durationSecs: 1000,
			},
			want: false,
		}, {
			name: "expired lease (only acquired) is invalid",
			template: leaseTemplate{
				durationSecs:     1000,
				timeSinceAcquire: 10000 * time.Second,
			},
			want: false,
		}, {
			name: "expired lease (acquired and renewed) is invalid",
			template: leaseTemplate{
				durationSecs:     1000,
				timeSinceAcquire: 10000 * time.Second,
				timeSinceRenew:   9000 * time.Second,
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lease := newLeaseFromTemplate(tc.template)

			got := isLeaseValid(*lease)
			if got != tc.want {
				t.Errorf("incorrect lease validity (got: %v, want: %v)", got, tc.want)
			}
		})
	}
}

type fakeLeaseLister struct {
	leases []*coordinationv1.Lease
	calls  []labels.Selector
	err    error
}

type labelMap map[string]string

func (l labelMap) Has(label string) bool {
	_, exists := l[label]
	return exists
}
func (l labelMap) Get(label string) string {
	value, exists := l[label]
	if !exists {
		return ""
	}

	return value
}

func (lister *fakeLeaseLister) List(selector labels.Selector) ([]*coordinationv1.Lease, error) {
	lister.calls = append(lister.calls, selector)

	if lister.err != nil {
		return nil, lister.err
	}

	res := []*coordinationv1.Lease{}
	for _, lease := range lister.leases {
		if selector.Matches(labelMap(lease.Labels)) {
			res = append(res, lease)
		}
	}

	return res, nil
}
func (lister *fakeLeaseLister) Leases(_ string) coordinationv1listers.LeaseNamespaceLister {
	panic("not implemented")
}

func TestServerLeaseCounter(t *testing.T) {
	testCases := []struct {
		name string

		templates        []leaseTemplate
		leaseListerError error

		labelSelector string

		want int
	}{
		{
			name:          "returns fallback count (0) when no leases exist",
			templates:     []leaseTemplate{},
			labelSelector: "label=value",
			want:          0,
		}, {
			name: "returns fallback count (0) when no leases matching selector exist",
			templates: []leaseTemplate{
				{
					durationSecs:     1000,
					timeSinceAcquire: time.Second,
					labels:           labelMap{"label": "wrong_value"},
				},
				{
					durationSecs:     1000,
					timeSinceAcquire: time.Second,
					labels:           labelMap{"wrong_label": "value"},
				},
			},
			labelSelector: "label=value",
			want:          0,
		}, {
			name: "returns fallback count (0) when no leases matching selector are still valid",
			templates: []leaseTemplate{
				{
					durationSecs:     1000,
					timeSinceAcquire: 10000 * time.Second,
					labels:           labelMap{"label": "value"},
				},
				{
					durationSecs:     1000,
					timeSinceAcquire: time.Second,
					labels:           labelMap{"wrong_label": "value"},
				},
			},
			labelSelector: "label=value",
			want:          0,
		}, {
			name:             "returns fallback count (0) when LeaseLister returns an error",
			templates:        []leaseTemplate{},
			labelSelector:    "label=value",
			leaseListerError: fmt.Errorf("test error"),
			want:             0,
		}, {
			name: "counts only valid leases matching label selector",
			templates: []leaseTemplate{
				{
					durationSecs:     1000,
					timeSinceAcquire: time.Second,
					labels:           labelMap{"label": "value"},
				},
				{
					durationSecs:     1000,
					timeSinceAcquire: time.Second,
					labels:           labelMap{"label": "value"},
				},
				{
					durationSecs:     1000,
					timeSinceAcquire: time.Second,
					labels:           labelMap{"label": "wrong_value"},
				},
			},
			labelSelector: "label=value",
			want:          2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ct := &controlledTime{t: time.Unix(10000000, 0)}
			timeNow = ct.Now
			leases := make([]runtime.Object, len(tc.templates))
			for i, template := range tc.templates {
				leases[i] = newLeaseFromTemplate(template)
			}
			ct.Advance(time.Millisecond)

			k8sClient := fake.NewSimpleClientset(leases...)
			selector, _ := labels.Parse(tc.labelSelector)

			counter := NewServerLeaseCounter(k8sClient, selector)

			got := counter.Count(context.Background())
			if tc.want != got {
				t.Errorf("incorrect server count (got: %v, want: %v)", got, tc.want)
			}
		})
	}
}

func TestServerLeaseCounter_FallbackCount(t *testing.T) {
	validLease := leaseTemplate{
		durationSecs:     1000,
		timeSinceAcquire: time.Second,
		labels:           map[string]string{"label": "value"},
	}
	invalidLease := leaseTemplate{
		durationSecs:     1000,
		timeSinceAcquire: time.Second * 10000,
		labels:           map[string]string{"label": "value"},
	}

	ct := &controlledTime{t: time.Unix(1000, 0)}
	timeNow = ct.Now
	leases := []runtime.Object{newLeaseFromTemplate(validLease), newLeaseFromTemplate(validLease), newLeaseFromTemplate(validLease), newLeaseFromTemplate(invalidLease)}
	ct.Advance(time.Millisecond)

	k8sClient := fake.NewSimpleClientset(leases...)
	callShouldFail := true

	selector, _ := labels.Parse("label=value")

	counter := NewServerLeaseCounter(k8sClient, selector)

	// First call should return fallback count of 0 because of leaseClient error.
	k8sClient.PrependReactor("*", "*", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		if callShouldFail {
			return true, nil, fmt.Errorf("dummy lease client error")
		}
		return false, nil, nil
	})
	ctx := context.Background()
	got := counter.Count(ctx)
	if got != 0 {
		t.Errorf("lease counter did not return fallback count on leaseClient error (got: %v, want: 0)", got)
	}

	// Second call should return the actual count (3) upon leaseClient success.
	callShouldFail = false
	actualCount := 3
	got = counter.Count(ctx)
	if got != actualCount {
		t.Errorf("lease counter did not return actual count on leaseClient success (got: %v, want: %v)", got, actualCount)
	}

	// Third call should return updated fallback count (3) upon leaseClient failure.
	callShouldFail = true
	got = counter.Count(ctx)
	if got != actualCount {
		t.Errorf("lease counter did not update fallback count after leaseClient success, returned incorrect count on subsequent leaseClient error (got: %v, want: %v)", got, actualCount)
	}
}
