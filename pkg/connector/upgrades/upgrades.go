// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

// Package upgrades holds the schema of the tables the iMessage connector owns
// in the bridge database (CloudKit backfill cache, shared profiles, pending
// attachment retries). They are versioned in their own version table,
// separate from bridgev2's, and upgraded once at connector startup.
//
// There is deliberately no 00-latest.sql fast path: version 0 covers both a
// fresh database and one whose tables were created ad hoc by the connector
// before this package existed, so every upgrade must be safe on both.
package upgrades

import (
	"embed"

	"go.mau.fi/util/dbutil"
)

// VersionTable is the connector's own version table name.
const VersionTable = "imessage_version"

//go:embed *.sql
var rawUpgrades embed.FS

// Table is the connector's upgrade table.
var Table = dbutil.BuildUpgradeTable().
	WithFS(rawUpgrades).
	With(upgradeV2).
	Finish()
