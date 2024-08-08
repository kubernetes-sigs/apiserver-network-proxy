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
package leases

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	clocktesting "k8s.io/utils/clock/testing"

	proxytesting "sigs.k8s.io/apiserver-network-proxy/pkg/testing"
)

func TestGarbageCollectionController(t *testing.T) {
	testCases := []struct {
		name           string
		template       proxytesting.LeaseTemplate
		selector       string
		expectDeletion bool
	}{
		{
			name: "does not delete valid acquired lease matching selector",
			template: proxytesting.LeaseTemplate{
				Labels:           map[string]string{"some": "label"},
				TimeSinceAcquire: 2 * time.Minute,
				DurationSecs:     1000,
			},
			selector:       "some=label",
			expectDeletion: false,
		}, {
			name: "does not delete valid renewed lease matching selector",
			template: proxytesting.LeaseTemplate{
				Labels:           map[string]string{"some": "label"},
				TimeSinceAcquire: 10 * time.Minute,
				TimeSinceRenew:   time.Minute,
				DurationSecs:     120,
			},
			selector:       "some=label",
			expectDeletion: false,
		}, {
			name: "does not delete expired lease not matching selector",
			template: proxytesting.LeaseTemplate{
				Labels:           map[string]string{"another": "label"},
				TimeSinceAcquire: 2 * time.Minute,
				TimeSinceRenew:   time.Minute,
				DurationSecs:     1000,
			},
			selector:       "some=label",
			expectDeletion: false,
		}, {
			name: "deletes expired lease matching selector",
			template: proxytesting.LeaseTemplate{
				Labels:           map[string]string{"some": "label"},
				TimeSinceAcquire: 4 * time.Minute,
				TimeSinceRenew:   3 * time.Minute,
				DurationSecs:     1,
			},
			selector:       "some=label",
			expectDeletion: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			pc := clocktesting.NewFakePassiveClock(now)
			lease := proxytesting.NewLeaseFromTemplate(pc, tc.template)
			k8sClient := fake.NewSimpleClientset(lease)

			controller := NewGarbageCollectionController(pc, k8sClient, "", 10*time.Millisecond, tc.selector)

			go controller.Run(context.Background())

			pc.SetTime(now.Add(100 * time.Millisecond))
			time.Sleep(100 * time.Millisecond)

			gotLease, err := k8sClient.CoordinationV1().Leases("").Get(context.Background(), lease.Name, metav1.GetOptions{})
			if errors.IsNotFound(err) && tc.expectDeletion {
				return
			} else if err != nil {
				t.Fatalf("error while getting lease: %v", err)
			} else if tc.expectDeletion {
				t.Errorf("lease should have been deleted, instead got: %v", gotLease)
			}
		})
	}

}
