package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/apiserver-network-proxy/pkg/util"
)

type labelMap map[string]string

func TestServerLeaseCounter(t *testing.T) {
	testCases := []struct {
		name string

		templates        []util.LeaseTemplate
		leaseListerError error

		labelSelector string

		want int
	}{
		{
			name:          "returns fallback count (0) when no leases exist",
			templates:     []util.LeaseTemplate{},
			labelSelector: "label=value",
			want:          0,
		}, {
			name: "returns fallback count (0) when no leases matching selector exist",
			templates: []util.LeaseTemplate{
				{
					DurationSecs:     1000,
					TimeSinceAcquire: time.Second,
					Labels:           labelMap{"label": "wrong_value"},
				},
				{
					DurationSecs:     1000,
					TimeSinceAcquire: time.Second,
					Labels:           labelMap{"wrong_label": "value"},
				},
			},
			labelSelector: "label=value",
			want:          0,
		}, {
			name: "returns fallback count (0) when no leases matching selector are still valid",
			templates: []util.LeaseTemplate{
				{
					DurationSecs:     1000,
					TimeSinceAcquire: 10000 * time.Second,
					Labels:           labelMap{"label": "value"},
				},
				{
					DurationSecs:     1000,
					TimeSinceAcquire: time.Second,
					Labels:           labelMap{"wrong_label": "value"},
				},
			},
			labelSelector: "label=value",
			want:          0,
		}, {
			name:             "returns fallback count (0) when LeaseLister returns an error",
			templates:        []util.LeaseTemplate{},
			labelSelector:    "label=value",
			leaseListerError: fmt.Errorf("test error"),
			want:             0,
		}, {
			name: "counts only valid leases matching label selector",
			templates: []util.LeaseTemplate{
				{
					DurationSecs:     1000,
					TimeSinceAcquire: time.Second,
					Labels:           labelMap{"label": "value"},
				},
				{
					DurationSecs:     1000,
					TimeSinceAcquire: time.Second,
					Labels:           labelMap{"label": "value"},
				},
				{
					DurationSecs:     1000,
					TimeSinceAcquire: time.Second,
					Labels:           labelMap{"label": "wrong_value"},
				},
			},
			labelSelector: "label=value",
			want:          2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			leases := make([]runtime.Object, len(tc.templates))
			for i, template := range tc.templates {
				leases[i] = util.NewLeaseFromTemplate(template)
			}

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
	validLease := util.LeaseTemplate{
		DurationSecs:     1000,
		TimeSinceAcquire: time.Second,
		Labels:           map[string]string{"label": "value"},
	}
	invalidLease := util.LeaseTemplate{
		DurationSecs:     1000,
		TimeSinceAcquire: time.Second * 10000,
		Labels:           map[string]string{"label": "value"},
	}

	leases := []runtime.Object{util.NewLeaseFromTemplate(validLease), util.NewLeaseFromTemplate(validLease), util.NewLeaseFromTemplate(validLease), util.NewLeaseFromTemplate(invalidLease)}

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
