// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"context"
	"maps"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func TestLikePrefixPattern(t *testing.T) {
	for in, want := range map[string]string{
		"statuskit.alias_portal.a@b.com.": "statuskit.alias!_portal.a@b.com.%",
		"a%b!c_d":                         "a!%b!!c!_d%",
		"":                                "%",
	} {
		if got := likePrefixPattern(in); got != want {
			t.Errorf("likePrefixPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadLoginAliasPortals checks that StatusKit alias→portal hydration
// loads exactly one login's rows: not those of a login whose ID matches an
// unescaped LIKE pattern ('_' wildcard), differs only by case, or extends
// this login's ID with a '.', and not the same keys under another bridge ID.
func TestLoadLoginAliasPortals(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		createTestKVStore(t, db)
		const bridgeID = networkid.BridgeID("imessage")
		const otherBridgeID = networkid.BridgeID("other")

		logins := []networkid.UserLoginID{
			"a_b@x.com",    // contains a LIKE wildcard
			"a_b@x.com.au", // extends the first login's ID with a '.'
			"aXb@x.com",    // matches "a_b@x.com." if '_' were unescaped
			"A_B@X.COM",    // differs only by case (SQLite LIKE ignores ASCII case)
		}
		aliases := []string{"tel:+15551234567", "mailto:x_y%z@example.com"}

		set := func(bridge networkid.BridgeID, key, value string) {
			t.Helper()
			if _, err := db.Exec(ctx, `INSERT INTO kv_store (bridge_id, key, value) VALUES ($1, $2, $3)`, bridge, key, value); err != nil {
				t.Fatalf("insert %q: %v", key, err)
			}
		}
		want := make(map[networkid.UserLoginID]map[string]networkid.PortalID)
		for _, login := range logins {
			want[login] = make(map[string]networkid.PortalID)
			for _, alias := range aliases {
				portal := networkid.PortalID(string(login) + "|" + alias)
				set(bridgeID, string(statusKitLoginKey(login, statusKitAliasPortalKeyPrefix, alias)), string(portal))
				set(otherBridgeID, string(statusKitLoginKey(login, statusKitAliasPortalKeyPrefix, alias)), "other-bridge")
				// Same login, other per-login prefixes: not alias rows.
				set(bridgeID, string(statusKitLoginKey(login, statusKitPresenceKeyPrefix, alias)), "presence")
				want[login][alias] = portal
			}
		}
		set(bridgeID, statusKitAliasPortalKeyPrefix+"tel:+15550000000", "unscoped")

		for _, login := range logins {
			var others []networkid.UserLoginID
			for _, o := range logins {
				if o != login {
					others = append(others, o)
				}
			}
			got, err := loadLoginAliasPortals(ctx, db, bridgeID, login, others)
			if err != nil {
				t.Fatalf("loadLoginAliasPortals(%q): %v", login, err)
			}
			if !maps.Equal(got, want[login]) {
				t.Errorf("loadLoginAliasPortals(%q) = %v, want %v", login, got, want[login])
			}
		}
	})
}
