// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

//go:build darwin && !ios

package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/lrhodin/imessage/imessage/mac"
)

const launchAgentLabel = "com.lrhodin.mautrix-imessage"

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// dialog shows a macOS dialog and returns true if the user clicked the
// default button. The optional second button is always "Quit".
func dialog(title, msg string) bool {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {"Quit","OK"} default button "OK"`,
		msg, title,
	)
	err := exec.Command("osascript", "-e", script).Run()
	return err == nil // user clicked OK
}

func dialogInfo(title, msg string) {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {"OK"} default button "OK"`,
		msg, title,
	)
	exec.Command("osascript", "-e", script).Run()
}

func canReadChatDB() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	dbPath := filepath.Join(home, "Library", "Messages", "chat.db")
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	_, err = db.Query("SELECT 1 FROM message LIMIT 1")
	return err == nil
}

func requestContacts() (bool, error) {
	cs := mac.NewContactStore()
	err := cs.RequestContactAccess()
	if err != nil {
		return false, err
	}
	return cs.HasContactAccess, nil
}

func installLaunchAgent(binaryPath, configPath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>-c</string>
        <string>%s</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>`,
		launchAgentLabel,
		binaryPath,
		configPath,
		filepath.Dir(configPath),
		filepath.Join(filepath.Dir(configPath), "bridge.stdout.log"),
		filepath.Join(filepath.Dir(configPath), "bridge.stderr.log"),
	)
	return os.WriteFile(launchAgentPath(), []byte(plist), 0644)
}

func runSetup(configPath string) {
	title := "iMessage Bridge Setup"

	// Resolve paths
	configPath, _ = filepath.Abs(configPath)
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)

	// ── Step 1: Full Disk Access ─────────────────────────────────
	if !canReadChatDB() {
		if !dialog(title, "Full Disk Access is required to read iMessages.\n\nClick OK, then add this app in the window that opens.") {
			os.Exit(0)
		}
		exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles").Run()
		for !canReadChatDB() {
			time.Sleep(2 * time.Second)
		}
		dialogInfo(title, "✓ Full Disk Access granted.")
	}

	// ── Step 2: Contacts ─────────────────────────────────────────
	granted, err := requestContacts()
	if err != nil {
		dialog(title, fmt.Sprintf("Contacts access error: %v\n\nPlease grant access in System Settings → Privacy & Security → Contacts.", err))
	} else if !granted {
		dialog(title, "Contacts access was denied.\n\nPlease enable it in System Settings → Privacy & Security → Contacts, then restart.")
	}

	// ── Step 3: Install LaunchAgent ──────────────────────────────
	// Unload any existing agent
	exec.Command("launchctl", "unload", launchAgentPath()).Run()

	if err := installLaunchAgent(exe, configPath); err != nil {
		dialog(title, fmt.Sprintf("Failed to install LaunchAgent: %v", err))
		os.Exit(1)
	}

	// ── Step 4: Start ────────────────────────────────────────────
	if out, err := exec.Command("launchctl", "load", launchAgentPath()).CombinedOutput(); err != nil {
		dialog(title, fmt.Sprintf("Failed to start LaunchAgent: %v\n%s", err, string(out)))
		os.Exit(1)
	}

	status := "✓ Full Disk Access granted\n"
	if granted {
		status += "✓ Contacts access granted\n"
	} else {
		status += "⚠ Contacts access not granted (names won't resolve)\n"
	}
	status += "✓ LaunchAgent installed — bridge will start at login\n"
	status += "\nThe bridge is running."

	dialogInfo(title, status)
}

func isSetupMode() bool {
	for _, arg := range os.Args[1:] {
		if strings.TrimLeft(arg, "-") == "setup" {
			return true
		}
	}
	return false
}


