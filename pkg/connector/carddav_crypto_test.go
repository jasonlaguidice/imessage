package connector

import (
	"testing"
)

func testKey() []byte {
	return DeriveCardDAVKey("test-bridge-secret", "test-user-login-id")
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey()
	password := "my-secret-app-password"
	encrypted, err := EncryptCardDAVPassword(key, password)
	if err != nil {
		t.Fatalf("EncryptCardDAVPassword error: %v", err)
	}
	if encrypted == "" {
		t.Fatal("encrypted should not be empty")
	}
	if encrypted == password {
		t.Fatal("encrypted should differ from plaintext")
	}

	decrypted, err := DecryptCardDAVPassword(key, encrypted)
	if err != nil {
		t.Fatalf("DecryptCardDAVPassword error: %v", err)
	}
	if decrypted != password {
		t.Errorf("decrypted = %q, want %q", decrypted, password)
	}
}

func TestEncryptDecryptRoundTrip_LongPassword(t *testing.T) {
	key := testKey()
	password := "this-is-a-very-long-password-with-special-chars-!@#$%^&*()"
	encrypted, err := EncryptCardDAVPassword(key, password)
	if err != nil {
		t.Fatalf("EncryptCardDAVPassword error: %v", err)
	}

	decrypted, err := DecryptCardDAVPassword(key, encrypted)
	if err != nil {
		t.Fatalf("DecryptCardDAVPassword error: %v", err)
	}
	if decrypted != password {
		t.Errorf("decrypted = %q, want %q", decrypted, password)
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	key := testKey()
	encrypted, err := EncryptCardDAVPassword(key, "my-password")
	if err != nil {
		t.Fatalf("EncryptCardDAVPassword error: %v", err)
	}

	wrongKey := DeriveCardDAVKey("different-bridge-secret", "different-user")
	_, err = DecryptCardDAVPassword(wrongKey, encrypted)
	if err == nil {
		t.Error("DecryptCardDAVPassword should fail with wrong key")
	}
}

func TestDecryptInvalidBase64(t *testing.T) {
	key := testKey()
	_, err := DecryptCardDAVPassword(key, "not-valid-base64!!!")
	if err == nil {
		t.Error("DecryptCardDAVPassword should fail with invalid base64")
	}
}

func TestDecryptTooShort(t *testing.T) {
	key := testKey()
	_, err := DecryptCardDAVPassword(key, "AA==")
	if err == nil {
		t.Error("DecryptCardDAVPassword should fail with too-short ciphertext")
	}
}

func TestDeriveCardDAVKey_Deterministic(t *testing.T) {
	key1 := DeriveCardDAVKey("secret", "user1")
	key2 := DeriveCardDAVKey("secret", "user1")
	if string(key1) != string(key2) {
		t.Error("DeriveCardDAVKey should be deterministic for same inputs")
	}
}

func TestDeriveCardDAVKey_PerUser(t *testing.T) {
	key1 := DeriveCardDAVKey("secret", "user1")
	key2 := DeriveCardDAVKey("secret", "user2")
	if string(key1) == string(key2) {
		t.Error("DeriveCardDAVKey should produce different keys for different users")
	}
}

func TestDeriveCardDAVKey_Length(t *testing.T) {
	key := DeriveCardDAVKey("secret", "user1")
	if len(key) != 32 {
		t.Errorf("DeriveCardDAVKey length = %d, want 32", len(key))
	}
}
