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
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// createTestKVStore creates bridgev2's kv_store table (as in its
// upgrades/00-latest.sql) in a store test DB.
func createTestKVStore(t *testing.T, db *dbutil.Database) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `CREATE TABLE kv_store (
		bridge_id TEXT NOT NULL,
		key       TEXT NOT NULL,
		value     TEXT NOT NULL,
		PRIMARY KEY (bridge_id, key)
	)`); err != nil {
		t.Fatalf("create kv_store: %v", err)
	}
}

func testLoginKVKeys(loginID networkid.UserLoginID) []string {
	var keys []string
	for _, k := range loginExactKVKeys(loginID) {
		keys = append(keys, string(k))
	}
	for _, p := range loginScopedKVPrefixes {
		keys = append(keys,
			string(statusKitLoginKey(loginID, p, "tel:+15551234567")),
			string(statusKitLoginKey(loginID, p, "mailto:x_y%z@example.com")),
		)
	}
	return keys
}

// TestDeleteLoginKVRows checks that logout removes every KV key of one login
// and nothing else: not another login whose ID only differs by LIKE
// metacharacters or case, not a login whose ID extends this one with a dot,
// not unscoped keys, and not the same keys under another bridge ID.
func TestDeleteLoginKVRows(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		createTestKVStore(t, db)
		const bridgeID = networkid.BridgeID("imessage")
		const otherBridgeID = networkid.BridgeID("other")

		target := networkid.UserLoginID("a_b%c@x.com")
		others := []networkid.UserLoginID{
			"aXbYYc@x.com",   // matches target's LIKE pattern if '_'/'%' were unescaped
			"A_B%C@X.COM",    // differs only by case (SQLite LIKE ignores ASCII case)
			"a_b%c@x.com.au", // extends target's ID with a '.'
		}

		set := func(bridge networkid.BridgeID, key string) {
			t.Helper()
			if _, err := db.Exec(ctx, `INSERT INTO kv_store (bridge_id, key, value) VALUES ($1, $2, 'v')`, bridge, key); err != nil {
				t.Fatalf("insert %q: %v", key, err)
			}
		}
		targetKeys := testLoginKVKeys(target)
		for _, k := range targetKeys {
			set(bridgeID, k)
			set(otherBridgeID, k)
		}
		var keep []string
		for _, o := range others {
			keep = append(keep, testLoginKVKeys(o)...)
		}
		keep = append(keep, "split_portals_enabled", "statuskit.presence.tel:+15551234567", "im.last_full_connect")
		for _, k := range keep {
			set(bridgeID, k)
		}

		n, err := deleteLoginKVRows(ctx, db, bridgeID, target, others)
		if err != nil {
			t.Fatalf("deleteLoginKVRows: %v", err)
		}
		if n != int64(len(targetKeys)) {
			t.Errorf("deleted %d keys, want %d", n, len(targetKeys))
		}

		exists := func(bridge networkid.BridgeID, key string) bool {
			var c int
			if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM kv_store WHERE bridge_id=$1 AND key=$2`, bridge, key).Scan(&c); err != nil {
				t.Fatalf("count %q: %v", key, err)
			}
			return c > 0
		}
		for _, k := range targetKeys {
			if exists(bridgeID, k) {
				t.Errorf("target key %q still present", k)
			}
			if !exists(otherBridgeID, k) {
				t.Errorf("other bridge's key %q was deleted", k)
			}
		}
		for _, k := range keep {
			if !exists(bridgeID, k) {
				t.Errorf("unrelated key %q was deleted", k)
			}
		}

		// Idempotent: a second run finds nothing.
		if n, err := deleteLoginKVRows(ctx, db, bridgeID, target, others); err != nil || n != 0 {
			t.Errorf("second deleteLoginKVRows = %d, %v; want 0, nil", n, err)
		}
	})
}

func TestRustUserStateKey(t *testing.T) {
	for in, want := range map[networkid.UserLoginID]string{
		"user@icloud.com": "user_icloud_com",
		"+15551234567":    "_15551234567",
		"émile@x.fr":      "émile_x_fr",
	} {
		if got := rustUserStateKey(in); got != want {
			t.Errorf("rustUserStateKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoveAccountFiles(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dir := filepath.Join(dataHome, "mautrix-imessage")
	if err := os.MkdirAll(filepath.Join(dir, "anisette_DEVICE"), 0o700); err != nil {
		t.Fatal(err)
	}
	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loginA := networkid.UserLoginID("a@icloud.com")
	loginB := networkid.UserLoginID("b@icloud.com")
	var kept []string
	touch("trustedpeers_111.plist")
	for _, base := range perLoginStateFileBases {
		touch(base + "_a_icloud_com.plist")
		touch(base + "_b_icloud_com.plist")
		kept = append(kept, base+"_b_icloud_com.plist")
	}
	for _, name := range []string{"trustedpeers_222.plist", "keystore.plist", "anisette_DEVICE/state.plist"} {
		touch(name)
		kept = append(kept, name)
	}

	if n := removeAccountFiles(zerolog.Nop(), "111", loginA, []networkid.UserLoginID{loginB}); n != len(perLoginStateFileBases)+1 {
		t.Errorf("removed %d files, want %d", n, len(perLoginStateFileBases)+1)
	}
	if _, err := os.Stat(filepath.Join(dir, "trustedpeers_111.plist")); !os.IsNotExist(err) {
		t.Errorf("trustedpeers_111.plist still present (err=%v)", err)
	}
	for _, base := range perLoginStateFileBases {
		if _, err := os.Stat(filepath.Join(dir, base+"_a_icloud_com.plist")); !os.IsNotExist(err) {
			t.Errorf("%s for login A still present (err=%v)", base, err)
		}
	}
	for _, name := range kept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}

	// A login whose ID maps to the same rustpush state key shares those
	// files, so they are kept; the DSID trust file is still removed.
	touch("trustedpeers_333.plist")
	shared := networkid.UserLoginID("b.icloud.com")
	if n := removeAccountFiles(zerolog.Nop(), "333", loginB, []networkid.UserLoginID{shared}); n != 1 {
		t.Errorf("removed %d files with a shared state key, want 1", n)
	}
	for _, base := range perLoginStateFileBases {
		if _, err := os.Stat(filepath.Join(dir, base+"_b_icloud_com.plist")); err != nil {
			t.Errorf("shared %s was removed: %v", base, err)
		}
	}
}
