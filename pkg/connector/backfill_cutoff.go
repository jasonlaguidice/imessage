// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// keepPastBackfillCutoff protects messages that share the anchor's
// millisecond from bridgev2's backfill cutoff. Since mautrix-go v0.30 the
// cutoff drops forward messages whose timestamp is not strictly after the
// anchor (and backward messages not strictly before it); our CloudKit and
// chat.db pagination is ordered by (timestamp, guid), so distinct messages in
// the anchor's millisecond are real and must not be lost. The anchor message
// itself (same ID) is removed. Others are moved a few nanoseconds past the
// anchor, keeping their relative order: Matrix event timestamps are in
// milliseconds, so what users see does not change.
func keepPastBackfillCutoff(messages []*bridgev2.BackfillMessage, anchor *database.Message, forward bool) []*bridgev2.BackfillMessage {
	if anchor == nil || len(messages) == 0 {
		return messages
	}

	// Drop the anchor itself (bridgev2 dedups it by ID anyway) and any nil
	// entry, keeping chronological order.
	filtered := make([]*bridgev2.BackfillMessage, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || msg.ID == anchor.ID {
			continue
		}
		filtered = append(filtered, msg)
	}

	if forward {
		// Leading run of messages at-or-before the anchor: push each one a
		// nanosecond past the anchor, oldest first, so the relative order of
		// the run is preserved and all of it survives the cutoff.
		for i, msg := range filtered {
			if msg.Timestamp.After(anchor.Timestamp) {
				break
			}
			filtered[i].Timestamp = anchor.Timestamp.Add(time.Duration(i+1) * time.Nanosecond)
		}
	} else {
		// Trailing run of messages at-or-after the anchor: work backwards
		// from the newest message and pull each one a nanosecond below the
		// anchor, so the newest ends up closest to the anchor and the
		// chronological order of the run is preserved.
		for k := 0; k < len(filtered); k++ {
			i := len(filtered) - 1 - k
			msg := filtered[i]
			if msg.Timestamp.Before(anchor.Timestamp) {
				break
			}
			filtered[i].Timestamp = anchor.Timestamp.Add(-time.Duration(k+1) * time.Nanosecond)
		}
	}

	return filtered
}
