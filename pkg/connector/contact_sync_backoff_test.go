// mautrix-imessage - A Matrix-iMessage puppeting bridge.

package connector

import (
	"testing"
	"time"
)

func TestContactSyncBackoff(t *testing.T) {
	base, max := 15*time.Minute, 6*time.Hour
	want := []time.Duration{
		base,             // 0 failures
		base,             // 1
		30 * time.Minute, // 2
		time.Hour,        // 3
		2 * time.Hour,    // 4
		4 * time.Hour,    // 5
		6 * time.Hour,    // 6 (capped)
		6 * time.Hour,    // 7
		6 * time.Hour,    // 50
	}
	failures := []int{0, 1, 2, 3, 4, 5, 6, 7, 50}
	for i, f := range failures {
		if got := contactSyncBackoff(base, max, f); got != want[i] {
			t.Errorf("contactSyncBackoff(%d) = %v, want %v", f, got, want[i])
		}
	}
}

func TestContactSyncSchedule(t *testing.T) {
	s := newContactSyncSchedule(2*time.Minute, time.Hour)
	if got := s.next(); got != 2*time.Minute {
		t.Fatalf("initial next = %v, want 2m", got)
	}
	if got := s.failed(); got != 2*time.Minute {
		t.Errorf("1st failure = %v, want 2m", got)
	}
	if got := s.failed(); got != 4*time.Minute {
		t.Errorf("2nd failure = %v, want 4m", got)
	}
	if got := s.next(); got != 4*time.Minute {
		t.Errorf("next after 2 failures = %v, want 4m", got)
	}
	for i := 0; i < 10; i++ {
		s.failed()
	}
	if got := s.next(); got != time.Hour {
		t.Errorf("capped next = %v, want 1h", got)
	}
	if !s.succeeded() {
		t.Error("succeeded() after failures should report recovery")
	}
	if s.succeeded() {
		t.Error("succeeded() with no failures should not report recovery")
	}
	if got := s.next(); got != 2*time.Minute {
		t.Errorf("next after reset = %v, want 2m", got)
	}
}
