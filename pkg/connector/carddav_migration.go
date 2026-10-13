// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
// Copyright (C) 2026 Jason LaGuidice
//
// One-time migration of legacy encrypted CardDAV passwords.
//
// Older versions stored the per-user CardDAV password as AES-256-GCM
// ciphertext (UserLoginMetadata.CardDAVPasswordEncrypted), encrypted with a
// single bridge-wide key file ($XDG_DATA_HOME/mautrix-imessage/carddav.key).
// The password now lives in plain UserLoginMetadata.CardDAVPassword, like the
// Apple account tokens. This file only exists to move old values over and
// delete the key file once nothing needs it. Nothing here encrypts.

package connector

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
)

const cardDAVKeyFileName = "carddav.key"

// legacyCardDAVMigrationLock serializes migration and key-file cleanup, since
// LoadUserLogin can run for several logins at once (e.g. NewLogin while
// another login is being recreated).
var legacyCardDAVMigrationLock sync.Mutex

// cardDAVKeyDir returns the session data directory where the legacy key is stored.
func cardDAVKeyDir() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "mautrix-imessage")
}

// cardDAVKeyPath returns the full path to the legacy CardDAV key file.
func cardDAVKeyPath() string {
	return filepath.Join(cardDAVKeyDir(), cardDAVKeyFileName)
}

