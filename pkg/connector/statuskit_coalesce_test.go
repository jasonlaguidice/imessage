package connector

import (
	"testing"
	"time"
)

// TestPresenceSubscribeDelay covers the trailing-edge coalescer's delay
// computation: a key at the start of a burst waits the full debounce window, a
// burst that has already run for a while is capped by presenceSubscribeMaxWait
// (measured from the FIRST key of the burst), and a burst older than the cap
// fires immediately.
func TestPresenceSubscribeDelay(t *testing.T) {
	now := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		firstAt time.Time
		want    time.Duration
	}{
		{
			name:    "first key of a burst waits the whole debounce window",
			firstAt: now,
			want:    3 * time.Second,
		},
		{
			name:    "10s into a burst still under the max-wait cap",
			firstAt: now.Add(-10 * time.Second),
			want:    3 * time.Second,
		},
		{
			name:    "28s into a burst is capped by the remaining max-wait",
			firstAt: now.Add(-28 * time.Second),
			want:    2 * time.Second,
		},
		{
			name:    "31s into a burst is past the cap and fires now",
			firstAt: now.Add(-31 * time.Second),
			want:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := presenceSubscribeDelay(now, tc.firstAt); got != tc.want {
				t.Fatalf("presenceSubscribeDelay(%v, %v) = %v, want %v", now, tc.firstAt, got, tc.want)
			}
		})
	}
}
