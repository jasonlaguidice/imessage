// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// trustedPeersFilePath returns the per-user keychain trust state path:
// ~/.local/share/mautrix-imessage/trustedpeers_<dsid>.plist
func trustedPeersFilePath(dsid string) (string, error) {
	dir, err := stateDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "trustedpeers_"+dsid+".plist"), nil
}

// stateDataDir returns the directory rustpush keeps its on-disk state in
// (mirrors resolve_xdg_data_dir in pkg/rustpushgo/src/lib.rs):
// $XDG_DATA_HOME/mautrix-imessage, else ~/.local/share/mautrix-imessage.
func stateDataDir() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "mautrix-imessage"), nil
}

// hasKeychainCliqueState returns true if the per-user trustedpeers.plist appears
// to contain a keychain user identity (i.e. trust circle has been joined).
func hasKeychainCliqueState(log zerolog.Logger, dsid string) bool {
	path, err := trustedPeersFilePath(dsid)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to determine trusted peers file path")
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// trustedpeers.plist is written by Rust as XML plist, where a joined clique
	// includes either userIdentity or user_identity key.
	if bytes.Contains(data, []byte("<key>userIdentity</key>")) || bytes.Contains(data, []byte("<key>user_identity</key>")) {
		return true
	}
	log.Info().Str("path", path).Msg("Trusted peers state exists but has no user identity (not in clique)")
	return false
}

// perLoginStateFileBases are rustpush's per-login state files, each named
// "<base>_<user_state_key>.plist" in stateDataDir (lib.rs: id_cache,
// facetime-state, statuskit_state_path, statuskit_channel_dates_path,
// passwords_state_path, sharedstreams-state; statuskit_cloudkit.rs:
// channel_map_path). Any new per-login file must be added here.
var perLoginStateFileBases = []string{
	"id_cache",
	"facetime-state",
	"statuskit-state",
	"statuskit-channel-dates",
	"statuskit-cloud-channel-map",
	"passwords-state",
	"sharedstreams-state",
}

// rustUserStateKey mirrors the Rust cache_key / user_state_key derivation in
// lib.rs: the login's IDS user_id (== the bridge login ID) with every
// character that is not alphanumeric (Rust char::is_alphanumeric) replaced
// by '_'.
func rustUserStateKey(loginID networkid.UserLoginID) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Other_Alphabetic, r) {
			return r
		}
		return '_'
	}, string(loginID))
}

// accountFilePaths lists the on-disk files belonging to one login: the
// per-DSID keychain trust state and, unless another login maps to the same
// rustpush state key (and so shares those files), the per-login rustpush
// state files.
func accountFilePaths(log zerolog.Logger, dsid string, loginID networkid.UserLoginID, otherLogins []networkid.UserLoginID) ([]string, error) {
	dir, err := stateDataDir()
	if err != nil {
		return nil, err
	}
	var paths []string
	if dsid != "" {
		paths = append(paths, filepath.Join(dir, "trustedpeers_"+dsid+".plist"))
	}
	if loginID == "" {
		return paths, nil
	}
	key := rustUserStateKey(loginID)
	for _, other := range otherLogins {
		if other != loginID && rustUserStateKey(other) == key {
			log.Warn().Str("state_key", key).Str("other_login_id", string(other)).
				Msg("Logout: another login shares this rustpush state key; keeping its state files")
			return paths, nil
		}
	}
	for _, base := range perLoginStateFileBases {
		paths = append(paths, filepath.Join(dir, base+"_"+key+".plist"))
	}
	return paths, nil
}

// removeAccountFiles removes the on-disk state belonging to one login on
// logout (see accountFilePaths) and returns how many files were removed.
// Missing files are not an error; other logins' files are untouched. Shared
// files (keystore.plist, the per-device anisette_<uuid> directory) are kept.
func removeAccountFiles(log zerolog.Logger, dsid string, loginID networkid.UserLoginID, otherLogins []networkid.UserLoginID) int {
	paths, err := accountFilePaths(log, dsid, loginID, otherLogins)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to determine account state file paths on logout")
		return 0
	}
	removed := 0
	for _, path := range paths {
		if err := os.Remove(path); err == nil {
			removed++
			log.Info().Str("path", path).Msg("Removed account state file on logout")
		} else if !os.IsNotExist(err) {
			log.Warn().Err(err).Str("path", path).Msg("Failed to remove account state file on logout")
		}
	}
	return removed
}
