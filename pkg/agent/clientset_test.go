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
		serverCountSource string
		leaseCounter ServerCounter
		responseCount int
		want int
	} {
		{
			name: "higher from response",
			serverCountSource: "max",
			responseCount: 42,
			leaseCounter: &FakeServerCounter{24},
			want: 42,
		},
		{
			name: "higher from leases",
			serverCountSource: "max",
			responseCount: 3,
			leaseCounter: &FakeServerCounter{6},
			want: 6,
		},
		{
			name: "both zero",
			serverCountSource: "max",
			responseCount: 0,
			leaseCounter: &FakeServerCounter{0},
			want: 1,
		},

		{
			name: "response picked by default when no lease counter",
			serverCountSource: "default",
			responseCount: 3,
			leaseCounter: nil,
			want: 3,
		},
		{
			name: "lease counter always picked when present",
			serverCountSource: "default",
			responseCount: 6,
			leaseCounter: &FakeServerCounter{3},
			want: 3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			cs := &ClientSet{
				clients: make(map[string]*Client),
				leaseCounter: tc.leaseCounter,
				serverCountSource: tc.serverCountSource,

			}
			cs.lastReceivedServerCount = tc.responseCount
			if got := cs.ServerCount(); got != tc.want {
				t.Errorf("cs.ServerCount() = %v, want: %v", got, tc.want)
			}
		})
	}

}
