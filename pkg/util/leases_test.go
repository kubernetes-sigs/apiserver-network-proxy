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
package util

import (
	"testing"
	"time"

	clocktesting "k8s.io/utils/clock/testing"

	proxytesting "sigs.k8s.io/apiserver-network-proxy/pkg/testing"
)

func TestIsLeaseValid(t *testing.T) {
	testCases := []struct {
		name     string
		template proxytesting.LeaseTemplate
		want     bool
	}{
		{
			name: "freshly acquired lease is valid",
			template: proxytesting.LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: time.Second,
			},
			want: true,
		}, {
			name: "freshly renewed lease is valid",
			template: proxytesting.LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: 10000 * time.Second,
				TimeSinceRenew:   time.Second,
			},
			want: true,
		}, {
			name: "lease with neither acquisition nor renewal time is invalid",
			template: proxytesting.LeaseTemplate{
				DurationSecs: 1000,
			},
			want: false,
		}, {
			name: "expired lease (only acquired) is invalid",
			template: proxytesting.LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: 10000 * time.Second,
			},
			want: false,
		}, {
			name: "expired lease (acquired and renewed) is invalid",
			template: proxytesting.LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: 10000 * time.Second,
				TimeSinceRenew:   9000 * time.Second,
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			pc := clocktesting.NewFakePassiveClock(now)

			lease := proxytesting.NewLeaseFromTemplate(pc, tc.template)

			pc.SetTime(now.Add(time.Millisecond))

			got := IsLeaseValid(pc, *lease)
			if got != tc.want {
				t.Errorf("incorrect lease validity (got: %v, want: %v)", got, tc.want)
			}
		})
	}
}
