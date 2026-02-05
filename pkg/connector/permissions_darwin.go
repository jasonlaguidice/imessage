// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

//go:build darwin && !ios

package connector

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	_ "github.com/mattn/go-sqlite3"
)

func canReadChatDB() bool {
	dbPath := filepath.Join(os.Getenv("HOME"), "Library", "Messages", "chat.db")
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	_, err = db.Query("SELECT 1 FROM message LIMIT 1")
	return err == nil
}

func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "operation not permitted") ||
		strings.Contains(msg, "permission denied")
}

func showDialogAndOpenFDA(log zerolog.Logger) {
	log.Warn().Msg("Full Disk Access lost — prompting user to re-grant")
	script := `display dialog "mautrix-imessage lost Full Disk Access (likely after a rebuild).\n\nClick OK to open System Settings. Please toggle the switch for mautrix-imessage off and back on." with title "iMessage Bridge" buttons {"OK"} default button "OK"`
	exec.Command("osascript", "-e", script).Run()
	exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run()
}

func waitForFDA(log zerolog.Logger) {
	for !canReadChatDB() {
		time.Sleep(2 * time.Second)
	}
	log.Info().Msg("Full Disk Access restored")
}
