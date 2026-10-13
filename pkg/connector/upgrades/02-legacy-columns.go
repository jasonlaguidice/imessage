// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package upgrades

import (
	"context"
	"fmt"

	"go.mau.fi/util/dbutil"
)

type legacyColumn struct {
	name, def string
}

// legacyColumns are the columns that the pre-versioned connector added to
// existing cloud_chat and cloud_message tables at runtime. Tables created by
// v1 already have them; tables created by older code may lack any of them.
var legacyColumns = []struct {
	table string
	cols  []legacyColumn
}{
	{"cloud_chat", []legacyColumn{
		{"record_name", "TEXT NOT NULL DEFAULT ''"},
		{"group_id", "TEXT NOT NULL DEFAULT ''"},
		{"group_photo_guid", "TEXT"},
		{"deleted", "BOOLEAN NOT NULL DEFAULT FALSE"},
		{"is_filtered", "INTEGER NOT NULL DEFAULT 0"},
		{"fwd_backfill_done", "BOOLEAN NOT NULL DEFAULT FALSE"},
	}},
	{"cloud_message", []legacyColumn{
		{"subject", "TEXT"},
		{"tapback_type", "INTEGER"},
		{"tapback_target_guid", "TEXT"},
		{"tapback_emoji", "TEXT"},
		{"attachments_json", "TEXT"},
		{"date_read_ms", "BIGINT NOT NULL DEFAULT 0"},
		{"record_name", "TEXT NOT NULL DEFAULT ''"},
		{"has_body", "BOOLEAN NOT NULL DEFAULT TRUE"},
	}},
}

// upgradeV2 adds any legacy column a pre-existing table is missing. Postgres
// has ADD COLUMN IF NOT EXISTS; SQLite does not, so there the column is
// looked up in pragma_table_info first. (dbutil's ColumnExists is not used
// on Postgres: its information_schema query ignores the schema, so a table
// of the same name in another schema could hide a missing column.)
var upgradeV2 = dbutil.WrapUpgrade(1, 2, 0, "Add columns missing from tables created before versioned migrations", dbutil.TxnModeOn, func(ctx context.Context, db *dbutil.Database) error {
	for _, tbl := range legacyColumns {
		for _, col := range tbl.cols {
			if err := addColumnIfMissing(ctx, db, tbl.table, col); err != nil {
				return err
			}
		}
	}
	return nil
})

func addColumnIfMissing(ctx context.Context, db *dbutil.Database, table string, col legacyColumn) error {
	switch db.Dialect {
	case dbutil.Postgres:
		if _, err := db.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`, table, col.name, col.def)); err != nil {
			return fmt.Errorf("failed to add %s column to %s: %w", col.name, table, err)
		}
	case dbutil.SQLite:
		exists, err := db.ColumnExists(ctx, table, col.name)
		if err != nil {
			return fmt.Errorf("failed to check for %s column in %s: %w", col.name, table, err)
		} else if exists {
			return nil
		}
		if _, err = db.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, col.name, col.def)); err != nil {
			return fmt.Errorf("failed to add %s column to %s: %w", col.name, table, err)
		}
	default:
		return fmt.Errorf("%w %s", dbutil.ErrUnsupportedDialect, db.Dialect)
	}
	return nil
}
