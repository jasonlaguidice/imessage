// mautrix-imessage - A Matrix-iMessage puppeting bridge.
//
// Exponential backoff for the contact sync loops (periodicCloudContactSync
// and retryCloudContacts). Ported from upstream corten-matrix 993ec4e3.

package connector

import (
	"errors"
	"time"
)

// isContactSyncThrottled reports whether a contact sync failed because Apple
// rate-limited the iCloud CardDAV endpoint (HTTP 403/429).
func isContactSyncThrottled(err error) bool {
	return errors.Is(err, errICloudContactsThrottled)
}

// contactSyncBackoff returns base doubled per consecutive failure after the
// first, capped at max: 15m → 30m → 1h → 2h → 4h → 6h. It keeps a throttled
// (403/429) contacts sync from retrying at its fixed cadence and compounding
// the rate limit.
func contactSyncBackoff(base, max time.Duration, failures int) time.Duration {
	if failures <= 1 {
		return base
	}
	d := base
	for i := 1; i < failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return d
}

// contactSyncSchedule tracks consecutive failures of one contact sync loop and
// yields the delay before its next attempt. Not safe for concurrent use; each
// loop owns its own schedule.
type contactSyncSchedule struct {
	base, max time.Duration
	failures  int
}

func newContactSyncSchedule(base, max time.Duration) *contactSyncSchedule {
	return &contactSyncSchedule{base: base, max: max}
}

// failed records a failed attempt and returns the backed-off next delay.
func (s *contactSyncSchedule) failed() time.Duration {
	s.failures++
	return contactSyncBackoff(s.base, s.max, s.failures)
}

// succeeded resets the schedule and reports whether it was recovering from
// failures, so the caller can log the recovery once.
func (s *contactSyncSchedule) succeeded() (recovered bool) {
	recovered = s.failures > 0
	s.failures = 0
	return recovered
}

// next returns the delay before the next attempt at the current failure count.
func (s *contactSyncSchedule) next() time.Duration {
	return contactSyncBackoff(s.base, s.max, s.failures)
}
