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
	"testing"
)

type FakeServerCounter struct {
	count int
}

func (f *FakeServerCounter) Count() int {
	return f.count
}

func TestServerCount(t *testing.T) {
	testCases := []struct{
		name string
		responseCount int
		leaseCount int
		want int
	} {
		{
			name: "higher from response",
			responseCount: 42,
			leaseCount: 24,
			want: 42,
		},
		{
			name: "higher from leases",
			responseCount: 3,
			leaseCount: 6,
			want: 6,
		},
		{
			name: "both zero",
			responseCount: 0,
			leaseCount: 0,
			want: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			lc := &FakeServerCounter{
				count: tc.leaseCount,
			}

			cs := &ClientSet{
				clients: make(map[string]*Client),
				leaseCounter: lc,
			}
			cs.lastReceivedServerCount = tc.responseCount
			if got := cs.ServerCount(); got != tc.want {
				t.Errorf("cs.ServerCount() = %v, want: %v", got, tc.want)
			}
		})
	}

}
