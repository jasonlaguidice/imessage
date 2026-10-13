package connector

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
)

// legacyEncryptCardDAVPasswordForTest is a test-only copy of the removed
// EncryptCardDAVPassword (plus generateCardDAVKey): it creates
// $XDG_DATA_HOME/mautrix-imessage/carddav.key if missing and returns base64
// AES-256-GCM ciphertext with the nonce prepended, exactly as older versions
// stored in carddav_password_encrypted.
func legacyEncryptCardDAVPasswordForTest(t *testing.T, password string) string {
	t.Helper()
	key, err := loadCardDAVKey()
	if err != nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatalf("generate key: %v", err)
		}
		writeTestCardDAVKey(t, key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(password), nil))
}

func writeTestCardDAVKey(t *testing.T, key []byte) {
	t.Helper()
	if err := os.MkdirAll(cardDAVKeyDir(), 0700); err != nil {
		t.Fatalf("create key dir: %v", err)
	}
	if err := os.WriteFile(cardDAVKeyPath(), key, 0600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
}

func TestDecryptLegacyCardDAVPassword_RoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, password := range []string{"my-secret-app-password", "this-is-a-very-long-password-with-special-chars-!@#$%^&*()"} {
		encrypted := legacyEncryptCardDAVPasswordForTest(t, password)
		decrypted, err := decryptLegacyCardDAVPassword(encrypted)
		if err != nil {
			t.Fatalf("decryptLegacyCardDAVPassword error: %v", err)
		}
		if decrypted != password {
			t.Errorf("decrypted = %q, want %q", decrypted, password)
		}
	}
}

func TestDecryptLegacyCardDAVPassword_WrongKey(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	encrypted := legacyEncryptCardDAVPasswordForTest(t, "my-password")
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(i)
	}
	writeTestCardDAVKey(t, newKey)
	if _, err := decryptLegacyCardDAVPassword(encrypted); err == nil {
		t.Error("decryptLegacyCardDAVPassword should fail with wrong key")
	}
}

func TestDecryptLegacyCardDAVPassword_BadInput(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	writeTestCardDAVKey(t, make([]byte, 32))
	if _, err := decryptLegacyCardDAVPassword("not-valid-base64!!!"); err == nil {
		t.Error("should fail with invalid base64")
	}
	if _, err := decryptLegacyCardDAVPassword("AA=="); err == nil {
		t.Error("should fail with too-short ciphertext")
	}
}

func TestLoadCardDAVKey_WrongSize(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	writeTestCardDAVKey(t, []byte("too-short"))
	if _, err := loadCardDAVKey(); err == nil {
		t.Error("loadCardDAVKey should fail with wrong-size key file")
	}
}

func TestLoadCardDAVKey_Missing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := loadCardDAVKey(); err == nil {
		t.Error("loadCardDAVKey should fail when key file is missing")
	}
}

func TestCardDAVKeyDir_XDGSet(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/test-xdg")
	if got, want := cardDAVKeyDir(), "/tmp/test-xdg/mautrix-imessage"; got != want {
		t.Errorf("cardDAVKeyDir() = %q, want %q", got, want)
	}
}

func TestCardDAVKeyDir_XDGUnset(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	home, _ := os.UserHomeDir()
	if got, want := cardDAVKeyDir(), filepath.Join(home, ".local", "share", "mautrix-imessage"); got != want {
		t.Errorf("cardDAVKeyDir() = %q, want %q", got, want)
	}
}

