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
	"fmt"
	"strings"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// Login-scoped KV prefix scans.
//
// Per-login KV keys have the form "<prefix><loginID>.<suffix>" (built by
// statusKitLoginKey). Reading or deleting "all of one login's keys under a
// prefix" with a plain LIKE is wrong in three ways:
//   - '_' and '%' in a login ID are LIKE wildcards;
//   - SQLite's LIKE ignores ASCII case;
//   - login IDs are Apple IDs and may contain '.', so "a@b.com." is also a
//     prefix of login "a@b.com.au"'s keys.
// scanLoginKVRows handles all three; every login-scoped prefix scan of
// kv_store must go through it.

// likeEscaper escapes LIKE metacharacters with '!', which needs no special
// handling in string literals on either SQLite or PostgreSQL (unlike '\',
// whose meaning depends on PostgreSQL's standard_conforming_strings).
var likeEscaper = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")

// likePrefixPattern returns a LIKE pattern, for use with ESCAPE '!', that
// matches every string starting with prefix (case-insensitively on SQLite,
// so callers must still re-check the prefix exactly).
func likePrefixPattern(prefix string) string {
	return likeEscaper.Replace(prefix) + "%"
}

// loginKVRow is one kv_store row belonging to a login. Suffix is Key with
// the login's scan prefix ("<prefix><loginID>.") removed.
type loginKVRow struct {
	Key    string
	Suffix string
	Value  string
}

// loginKVShadowPrefixes returns the scan prefixes of other logins that fall
// under scan, i.e. those of a login whose ID extends loginID (such as
// "a@b.com.au" for "a@b.com"). Keys under them belong to that other login.
func loginKVShadowPrefixes(scan string, loginID networkid.UserLoginID, otherLoginIDs []networkid.UserLoginID) []string {
	var shadows []string
	for _, other := range otherLoginIDs {
		if other == loginID {
			continue
		}
		for _, p := range loginScopedKVPrefixes {
			if s := string(statusKitLoginKey(other, p, "")); strings.HasPrefix(s, scan) {
				shadows = append(shadows, s)
			}
		}
	}
	return shadows
}

// scanLoginKVRows returns loginID's kv_store rows for bridgeID under one
// statusKitLoginKey prefix. LIKE (with escaped metacharacters) only narrows
// the scan; the exact, case-sensitive prefix test in Go and the exclusion of
// keys under another login's scan prefix decide. otherLoginIDs must list
// every other login on this bridge.
func scanLoginKVRows(ctx context.Context, db *dbutil.Database, bridgeID networkid.BridgeID, loginID networkid.UserLoginID, prefix string, otherLoginIDs []networkid.UserLoginID) ([]loginKVRow, error) {
	scan := string(statusKitLoginKey(loginID, prefix, ""))
	shadows := loginKVShadowPrefixes(scan, loginID, otherLoginIDs)
	rows, err := db.Query(ctx,
		`SELECT key, value FROM kv_store WHERE bridge_id=$1 AND key LIKE $2 ESCAPE '!'`,
		bridgeID, likePrefixPattern(scan))
	if err != nil {
		return nil, fmt.Errorf("failed to scan kv keys with prefix %q: %w", scan, err)
	}
	defer rows.Close()
	var out []loginKVRow
	for rows.Next() {
		var row loginKVRow
		if err := rows.Scan(&row.Key, &row.Value); err != nil {
			return nil, fmt.Errorf("failed to scan kv row: %w", err)
		}
		if !strings.HasPrefix(row.Key, scan) || hasAnyPrefix(row.Key, shadows) {
			continue
		}
		row.Suffix = row.Key[len(scan):]
		out = append(out, row)
	}
	return out, rows.Err()
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
