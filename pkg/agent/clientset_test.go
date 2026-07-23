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
	"errors"
	"testing"
	"time"
)

type FakeServerCounter struct {
	count int
}

func (f *FakeServerCounter) Count() int {
	return f.count
}

func TestServerCount(t *testing.T) {
	testCases := []struct {
		name              string
		serverCountSource string
		leaseCounter      ServerCounter
		responseCount     int
		want              int
	}{
		{
			name:              "higher from response",
			serverCountSource: "max",
			responseCount:     42,
			leaseCounter:      &FakeServerCounter{24},
			want:              42,
		},
		{
			name:              "higher from leases",
			serverCountSource: "max",
			responseCount:     3,
			leaseCounter:      &FakeServerCounter{6},
			want:              6,
		},
		{
			name:              "both zero",
			serverCountSource: "max",
			responseCount:     0,
			leaseCounter:      &FakeServerCounter{0},
			want:              1,
		},

		{
			name:              "response picked by default when no lease counter",
			serverCountSource: "default",
			responseCount:     3,
			leaseCounter:      nil,
			want:              3,
		},
		{
			name:              "lease counter always picked when present",
			serverCountSource: "default",
			responseCount:     6,
			leaseCounter:      &FakeServerCounter{3},
			want:              3,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			cs := &ClientSet{
				clients:           make(map[string]*Client),
				leaseCounter:      tc.leaseCounter,
				serverCountSource: tc.serverCountSource,
			}
			cs.lastReceivedServerCount = tc.responseCount
			if got := cs.ServerCount(); got != tc.want {
				t.Errorf("cs.ServerCount() = %v, want: %v", got, tc.want)
			}
		})
	}

}

func TestNextResyncDuration(t *testing.T) {
	const (
		syncInterval    = 1 * time.Second
		syncIntervalCap = 10 * time.Second
	)

	dup := &DuplicateServerError{ServerID: "server-1"}
	realErr := errors.New("dial tcp: connection refused")

	// wait.Jitter(base, 0.1) yields a value in [base, base*1.1).
	expectJitter := func(base time.Duration) func(*testing.T, time.Duration) {
		return func(t *testing.T, d time.Duration) {
			t.Helper()
			upper := base + time.Duration(0.1*float64(base))
			if d < base || d > upper {
				t.Errorf("duration = %v, want within [%v, %v]", d, base, upper)
			}
		}
	}
	expectZero := func(t *testing.T, d time.Duration) {
		t.Helper()
		if d != 0 {
			t.Errorf("duration = %v, want immediate retry (0)", d)
		}
	}
	expectPositive := func(t *testing.T, d time.Duration) {
		t.Helper()
		if d <= 0 {
			t.Errorf("duration = %v, want a positive backoff", d)
		}
	}

	testCases := []struct {
		name                       string
		syncImmediatelyOnDuplicate bool
		err                        error
		serverCount                int
		clientsCount               int
		check                      func(*testing.T, time.Duration)
	}{
		{
			name:         "success resets to sync interval",
			err:          nil,
			serverCount:  3,
			clientsCount: 3,
			check:        expectJitter(syncInterval),
		},
		{
			name:         "real error backs off",
			err:          realErr,
			serverCount:  3,
			clientsCount: 1,
			check:        expectPositive,
		},
		{
			name:         "duplicate with enough clients backs off",
			err:          dup,
			serverCount:  3,
			clientsCount: 3,
			check:        expectPositive,
		},
		{
			name:         "duplicate needing more clients waits a sync interval by default",
			err:          dup,
			serverCount:  3,
			clientsCount: 1,
			check:        expectJitter(syncInterval),
		},
		{
			name:                       "duplicate needing more clients retries immediately when enabled",
			syncImmediatelyOnDuplicate: true,
			err:                        dup,
			serverCount:                3,
			clientsCount:               1,
			check:                      expectZero,
		},
		{
			name:                       "flag does not shortcut backoff when clients are sufficient",
			syncImmediatelyOnDuplicate: true,
			err:                        dup,
			serverCount:                3,
			clientsCount:               3,
			check:                      expectPositive,
		},
		{
			name:                       "flag waits a sync interval when server count is unknown",
			syncImmediatelyOnDuplicate: true,
			err:                        dup,
			serverCount:                0,
			clientsCount:               1,
			check:                      expectJitter(syncInterval),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := &ClientSet{
				clients:                    make(map[string]*Client),
				syncInterval:               syncInterval,
				syncIntervalCap:            syncIntervalCap,
				syncImmediatelyOnDuplicate: tc.syncImmediatelyOnDuplicate,
			}
			backoff := cs.resetBackoff()
			got, gotBackoff, _ := cs.nextResyncDuration(backoff, 0, tc.err, tc.serverCount, tc.clientsCount)
			if gotBackoff == nil {
				t.Fatalf("nextResyncDuration returned a nil backoff")
			}
			tc.check(t, got)
		})
	}
}