func TestMigrateLegacyCardDAVPassword(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	encrypted := legacyEncryptCardDAVPasswordForTest(t, "app-password")

	// Start from stored JSON, as LoadUserLogin would see it.
	stored := `{"carddav_email":"a@b.com","carddav_url":"https://dav.example.com/","carddav_password_encrypted":"` + encrypted + `"}`
	var meta UserLoginMetadata
	if err := json.Unmarshal([]byte(stored), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if !needsLegacyCardDAVMigration(&meta) {
		t.Fatal("needsLegacyCardDAVMigration should be true for legacy metadata")
	}

	changed, err := migrateLegacyCardDAVPassword(&meta)
	if err != nil {
		t.Fatalf("migrateLegacyCardDAVPassword error: %v", err)
	}
	if !changed {
		t.Error("changed should be true")
	}
	if meta.CardDAVPassword != "app-password" {
		t.Errorf("CardDAVPassword = %q, want %q", meta.CardDAVPassword, "app-password")
	}
	if meta.CardDAVPasswordEncrypted != "" {
		t.Errorf("CardDAVPasswordEncrypted = %q, want empty", meta.CardDAVPasswordEncrypted)
	}
	if needsLegacyCardDAVMigration(&meta) {
		t.Error("needsLegacyCardDAVMigration should be false after migration")
	}

	out, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if strings.Contains(string(out), "carddav_password_encrypted") {
		t.Errorf("saved metadata still has the legacy key: %s", out)
	}
	if !strings.Contains(string(out), `"carddav_password":"app-password"`) {
		t.Errorf("saved metadata lacks carddav_password: %s", out)
	}
}

func TestMigrateLegacyCardDAVPassword_DecryptFailureClears(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	encrypted := legacyEncryptCardDAVPasswordForTest(t, "app-password")
	if err := os.Remove(cardDAVKeyPath()); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	meta := &UserLoginMetadata{CardDAVEmail: "a@b.com", CardDAVPasswordEncrypted: encrypted}
	changed, err := migrateLegacyCardDAVPassword(meta)
	if err == nil {
		t.Error("expected a decryption error with the key file missing")
	}
	if !changed {
		t.Error("changed should be true so the cleared field is saved")
	}
	if meta.CardDAVPasswordEncrypted != "" || meta.CardDAVPassword != "" {
		t.Errorf("got encrypted=%q password=%q, want both empty", meta.CardDAVPasswordEncrypted, meta.CardDAVPassword)
	}
}

func TestMigrateLegacyCardDAVPassword_NoOpAndAlreadyPlain(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	meta := &UserLoginMetadata{CardDAVEmail: "a@b.com", CardDAVPassword: "pw"}
	if changed, err := migrateLegacyCardDAVPassword(meta); changed || err != nil {
		t.Errorf("plain-only metadata: changed=%v err=%v, want false/nil", changed, err)
	}
	if changed, err := migrateLegacyCardDAVPassword(nil); changed || err != nil {
		t.Errorf("nil metadata: changed=%v err=%v, want false/nil", changed, err)
	}

	// Both set (should not happen, but): keep the plain one, drop the legacy
	// field without needing the key.
	meta = &UserLoginMetadata{CardDAVPassword: "pw", CardDAVPasswordEncrypted: "stale"}
	if needsLegacyCardDAVMigration(meta) {
		t.Error("needsLegacyCardDAVMigration should be false when the plain password is set")
	}
	changed, err := migrateLegacyCardDAVPassword(meta)
	if !changed || err != nil {
		t.Errorf("changed=%v err=%v, want true/nil", changed, err)
	}
	if meta.CardDAVPassword != "pw" || meta.CardDAVPasswordEncrypted != "" {
		t.Errorf("got password=%q encrypted=%q", meta.CardDAVPassword, meta.CardDAVPasswordEncrypted)
	}
}

func TestRemoveLegacyCardDAVKeyIfUnused(t *testing.T) {
	log := zerolog.Nop()
	ctx := context.Background()
	fixed := func(remaining bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) { return remaining, err }
	}
	keyExists := func() bool {
		_, err := os.Stat(cardDAVKeyPath())
		return err == nil
	}

	t.Run("kept while logins still use it", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		writeTestCardDAVKey(t, make([]byte, 32))
		removeLegacyCardDAVKeyIfUnused(ctx, fixed(true, nil), log)
		if !keyExists() {
			t.Error("key file deleted while a login still needs it")
		}
	})
	t.Run("kept when the check fails", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		writeTestCardDAVKey(t, make([]byte, 32))
		removeLegacyCardDAVKeyIfUnused(ctx, fixed(false, errors.New("db down")), log)
		if !keyExists() {
			t.Error("key file deleted although the DB check failed")
		}
	})
	t.Run("deleted when unused", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		writeTestCardDAVKey(t, make([]byte, 32))
		removeLegacyCardDAVKeyIfUnused(ctx, fixed(false, nil), log)
		if keyExists() {
			t.Error("key file should be deleted once no login uses it")
		}
	})
	t.Run("no key file skips the DB check", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		called := false
		removeLegacyCardDAVKeyIfUnused(ctx, func(context.Context) (bool, error) {
			called = true
			return false, nil
		}, log)
		if called {
			t.Error("DB check should not run when there is no key file")
		}
	})
}

func TestCountLegacyCardDAVPasswords(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		metaType := "TEXT"
		if db.Dialect == dbutil.Postgres {
			metaType = "jsonb"
		}
		// Minimal stand-in for bridgev2's user_login table: only the
		// metadata column matters to the query.
		if _, err := db.Exec(ctx, `CREATE TABLE user_login (bridge_id TEXT NOT NULL, id TEXT NOT NULL, metadata `+metaType+` NOT NULL)`); err != nil {
			t.Fatalf("create table: %v", err)
		}
		insert := func(id string, meta *UserLoginMetadata) {
			t.Helper()
			raw, err := json.Marshal(meta)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, err := db.Exec(ctx, `INSERT INTO user_login (bridge_id, id, metadata) VALUES ('br', $1, $2)`, id, string(raw)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		count := func() int {
			t.Helper()
			n, err := countLegacyCardDAVPasswords(ctx, db)
			if err != nil {
				t.Fatalf("countLegacyCardDAVPasswords: %v", err)
			}
			return n
		}

		insert("plain", &UserLoginMetadata{CardDAVEmail: "a@b.com", CardDAVPassword: "pw"})
		insert("none", &UserLoginMetadata{})
		if n := count(); n != 0 {
			t.Errorf("count = %d, want 0 with no legacy rows", n)
		}

		insert("legacy1", &UserLoginMetadata{CardDAVEmail: "a@b.com", CardDAVPasswordEncrypted: "enc1"})
		insert("legacy2", &UserLoginMetadata{CardDAVEmail: "c@d.com", CardDAVPasswordEncrypted: "enc2"})
		if n := count(); n != 2 {
			t.Errorf("count = %d, want 2", n)
		}

		// Migrating one row (what LoadUserLogin's save does) leaves one.
		migrated, _ := json.Marshal(&UserLoginMetadata{CardDAVEmail: "a@b.com", CardDAVPassword: "pw1"})
		if _, err := db.Exec(ctx, `UPDATE user_login SET metadata=$1 WHERE id='legacy1'`, string(migrated)); err != nil {
			t.Fatalf("update: %v", err)
		}
		if n := count(); n != 1 {
			t.Errorf("count = %d, want 1 after migrating one row", n)
		}
	})
}
