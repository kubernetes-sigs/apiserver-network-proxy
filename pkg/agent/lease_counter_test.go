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

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	coordinationv1lister "k8s.io/client-go/listers/coordination/v1"
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
			leaseInformer := NewLeaseInformerWithMetrics(k8sClient, "", time.Millisecond*100)
			leaseInformer.Run(context.TODO().Done())
			leaseLister := coordinationv1lister.NewLeaseLister(leaseInformer.GetIndexer())

			counter := NewServerLeaseCounter(pc, leaseLister, selector)

			got := counter.Count()
			if tc.want != got {
				t.Errorf("incorrect server count (got: %v, want: %v)", got, tc.want)
			}
		})
	}
}

type fakeLeaseLister struct {
	LeaseList []*coordinationv1.Lease
	Err       error
}

func (lister fakeLeaseLister) List(selector labels.Selector) (ret []*coordinationv1.Lease, err error) {
	if lister.Err != nil {
		return nil, lister.Err
	}

	for _, lease := range lister.LeaseList {
		if selector.Matches(labels.Set(lease.Labels)) {
			ret = append(ret, lease)
		}
	}
	return ret, nil
}

func (lister fakeLeaseLister) Leases(_ string) coordinationv1lister.LeaseNamespaceLister {
	panic("should not be used")
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
	leases := []*coordinationv1.Lease{proxytesting.NewLeaseFromTemplate(pc, validLease), proxytesting.NewLeaseFromTemplate(pc, validLease), proxytesting.NewLeaseFromTemplate(pc, validLease), proxytesting.NewLeaseFromTemplate(pc, invalidLease)}
	selector, _ := labels.Parse("label=value")
	leaseLister := fakeLeaseLister{LeaseList: leases}

	counter := NewServerLeaseCounter(pc, leaseLister, selector)

	got := counter.Count()
	leaseLister.Err = fmt.Errorf("fake lease listing error")
	if got != 1 {
		t.Errorf("lease counter did not return fallback count on leaseLister error (got: %v, want: 1)", got)
	}

	// Second call should return the actual count (3) upon leaseClient success.
	leaseLister.Err = nil
	actualCount := 3
	got = counter.Count()
	if got != actualCount {
		t.Errorf("lease counter did not return actual count on leaseClient success (got: %v, want: %v)", got, actualCount)
	}

	// Third call should return updated fallback count (3) upon leaseClient failure.
	leaseLister.Err = fmt.Errorf("fake lease listing error")
	got = counter.Count()
	if got != actualCount {
		t.Errorf("lease counter did not update fallback count after leaseClient success, returned incorrect count on subsequent leaseClient error (got: %v, want: %v)", got, actualCount)
	}
}
