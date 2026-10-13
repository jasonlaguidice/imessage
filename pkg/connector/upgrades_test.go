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
	"database/sql"
	"slices"
	"testing"

	"go.mau.fi/util/dbutil"

	"github.com/lrhodin/imessage/pkg/connector/upgrades"
)

// expectedConnectorColumns is the full column set of every connector-owned
// table after the migrations, whichever schema the database started from.
var expectedConnectorColumns = map[string][]string{
	"cloud_sync_state": {"login_id", "zone", "continuation_token", "last_success_ts", "last_error", "updated_ts"},
	"cloud_chat": {"login_id", "cloud_chat_id", "record_name", "group_id", "portal_id", "service", "display_name",
		"group_photo_guid", "participants_json", "updated_ts", "created_ts", "deleted", "is_filtered", "fwd_backfill_done"},
	"cloud_message": {"login_id", "guid", "chat_id", "portal_id", "timestamp_ms", "sender", "is_from_me", "text",
		"subject", "service", "deleted", "tapback_type", "tapback_target_guid", "tapback_emoji", "attachments_json",
		"created_ts", "updated_ts", "date_read_ms", "record_name", "has_body"},
	"group_photo_cache":      {"login_id", "portal_id", "ts", "data"},
	"restore_override":       {"login_id", "portal_id", "updated_ts"},
	"cloud_attachment_cache": {"login_id", "record_name", "content_json", "created_ts"},
	"shared_profiles": {"login_id", "identifier", "display_name", "first_name", "last_name", "avatar", "record_key",
		"decryption_key", "has_poster", "updated_ts"},
	"pending_attachment_retry": {"login_id", "message_guid", "att_index", "att_id", "portal_id", "sender",
		"timestamp_ms", "filename", "mime_type", "uti_type", "size_bytes", "mmcs_descriptor", "created_at",
		"last_attempt_at", "attempt_count", "next_attempt_at"},
}

var expectedConnectorIndexes = []string{
	"cloud_chat_portal_idx",
	"cloud_message_portal_ts_idx",
	"cloud_message_chat_ts_idx",
	"pending_attachment_retry_due_idx",
	"cloud_chat_record_name_idx",
	"cloud_chat_group_id_idx",
}

