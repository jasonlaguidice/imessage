// mautrix-imessage - A Matrix-iMessage puppeting bridge.

package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

func TestGhostProfileSnapshot(t *testing.T) {
	ghost := &bridgev2.Ghost{Ghost: &database.Ghost{
		Name:        "Alice",
		AvatarMXC:   "mxc://example.org/a",
		Identifiers: []string{"tel:+15555550123"},
	}}
	before := snapshotGhostProfile(ghost)

	ghost.NameSet = true // bookkeeping only; not a visible change
	if !before.equal(snapshotGhostProfile(ghost)) {
		t.Error("bookkeeping-only change should not count as a profile change")
	}
	steps := []struct {
		name   string
		mutate func()
	}{
		{"name", func() { ghost.Name = "Alice B" }},
		{"avatar", func() { ghost.AvatarMXC = "mxc://example.org/b" }},
		{"identifiers", func() { ghost.Identifiers = append(ghost.Identifiers, "tel:+15555550199") }},
	}
	for _, step := range steps {
		step.mutate()
		if before.equal(snapshotGhostProfile(ghost)) {
			t.Errorf("%s change not detected", step.name)
		}
		before = snapshotGhostProfile(ghost)
	}
}