// TestNextResyncDurationBackoffGrows verifies that the error and
// duplicate-with-enough-clients paths use exponential backoff (backoff.Step),
// distinguishing them from the constant reset+jitter path: feeding the returned
// backoff back in must yield a strictly growing delay.
func TestNextResyncDurationBackoffGrows(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		serverCount  int
		clientsCount int
	}{
		{name: "real error", err: errors.New("connection refused"), serverCount: 3, clientsCount: 1},
		{name: "duplicate with enough clients", err: &DuplicateServerError{ServerID: "s"}, serverCount: 3, clientsCount: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := &ClientSet{
				clients:         make(map[string]*Client),
				syncInterval:    1 * time.Second,
				syncIntervalCap: 60 * time.Second,
			}
			backoff := cs.resetBackoff()
			var prev time.Duration
			for i := 0; i < 3; i++ {
				var d time.Duration
				d, backoff, _ = cs.nextResyncDuration(backoff, 0, tc.err, tc.serverCount, tc.clientsCount)
				if i > 0 && d <= prev {
					t.Errorf("step %d: duration %v did not grow beyond previous %v (backoff not applied)", i, d, prev)
				}
				prev = d
			}
		})
	}
}

// TestNextResyncDurationImmediateRetryBudget verifies that immediate retries are
// bounded: with the flag enabled and a permanent deficit, only serverCount
// consecutive zero-delay retries are allowed before falling back to the delay,
// and progress resets the budget.
func TestNextResyncDurationImmediateRetryBudget(t *testing.T) {
	const serverCount = 3
	dup := &DuplicateServerError{ServerID: "s"}
	cs := &ClientSet{
		clients:                    make(map[string]*Client),
		syncInterval:               1 * time.Second,
		syncIntervalCap:            10 * time.Second,
		syncImmediatelyOnDuplicate: true,
	}
	backoff := cs.resetBackoff()

	// A permanent deficit (clientsCount stays below serverCount) must yield
	// exactly serverCount immediate (zero) retries, then a positive delay.
	immediateRetries := 0
	for i := 0; i < serverCount; i++ {
		var d time.Duration
		d, backoff, immediateRetries = cs.nextResyncDuration(backoff, immediateRetries, dup, serverCount, 1)
		if d != 0 {
			t.Fatalf("attempt %d: duration = %v, want immediate retry (0)", i, d)
		}
	}
	d, backoff, immediateRetries := cs.nextResyncDuration(backoff, immediateRetries, dup, serverCount, 1)
	if d <= 0 {
		t.Fatalf("after budget spent: duration = %v, want a positive fallback delay", d)
	}
	if immediateRetries != serverCount {
		t.Fatalf("spent budget was not preserved: immediateRetries = %d, want %d", immediateRetries, serverCount)
	}

	// A successful connection resets the budget so immediate retries resume.
	_, backoff, immediateRetries = cs.nextResyncDuration(backoff, immediateRetries, nil, serverCount, serverCount)
	if immediateRetries != 0 {
		t.Fatalf("progress did not reset the budget: immediateRetries = %d, want 0", immediateRetries)
	}
	d, _, _ = cs.nextResyncDuration(backoff, immediateRetries, dup, serverCount, 1)
	if d != 0 {
		t.Fatalf("after reset: duration = %v, want immediate retry (0)", d)
	}
}