// legacyOldestSchema is the schema of the first CloudKit backfill release
// (2a6c6fd5): cloud_chat and cloud_message without any of the columns the
// old migrateColumns later added, and the since-removed cloud_repair_task.
func legacyOldestSchema(dialect dbutil.Dialect) []string {
	repairID := "id BIGSERIAL PRIMARY KEY"
	if dialect == dbutil.SQLite {
		repairID = "id INTEGER PRIMARY KEY AUTOINCREMENT"
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS cloud_sync_state (
			login_id TEXT NOT NULL,
			zone TEXT NOT NULL,
			continuation_token TEXT,
			last_success_ts BIGINT,
			last_error TEXT,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, zone)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_chat (
			login_id TEXT NOT NULL,
			cloud_chat_id TEXT NOT NULL,
			portal_id TEXT NOT NULL,
			service TEXT,
			display_name TEXT,
			participants_json TEXT,
			updated_ts BIGINT,
			created_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, cloud_chat_id)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_message (
			login_id TEXT NOT NULL,
			guid TEXT NOT NULL,
			chat_id TEXT,
			portal_id TEXT,
			timestamp_ms BIGINT NOT NULL,
			sender TEXT,
			is_from_me BOOLEAN NOT NULL,
			text TEXT,
			service TEXT,
			deleted BOOLEAN NOT NULL DEFAULT FALSE,
			created_ts BIGINT NOT NULL,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, guid)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_repair_task (
			` + repairID + `,
			login_id TEXT NOT NULL,
			task_type TEXT NOT NULL,
			cloud_chat_id TEXT,
			portal_id TEXT,
			since_ts_ms BIGINT NOT NULL,
			not_before_ts BIGINT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_ts BIGINT NOT NULL,
			updated_ts BIGINT NOT NULL,
			done_ts BIGINT
		)`,
		`CREATE INDEX IF NOT EXISTS cloud_chat_portal_idx
			ON cloud_chat (login_id, portal_id, cloud_chat_id)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_portal_ts_idx
			ON cloud_message (login_id, portal_id, timestamp_ms, guid)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_chat_ts_idx
			ON cloud_message (login_id, chat_id, timestamp_ms, guid)`,
		`CREATE INDEX IF NOT EXISTS cloud_repair_due_idx
			ON cloud_repair_task (login_id, done_ts, not_before_ts, id)`,
	}
}

// legacyLastAdHocSchema is what the last ad-hoc schema code (the per-login
// ensureSchema functions replaced by pkg/connector/upgrades) left behind on
// a database it created: its CREATE statements, then the ALTERs its
// migrateColumns ran for the columns missing from those CREATEs, then the
// tables and indexes it created afterwards.
var legacyLastAdHocSchema = []string{
	`CREATE TABLE IF NOT EXISTS cloud_sync_state (
		login_id TEXT NOT NULL,
		zone TEXT NOT NULL,
		continuation_token TEXT,
		last_success_ts BIGINT,
		last_error TEXT,
		updated_ts BIGINT NOT NULL,
		PRIMARY KEY (login_id, zone)
	)`,
	`CREATE TABLE IF NOT EXISTS cloud_chat (
		login_id TEXT NOT NULL,
		cloud_chat_id TEXT NOT NULL,
		record_name TEXT NOT NULL DEFAULT '',
		group_id TEXT NOT NULL DEFAULT '',
		portal_id TEXT NOT NULL,
		service TEXT,
		display_name TEXT,
		group_photo_guid TEXT,
		participants_json TEXT,
		updated_ts BIGINT,
		created_ts BIGINT NOT NULL,
		PRIMARY KEY (login_id, cloud_chat_id)
	)`,
	`CREATE TABLE IF NOT EXISTS cloud_message (
		login_id TEXT NOT NULL,
		guid TEXT NOT NULL,
		chat_id TEXT,
		portal_id TEXT,
		timestamp_ms BIGINT NOT NULL,
		sender TEXT,
		is_from_me BOOLEAN NOT NULL,
		text TEXT,
		subject TEXT,
		service TEXT,
		deleted BOOLEAN NOT NULL DEFAULT FALSE,
		tapback_type INTEGER,
		tapback_target_guid TEXT,
		tapback_emoji TEXT,
		attachments_json TEXT,
		created_ts BIGINT NOT NULL,
		updated_ts BIGINT NOT NULL,
		PRIMARY KEY (login_id, guid)
	)`,
	`CREATE TABLE IF NOT EXISTS group_photo_cache (
		login_id TEXT NOT NULL,
		portal_id TEXT NOT NULL,
		ts BIGINT NOT NULL,
		data BYTEA NOT NULL,
		PRIMARY KEY (login_id, portal_id)
	)`,
	`CREATE TABLE IF NOT EXISTS restore_override (
		login_id TEXT NOT NULL,
		portal_id TEXT NOT NULL,
		updated_ts BIGINT NOT NULL,
		PRIMARY KEY (login_id, portal_id)
	)`,
	`CREATE INDEX IF NOT EXISTS cloud_chat_portal_idx
		ON cloud_chat (login_id, portal_id, cloud_chat_id)`,
	`CREATE INDEX IF NOT EXISTS cloud_message_portal_ts_idx
		ON cloud_message (login_id, portal_id, timestamp_ms, guid)`,
	`CREATE INDEX IF NOT EXISTS cloud_message_chat_ts_idx
		ON cloud_message (login_id, chat_id, timestamp_ms, guid)`,
	`ALTER TABLE cloud_chat ADD COLUMN deleted BOOLEAN NOT NULL DEFAULT FALSE`,
	`ALTER TABLE cloud_chat ADD COLUMN is_filtered INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE cloud_chat ADD COLUMN fwd_backfill_done BOOLEAN NOT NULL DEFAULT FALSE`,
	`ALTER TABLE cloud_message ADD COLUMN date_read_ms BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE cloud_message ADD COLUMN record_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE cloud_message ADD COLUMN has_body BOOLEAN NOT NULL DEFAULT TRUE`,
	`CREATE TABLE IF NOT EXISTS cloud_attachment_cache (
		login_id    TEXT    NOT NULL,
		record_name TEXT    NOT NULL,
		content_json BYTEA   NOT NULL,
		created_ts  BIGINT  NOT NULL,
		PRIMARY KEY (login_id, record_name)
	)`,
	`CREATE INDEX IF NOT EXISTS cloud_chat_record_name_idx
		ON cloud_chat (login_id, record_name) WHERE record_name <> ''`,
	`CREATE INDEX IF NOT EXISTS cloud_chat_group_id_idx
		ON cloud_chat (login_id, group_id) WHERE group_id <> ''`,
	`CREATE TABLE IF NOT EXISTS shared_profiles (
		login_id       TEXT    NOT NULL,
		identifier     TEXT    NOT NULL,
		display_name   TEXT    NOT NULL DEFAULT '',
		first_name     TEXT    NOT NULL DEFAULT '',
		last_name      TEXT    NOT NULL DEFAULT '',
		avatar         BYTEA,
		record_key     TEXT    NOT NULL,
		decryption_key BYTEA    NOT NULL,
		has_poster     BOOLEAN NOT NULL DEFAULT FALSE,
		updated_ts     BIGINT  NOT NULL,
		PRIMARY KEY (login_id, identifier)
	)`,
	`CREATE TABLE IF NOT EXISTS pending_attachment_retry (
		login_id          TEXT    NOT NULL,
		message_guid      TEXT    NOT NULL,
		att_index         INTEGER NOT NULL,
		att_id            TEXT    NOT NULL,
		portal_id         TEXT    NOT NULL,
		sender            TEXT    NOT NULL DEFAULT '',
		timestamp_ms      BIGINT  NOT NULL DEFAULT 0,
		filename          TEXT    NOT NULL,
		mime_type         TEXT    NOT NULL,
		uti_type          TEXT    NOT NULL DEFAULT '',
		size_bytes        BIGINT  NOT NULL,
		mmcs_descriptor   TEXT    NOT NULL,
		created_at        BIGINT  NOT NULL,
		last_attempt_at   BIGINT  NOT NULL,
		attempt_count     INTEGER NOT NULL DEFAULT 0,
		next_attempt_at   BIGINT  NOT NULL,
		PRIMARY KEY (login_id, message_guid, att_index)
	)`,
	`CREATE INDEX IF NOT EXISTS pending_attachment_retry_due_idx
		ON pending_attachment_retry (login_id, next_attempt_at)`,
}

func execAll(t *testing.T, db *dbutil.Database, stmts []string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := db.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// tableColumns lists table's columns in the current schema only (Postgres
// tests run in a per-test schema of a shared database).
func tableColumns(t *testing.T, db *dbutil.Database, table string) []string {
	t.Helper()
	query := `SELECT name FROM pragma_table_info($1)`
	if db.Dialect == dbutil.Postgres {
		query = `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`
	}
	return queryStrings(t, db, query, table)
}

func indexNames(t *testing.T, db *dbutil.Database) []string {
	t.Helper()
	query := `SELECT name FROM sqlite_master WHERE type = 'index'`
	if db.Dialect == dbutil.Postgres {
		query = `SELECT indexname FROM pg_indexes WHERE schemaname = current_schema()`
	}
	return queryStrings(t, db, query)
}

func queryStrings(t *testing.T, db *dbutil.Database, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", query, err)
	}
	return out
}

func connectorSchemaVersion(t *testing.T, db *dbutil.Database) int {
	t.Helper()
	var version int
	if err := db.QueryRow(context.Background(), `SELECT version FROM `+upgrades.VersionTable).Scan(&version); err != nil {
		t.Fatalf("read %s: %v", upgrades.VersionTable, err)
	}
	return version
}

// checkLatestSchema checks the version row and that every expected column
// and index exists.
func checkLatestSchema(t *testing.T, db *dbutil.Database) {
	t.Helper()
	if got, want := connectorSchemaVersion(t, db), len(upgrades.Table); got != want {
		t.Errorf("%s version = %d, want %d", upgrades.VersionTable, got, want)
	}
	for table, want := range expectedConnectorColumns {
		got := tableColumns(t, db, table)
		for _, col := range want {
			if !slices.Contains(got, col) {
				t.Errorf("%s is missing column %s (has %v)", table, col, got)
			}
		}
	}
	indexes := indexNames(t, db)
	for _, idx := range expectedConnectorIndexes {
		if !slices.Contains(indexes, idx) {
			t.Errorf("index %s missing (have %v)", idx, indexes)
		}
	}
}

// TestUpgradeFreshDB runs the migrations on an empty database, then again
// (a restart), and checks the resulting schema.
func TestUpgradeFreshDB(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		upgradeTestDB(t, db)
		checkLatestSchema(t, db)
		upgradeTestDB(t, db)
		checkLatestSchema(t, db)
	})
}

// TestUpgradeLegacyDB runs the migrations on databases whose tables were
// created by the pre-versioned connector code (no version table, so they
// start at version 0 like a fresh database) and checks that existing rows
// survive and every column the current code uses exists afterwards.
func TestUpgradeLegacyDB(t *testing.T) {
	fixtures := []struct {
		name   string
		schema func(dbutil.Dialect) []string
	}{
		{"oldest", legacyOldestSchema},
		{"last-ad-hoc", func(dbutil.Dialect) []string { return legacyLastAdHocSchema }},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
				ctx := context.Background()
				execAll(t, db, fx.schema(db.Dialect))
				// Columns every historical version of these tables has.
				execAll(t, db, []string{
					`INSERT INTO cloud_sync_state (login_id, zone, continuation_token, updated_ts)
						VALUES ('login-a', 'chatManateeZone', 'tok', 1)`,
					`INSERT INTO cloud_chat (login_id, cloud_chat_id, portal_id, service, display_name, participants_json, updated_ts, created_ts)
						VALUES ('login-a', 'chat1', 'gid:g1', 'iMessage', 'Family', '["tel:+1555"]', 2, 1)`,
					`INSERT INTO cloud_message (login_id, guid, chat_id, portal_id, timestamp_ms, sender, is_from_me, text, service, created_ts, updated_ts)
						VALUES ('login-a', 'm1', 'chat1', 'gid:g1', 1000, 'tel:+1555', FALSE, 'hello', 'iMessage', 1, 1)`,
				})

				upgradeTestDB(t, db)
				checkLatestSchema(t, db)

				var token string
				if err := db.QueryRow(ctx, `SELECT continuation_token FROM cloud_sync_state WHERE login_id='login-a' AND zone='chatManateeZone'`).Scan(&token); err != nil || token != "tok" {
					t.Errorf("cloud_sync_state row = %q, %v", token, err)
				}

				var displayName, recordName, groupID string
				var chatDeleted, fwdDone bool
				var isFiltered int
				if err := db.QueryRow(ctx, `
					SELECT display_name, record_name, group_id, deleted, is_filtered, fwd_backfill_done
					FROM cloud_chat WHERE login_id='login-a' AND cloud_chat_id='chat1'`,
				).Scan(&displayName, &recordName, &groupID, &chatDeleted, &isFiltered, &fwdDone); err != nil {
					t.Fatalf("read legacy cloud_chat row: %v", err)
				}
				if displayName != "Family" || recordName != "" || groupID != "" || chatDeleted || isFiltered != 0 || fwdDone {
					t.Errorf("cloud_chat row = %q %q %q %v %d %v", displayName, recordName, groupID, chatDeleted, isFiltered, fwdDone)
				}

				var text, msgRecordName string
				var subject sql.NullString
				var dateRead int64
				var hasBody bool
				if err := db.QueryRow(ctx, `
					SELECT text, subject, date_read_ms, record_name, has_body
					FROM cloud_message WHERE login_id='login-a' AND guid='m1'`,
				).Scan(&text, &subject, &dateRead, &msgRecordName, &hasBody); err != nil {
					t.Fatalf("read legacy cloud_message row: %v", err)
				}
				if text != "hello" || subject.Valid || dateRead != 0 || msgRecordName != "" || !hasBody {
					t.Errorf("cloud_message row = %q %v %d %q %v", text, subject, dateRead, msgRecordName, hasBody)
				}

				// The store works on the upgraded legacy tables, and the
				// startup cleanup keeps the ordinary message.
				store := newCloudBackfillStore(db, testStoreLoginA)
				if err := store.deleteLeakedSystemMessages(ctx); err != nil {
					t.Fatalf("deleteLeakedSystemMessages: %v", err)
				}
				if err := store.upsertMessageBatch(ctx, []cloudMessageRow{
					{GUID: "m2", RecordName: "r2", CloudChatID: "chat1", PortalID: "gid:g1", TimestampMS: 2000, Sender: "tel:+1555", Text: "new", Service: "iMessage", HasBody: true},
				}); err != nil {
					t.Fatalf("upsertMessageBatch: %v", err)
				}
				if n := countLoginRows(t, db, "cloud_message", testStoreLoginA); n != 2 {
					t.Errorf("cloud_message rows = %d, want 2", n)
				}

				// A restart finds the version row and runs nothing.
				upgradeTestDB(t, db)
				checkLatestSchema(t, db)
			})
		})
	}
}