// loadCardDAVKey reads the legacy AES-256 key from the session data directory.
func loadCardDAVKey() ([]byte, error) {
	key, err := os.ReadFile(cardDAVKeyPath())
	if err != nil {
		return nil, fmt.Errorf("failed to read CardDAV key file (%s): %w", cardDAVKeyPath(), err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("CardDAV key file has wrong size: %d (expected 32)", len(key))
	}
	return key, nil
}

// decryptLegacyCardDAVPassword decrypts a base64-encoded AES-256-GCM
// ciphertext (nonce prepended) written by older versions, using carddav.key.
func decryptLegacyCardDAVPassword(encrypted string) (string, error) {
	key, err := loadCardDAVKey()
	if err != nil {
		return "", err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(plaintext), nil
}

// needsLegacyCardDAVMigration reports whether meta still holds a legacy
// encrypted password that has not been moved to CardDAVPassword.
func needsLegacyCardDAVMigration(meta *UserLoginMetadata) bool {
	return meta != nil && meta.CardDAVPasswordEncrypted != "" && meta.CardDAVPassword == ""
}

// migrateLegacyCardDAVPassword moves a legacy encrypted password into
// CardDAVPassword and clears CardDAVPasswordEncrypted. It returns whether meta
// changed (and so must be saved). If decryption fails, the legacy field is
// still cleared, meta is reported as changed, and the decryption error is
// returned so the caller can log it; the user then has to run set-carddav
// again. It only reads the local key file: no network work.
func migrateLegacyCardDAVPassword(meta *UserLoginMetadata) (changed bool, err error) {
	if meta == nil || meta.CardDAVPasswordEncrypted == "" {
		return false, nil
	}
	if meta.CardDAVPassword == "" {
		meta.CardDAVPassword, err = decryptLegacyCardDAVPassword(meta.CardDAVPasswordEncrypted)
	}
	meta.CardDAVPasswordEncrypted = ""
	return true, err
}

// removeLegacyCardDAVKeyIfUnused deletes carddav.key once anyLegacyStored
// reports that no stored login still holds an encrypted password. It does
// nothing when the key file is already gone, and keeps the file if the check
// fails.
func removeLegacyCardDAVKeyIfUnused(ctx context.Context, anyLegacyStored func(context.Context) (bool, error), log zerolog.Logger) {
	keyPath := cardDAVKeyPath()
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		log.Warn().Err(err).Str("path", keyPath).Msg("Failed to stat legacy CardDAV key file")
		return
	}
	remaining, err := anyLegacyStored(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to check for legacy encrypted CardDAV passwords; keeping legacy key file")
		return
	} else if remaining {
		return
	}
	if err := os.Remove(keyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn().Err(err).Str("path", keyPath).Msg("Failed to delete legacy CardDAV key file")
		return
	}
	log.Info().Str("path", keyPath).Msg("Deleted legacy CardDAV key file: no stored login uses it any more")
}

// legacyCardDAVPasswordCountQuery counts user_login rows (of any bridge
// sharing this DB, to stay conservative) whose metadata JSON still has the
// legacy carddav_password_encrypted key. The field is omitempty, so the key is
// only present when set. CAST(... AS TEXT) works on SQLite (metadata is
// text) and Postgres (metadata is jsonb, whose text form quotes keys the same
// way). A false positive only keeps the key file, which is harmless.
const legacyCardDAVPasswordCountQuery = `
	SELECT COUNT(*) FROM user_login
	WHERE CAST(metadata AS TEXT) LIKE '%"carddav_password_encrypted"%'
`

// countLegacyCardDAVPasswords returns how many stored user logins still hold
// a legacy encrypted CardDAV password.
func countLegacyCardDAVPasswords(ctx context.Context, db *dbutil.Database) (int, error) {
	var count int
	if err := db.QueryRow(ctx, legacyCardDAVPasswordCountQuery).Scan(&count); err != nil {
		return 0, fmt.Errorf("failed to count legacy encrypted CardDAV passwords: %w", err)
	}
	return count, nil
}

// anyLegacyCardDAVPasswordStored reports whether any stored user login still
// has carddav_password_encrypted set. It reads the database directly, not the
// bridgev2 login cache, so it sees logins that are not loaded yet and does not
// take the cache lock (LoadUserLogin runs while bridgev2 holds it).
func (c *IMConnector) anyLegacyCardDAVPasswordStored(ctx context.Context) (bool, error) {
	count, err := countLegacyCardDAVPasswords(ctx, c.Bridge.DB.Database)
	return count > 0, err
}

// removeLegacyCardDAVKeyIfUnused deletes carddav.key if no user_login row in
// the bridge DB still needs it.
func (c *IMConnector) removeLegacyCardDAVKeyIfUnused(ctx context.Context, log zerolog.Logger) {
	legacyCardDAVMigrationLock.Lock()
	defer legacyCardDAVMigrationLock.Unlock()
	removeLegacyCardDAVKeyIfUnused(ctx, c.anyLegacyCardDAVPasswordStored, log)
}

// migrateLegacyCardDAVLogin runs the one-time password migration for one
// login during LoadUserLogin: decrypt, store plaintext, clear the legacy
// field, save, then delete the key file if this was the last login using it.
// Failures are logged only; they never block loading the login.
func (c *IMConnector) migrateLegacyCardDAVLogin(ctx context.Context, login *bridgev2.UserLogin, meta *UserLoginMetadata, log zerolog.Logger) {
	if !needsLegacyCardDAVMigration(meta) {
		return
	}
	legacyCardDAVMigrationLock.Lock()
	defer legacyCardDAVMigrationLock.Unlock()

	_, decryptErr := migrateLegacyCardDAVPassword(meta)
	if decryptErr != nil {
		log.Warn().Err(decryptErr).
			Str("user_login_id", string(login.ID)).
			Msg("Could not decrypt legacy CardDAV password; cleared it. External CardDAV contacts are disabled for this login until the user runs set-carddav again")
	}
	if err := login.Save(ctx); err != nil {
		// The plaintext is in memory, so this run still works; the DB row keeps
		// the ciphertext, so the key file stays and the next start retries.
		log.Warn().Err(err).Str("user_login_id", string(login.ID)).Msg("Failed to save migrated CardDAV password")
		return
	}
	if decryptErr == nil {
		log.Info().Str("user_login_id", string(login.ID)).Msg("Migrated legacy encrypted CardDAV password to plain login metadata")
	}
	removeLegacyCardDAVKeyIfUnused(ctx, c.anyLegacyCardDAVPasswordStored, log)
}
