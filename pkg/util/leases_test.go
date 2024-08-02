package util

import (
	"testing"
	"time"
)

type controlledTime struct {
	t time.Time
}

func (ct *controlledTime) Now() time.Time {
	return ct.t
}

func (ct *controlledTime) Advance(d time.Duration) {
	ct.t = ct.t.Add(d)
}

func TestIsLeaseValid(t *testing.T) {
	testCases := []struct {
		name     string
		template LeaseTemplate
		want     bool
	}{
		{
			name: "freshly acquired lease is valid",
			template: LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: time.Second,
			},
			want: true,
		}, {
			name: "freshly renewed lease is valid",
			template: LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: 10000 * time.Second,
				TimeSinceRenew:   time.Second,
			},
			want: true,
		}, {
			name: "lease with neither acquisition nor renewal time is invalid",
			template: LeaseTemplate{
				DurationSecs: 1000,
			},
			want: false,
		}, {
			name: "expired lease (only acquired) is invalid",
			template: LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: 10000 * time.Second,
			},
			want: false,
		}, {
			name: "expired lease (acquired and renewed) is invalid",
			template: LeaseTemplate{
				DurationSecs:     1000,
				TimeSinceAcquire: 10000 * time.Second,
				TimeSinceRenew:   9000 * time.Second,
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ct := &controlledTime{t: time.Now()}
			timeNow = ct.Now
			lease := NewLeaseFromTemplate(tc.template)
			ct.Advance(time.Millisecond)

			got := IsLeaseValid(*lease)
			if got != tc.want {
				t.Errorf("incorrect lease validity (got: %v, want: %v)", got, tc.want)
			}
		})
	}
}
