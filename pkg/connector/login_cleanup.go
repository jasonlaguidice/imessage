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

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// Logout cleanup: removal of the data the connector stores itself for one
// login. bridgev2 removes its own rows (user_login, portals per its cleanup
// settings) after LogoutRemote returns; everything here would otherwise be
// left behind forever.

// deleteLoginRows deletes every row of each table whose login_id is loginID,
// in a single transaction. A table that does not exist yet (its store's
// schema was never ensured on this bridge, e.g. CloudKit backfill never ran)
// is skipped. Per-table counts are logged at info level via the context
// logger.
func deleteLoginRows(ctx context.Context, db *dbutil.Database, loginID networkid.UserLoginID, tables []string) error {
	return db.DoTxn(ctx, nil, func(ctx context.Context) error {
		log := zerolog.Ctx(ctx)
		for _, table := range tables {
			exists, err := db.TableExists(ctx, table)
			if err != nil {
				return fmt.Errorf("failed to check whether %s exists: %w", table, err)
			}
			if !exists {
				log.Info().Str("table", table).Msg("Logout: table does not exist, nothing to delete")
				continue
			}
			// table comes from a fixed in-package list, never from input.
			res, err := db.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE login_id=$1`, table), loginID)
			if err != nil {
				return fmt.Errorf("failed to delete %s rows: %w", table, err)
			}
			n, _ := res.RowsAffected()
			log.Info().Str("table", table).Int64("rows", n).Msg("Logout: deleted login rows")
		}
		return nil
	})
}

// loginScopedKVPrefixes are the KV prefixes whose keys have the form
// "<prefix><loginID>.<suffix>" (built by statusKitLoginKey). Any new
// per-login prefix must be added here so logout removes it.
var loginScopedKVPrefixes = []string{
	statusKitPresenceKeyPrefix,
	statusKitPresencePortalKeyPrefix,
	statusKitAliasPortalKeyPrefix,
	statusKitChannelClusterKeyPrefix,
	statusKitAliasChannelsKeyPrefix,
	statusKitIDSAttemptKeyPrefix,
	statusKitLastInviteKeyPrefix,
	statusKitInvitedOkKeyPrefix,
	statusKitReshareSeenKeyPrefix,
	statusKitNoKeysKeyPrefix,
}

// loginExactKVKeys returns the single-row KV keys that belong to loginID.
func loginExactKVKeys(loginID networkid.UserLoginID) []database.Key {
	return []database.Key{
		lastFullConnectKVKeyFor(loginID),
		petRefreshKVKeyFor(loginID),
		statusKitLastPostInviteShareKVKeyFor(loginID),
	}
}

// deleteLoginKVRows deletes loginID's rows from bridgev2's kv_store for
// bridgeID, in one transaction, and returns how many rows were removed.
//
// Login IDs are Apple IDs and may contain '.', so a scan prefix like
// "statuskit.no_keys.a@b.com." also matches "statuskit.no_keys.a@b.com.au.x",
// which belongs to login "a@b.com.au". otherLoginIDs (every other login on
// this bridge) is used to skip any key that falls under another login's own
// scan prefix, so only this login's keys are deleted.
func deleteLoginKVRows(ctx context.Context, db *dbutil.Database, bridgeID networkid.BridgeID, loginID networkid.UserLoginID, otherLoginIDs []networkid.UserLoginID) (int64, error) {
	var total int64
	err := db.DoTxn(ctx, nil, func(ctx context.Context) error {
		total = 0
		del := func(key string) error {
			res, err := db.Exec(ctx, `DELETE FROM kv_store WHERE bridge_id=$1 AND key=$2`, bridgeID, key)
			if err != nil {
				return fmt.Errorf("failed to delete kv key %q: %w", key, err)
			}
			n, _ := res.RowsAffected()
			total += n
			return nil
		}
		for _, key := range loginExactKVKeys(loginID) {
			if err := del(string(key)); err != nil {
				return err
			}
		}
		for _, prefix := range loginScopedKVPrefixes {
			rows, err := scanLoginKVRows(ctx, db, bridgeID, loginID, prefix, otherLoginIDs)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if err := del(row.Key); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return total, err
}

// otherLoginIDs returns the IDs of every other login on this bridge.
func (c *IMClient) otherLoginIDs(ctx context.Context) ([]networkid.UserLoginID, error) {
	rows, err := c.Main.Bridge.DB.Query(ctx,
		`SELECT id FROM user_login WHERE bridge_id=$1 AND id<>$2`,
		c.Main.Bridge.ID, c.UserLogin.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list other logins: %w", err)
	}
	defer rows.Close()
	var ids []networkid.UserLoginID
	for rows.Next() {
		var id networkid.UserLoginID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan login id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// deleteLoginKVKeys deletes this login's kv_store rows (see
// deleteLoginKVRows) and returns how many were removed.
func (c *IMClient) deleteLoginKVKeys(ctx context.Context, otherLogins []networkid.UserLoginID) (int64, error) {
	return deleteLoginKVRows(ctx, c.Main.Bridge.DB.Database, c.Main.Bridge.ID, c.UserLogin.ID, otherLogins)
}

// loginDataStore is a connector store holding rows for one login.
type loginDataStore interface {
	deleteAllForLogin(ctx context.Context) error
}

// deleteLoginStoreRows deletes this login's rows from every connector store.
// A failing store is logged and does not stop the others.
func (c *IMClient) deleteLoginStoreRows(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	type namedStore struct {
		name  string
		store loginDataStore
	}
	// Nil checks before boxing: a nil *T in an interface is non-nil.
	var stores []namedStore
	if c.cloudStore != nil {
		stores = append(stores, namedStore{"cloud backfill", c.cloudStore})
	}
	if c.pendingAttachments != nil {
		stores = append(stores, namedStore{"pending attachment retry", c.pendingAttachments})
	}
	if c.sharedProfileStore != nil {
		stores = append(stores, namedStore{"shared profile", c.sharedProfileStore})
	}
	for _, s := range stores {
		log.Info().Str("store", s.name).Msg("Logout: deleting store rows")
		if err := s.store.deleteAllForLogin(ctx); err != nil {
			log.Err(err).Str("store", s.name).Msg("Logout: failed to delete store rows")
		}
	}
}
