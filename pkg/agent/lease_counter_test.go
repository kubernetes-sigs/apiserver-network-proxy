/*
Copyright 2024 The Kubernetes Authors.

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
	clocktesting "k8s.io/utils/clock/testing"
	proxytesting "sigs.k8s.io/apiserver-network-proxy/pkg/testing"
)

type labelMap map[string]string

func TestServerLeaseCounter(t *testing.T) {
	testCases := []struct {
		name string

		templates        []proxytesting.LeaseTemplate
		leaseListerError error

		labelSelector string

		want int
	}{
		{
			name:          "returns fallback count (1) when no leases exist",
			templates:     []proxytesting.LeaseTemplate{},
			labelSelector: "label=value",
			want:          1,
		}, {
			name: "returns fallback count (1) when no leases matching selector exist",
			templates: []proxytesting.LeaseTemplate{
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
			want:          1,
		}, {
			name: "returns fallback count (1) when no leases matching selector are still valid",
			templates: []proxytesting.LeaseTemplate{
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
			want:          1,
		}, {
			name:             "returns fallback count (1) when LeaseLister returns an error",
			templates:        []proxytesting.LeaseTemplate{},
			labelSelector:    "label=value",
			leaseListerError: fmt.Errorf("test error"),
			want:             1,
		}, {
			name: "counts only valid leases matching label selector",
			templates: []proxytesting.LeaseTemplate{
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
			pc := clocktesting.NewFakePassiveClock(time.Now())
			leases := make([]runtime.Object, len(tc.templates))
			for i, template := range tc.templates {
				leases[i] = proxytesting.NewLeaseFromTemplate(pc, template)
			}

			k8sClient := fake.NewSimpleClientset(leases...)
			selector, _ := labels.Parse(tc.labelSelector)

			counter := NewServerLeaseCounter(pc, k8sClient, selector, "")

			got := counter.Count(context.Background())
			if tc.want != got {
				t.Errorf("incorrect server count (got: %v, want: %v)", got, tc.want)
			}
		})
	}
}

func TestServerLeaseCounter_FallbackCount(t *testing.T) {
	validLease := proxytesting.LeaseTemplate{
		DurationSecs:     1000,
		TimeSinceAcquire: time.Second,
		Labels:           map[string]string{"label": "value"},
	}
	invalidLease := proxytesting.LeaseTemplate{
		DurationSecs:     1000,
		TimeSinceAcquire: time.Second * 10000,
		Labels:           map[string]string{"label": "value"},
	}

	pc := clocktesting.NewFakeClock(time.Now())
	leases := []runtime.Object{proxytesting.NewLeaseFromTemplate(pc, validLease), proxytesting.NewLeaseFromTemplate(pc, validLease), proxytesting.NewLeaseFromTemplate(pc, validLease), proxytesting.NewLeaseFromTemplate(pc, invalidLease)}

	k8sClient := fake.NewSimpleClientset(leases...)
	callShouldFail := true

	selector, _ := labels.Parse("label=value")

	counter := NewServerLeaseCounter(pc, k8sClient, selector, "")

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
