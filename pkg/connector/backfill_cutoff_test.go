package connector

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// mautrixCutoffForward mirrors bridgev2.Portal.cutoffMessages' forward branch
// (mautrix-go v0.31): the leading run of messages whose ID equals the anchor's
// or whose timestamp is not strictly after the anchor's is dropped.
func mautrixCutoffForward(messages []*bridgev2.BackfillMessage, anchor *database.Message) []*bridgev2.BackfillMessage {
	cutoff := -1
	for i, msg := range messages {
		if msg.ID == anchor.ID || !msg.Timestamp.After(anchor.Timestamp) {
			cutoff = i
		} else {
			break
		}
	}
	if cutoff != -1 {
		return messages[cutoff+1:]
	}
	return messages
}

// mautrixCutoffBackward mirrors bridgev2.Portal.cutoffMessages' backward
// branch: the trailing run of messages whose ID equals the anchor's or whose
// timestamp is not strictly before the anchor's is dropped.
func mautrixCutoffBackward(messages []*bridgev2.BackfillMessage, anchor *database.Message) []*bridgev2.BackfillMessage {
	cutoff := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].ID == anchor.ID || !messages[i].Timestamp.Before(anchor.Timestamp) {
			cutoff = i
		} else {
			break
		}
	}
	if cutoff != -1 {
		return messages[:cutoff]
	}
	return messages
}

func TestKeepPastBackfillCutoffNilAnchor(t *testing.T) {
	T := time.UnixMilli(1700000000000).UTC()
	in := []*bridgev2.BackfillMessage{
		{ID: networkid.MessageID("b"), Timestamp: T},
		{ID: networkid.MessageID("c"), Timestamp: T},
	}
	out := keepPastBackfillCutoff(in, nil, true)
	if len(out) != 2 {
		t.Fatalf("nil anchor must leave the slice untouched, got %d messages", len(out))
	}
	for i, msg := range out {
		if !msg.Timestamp.Equal(in[i].Timestamp) {
			t.Fatalf("message %d timestamp changed with a nil anchor: %v", i, msg.Timestamp)
		}
	}
}

func TestKeepPastBackfillCutoffForward(t *testing.T) {
	T := time.UnixMilli(1700000000000).UTC()
	anchor := &database.Message{ID: networkid.MessageID("a"), Timestamp: T}
	in := []*bridgev2.BackfillMessage{
		{ID: networkid.MessageID("b"), Timestamp: T},
		{ID: networkid.MessageID("c"), Timestamp: T},
		{ID: networkid.MessageID("d"), Timestamp: T.Add(time.Millisecond)},
	}
	out := keepPastBackfillCutoff(in, anchor, true)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	want := []time.Time{
		T.Add(time.Nanosecond),
		T.Add(2 * time.Nanosecond),
		T.Add(time.Millisecond),
	}
	for i, msg := range out {
		if !msg.Timestamp.Equal(want[i]) {
			t.Fatalf("message %d (%s): expected timestamp %v, got %v", i, msg.ID, want[i], msg.Timestamp)
		}
		if !msg.Timestamp.After(T) {
			t.Fatalf("message %d (%s): timestamp %v is not strictly after the anchor", i, msg.ID, msg.Timestamp)
		}
	}
}

func TestKeepPastBackfillCutoffForwardDropsAnchorID(t *testing.T) {
	T := time.UnixMilli(1700000000000).UTC()
	anchor := &database.Message{ID: networkid.MessageID("a"), Timestamp: T}
	in := []*bridgev2.BackfillMessage{
		{ID: networkid.MessageID("a"), Timestamp: T},
		{ID: networkid.MessageID("b"), Timestamp: T},
	}
	out := keepPastBackfillCutoff(in, anchor, true)
	if len(out) != 1 {
		t.Fatalf("expected the anchor itself to be dropped, got %d messages", len(out))
	}
	if out[0].ID != networkid.MessageID("b") {
		t.Fatalf("expected only message b, got %q", out[0].ID)
	}
	if !out[0].Timestamp.After(T) {
		t.Fatalf("message b was not moved past the anchor: %v", out[0].Timestamp)
	}
}

func TestKeepPastBackfillCutoffBackward(t *testing.T) {
	T := time.UnixMilli(1700000000000).UTC()
	anchor := &database.Message{ID: networkid.MessageID("a"), Timestamp: T}
	in := []*bridgev2.BackfillMessage{
		{ID: networkid.MessageID("x"), Timestamp: T.Add(-time.Millisecond)},
		{ID: networkid.MessageID("y"), Timestamp: T},
		{ID: networkid.MessageID("z"), Timestamp: T},
	}
	out := keepPastBackfillCutoff(in, anchor, false)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	want := []time.Time{
		T.Add(-time.Millisecond),
		T.Add(-2 * time.Nanosecond),
		T.Add(-time.Nanosecond),
	}
	for i, msg := range out {
		if !msg.Timestamp.Equal(want[i]) {
			t.Fatalf("message %d (%s): expected timestamp %v, got %v", i, msg.ID, want[i], msg.Timestamp)
		}
	}
	if !out[1].Timestamp.Before(out[2].Timestamp) {
		t.Fatalf("chronological order broken: y (%v) must stay before z (%v)", out[1].Timestamp, out[2].Timestamp)
	}
	if !out[2].Timestamp.Before(T) {
		t.Fatalf("z (%v) was not moved below the anchor", out[2].Timestamp)
	}
}

// TestKeepPastBackfillCutoffSurvivesMautrixCutoff feeds the helper's output
// through mautrix-go v0.31's cutoff rule and asserts nothing is dropped.
func TestKeepPastBackfillCutoffSurvivesMautrixCutoff(t *testing.T) {
	T := time.UnixMilli(1700000000000).UTC()
	anchor := &database.Message{ID: networkid.MessageID("a"), Timestamp: T}

	forwardIn := []*bridgev2.BackfillMessage{
		{ID: networkid.MessageID("b"), Timestamp: T},
		{ID: networkid.MessageID("c"), Timestamp: T},
		{ID: networkid.MessageID("d"), Timestamp: T.Add(time.Millisecond)},
	}
	forward := keepPastBackfillCutoff(forwardIn, anchor, true)
	for _, msg := range forward {
		if msg.ID == anchor.ID {
			t.Fatalf("forward: anchor id %q survived the cutoff", msg.ID)
		}
		if !msg.Timestamp.After(anchor.Timestamp) {
			t.Fatalf("forward: message %q at %v would be cut off", msg.ID, msg.Timestamp)
		}
	}
	if got := mautrixCutoffForward(forward, anchor); len(got) != len(forward) {
		t.Fatalf("forward: mautrix cutoff kept %d of %d messages", len(got), len(forward))
	}

	backwardIn := []*bridgev2.BackfillMessage{
		{ID: networkid.MessageID("x"), Timestamp: T.Add(-time.Millisecond)},
		{ID: networkid.MessageID("y"), Timestamp: T},
		{ID: networkid.MessageID("z"), Timestamp: T},
	}
	backward := keepPastBackfillCutoff(backwardIn, anchor, false)
	for _, msg := range backward {
		if msg.ID == anchor.ID {
			t.Fatalf("backward: anchor id %q survived the cutoff", msg.ID)
		}
		if !msg.Timestamp.Before(anchor.Timestamp) {
			t.Fatalf("backward: message %q at %v would be cut off", msg.ID, msg.Timestamp)
		}
	}
	if got := mautrixCutoffBackward(backward, anchor); len(got) != len(backward) {
		t.Fatalf("backward: mautrix cutoff kept %d of %d messages", len(got), len(backward))
	}
}
