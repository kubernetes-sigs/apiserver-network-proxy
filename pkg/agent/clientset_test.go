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

func TestAggregateServerCounter(t *testing.T) {
	testCases := []struct {
		name            string
		source          string
		leaseCounter    ServerCounter
		responseCounter ServerCounter
		want            int
	}{
		{
			name:            "max: higher from response",
			source:          "max",
			leaseCounter:    &FakeServerCounter{count: 24},
			responseCounter: &FakeServerCounter{count: 42},
			want:            42,
		},
		{
			name:            "max: higher from leases",
			source:          "max",
			leaseCounter:    &FakeServerCounter{count: 6},
			responseCounter: &FakeServerCounter{count: 3},
			want:            6,
		},
		{
			name:            "max: both zero",
			source:          "max",
			leaseCounter:    &FakeServerCounter{count: 0},
			responseCounter: &FakeServerCounter{count: 0},
			want:            1, // fallback
		},
		{
			name:            "default: lease counter is nil",
			source:          "default",
			leaseCounter:    nil,
			responseCounter: &FakeServerCounter{count: 3},
			want:            3,
		},
		{
			name:            "default: lease counter is present",
			source:          "default",
			leaseCounter:    &FakeServerCounter{count: 3},
			responseCounter: &FakeServerCounter{count: 6},
			want:            3, // lease count is preferred
		},
		{
			name:            "default: lease count is zero",
			source:          "default",
			leaseCounter:    &FakeServerCounter{count: 0},
			responseCounter: &FakeServerCounter{count: 6},
			want:            1, // fallback
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			agg := NewAggregateServerCounter(tc.leaseCounter, tc.responseCounter, tc.source)
			if got := agg.Count(); got != tc.want {
				t.Errorf("agg.Count() = %v, want: %v", got, tc.want)
			}
		})
	}
}

func TestResponseBasedCounter(t *testing.T) {
	testCases := []struct {
		name          string
		responseCount int
		want          int
	}{
		{
			name:          "non-zero count",
			responseCount: 5,
			want:          5,
		},
		{
			name:          "zero count",
			responseCount: 0,
			want:          1, // fallback
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := &ClientSet{lastReceivedServerCount: tc.responseCount}
			rbc := NewResponseBasedCounter(cs)
			if got := rbc.Count(); got != tc.want {
				t.Errorf("rbc.Count() = %v, want: %v", got, tc.want)
			}
		})
	}
}
