package connector

import (
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

func newTestClientForLogin(id networkid.UserLoginID) *IMClient {
	return &IMClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: id}}}
}

// TestLastFullConnectKVKeyIsPerLogin guards the multi-user fix: one login's
// full connect must not start the IDS-sweep cooldown for every other login.
func TestLastFullConnectKVKeyIsPerLogin(t *testing.T) {
	a := newTestClientForLogin("login-a").lastFullConnectKVKey()
	b := newTestClientForLogin("login-b").lastFullConnectKVKey()
	if a == b {
		t.Fatalf("both logins share the key %q", a)
	}
	if a == database.Key("im.last_full_connect") {
		t.Fatalf("key %q collides with the legacy global key", a)
	}
}

// TestDisconnectIsSerialized runs overlapping Disconnects, as LogoutRemote
// racing a rebuild's LoadUserLogin does. Without disconnectMu both frames can
// pass the stopChan nil-check and double-close it.
func TestDisconnectIsSerialized(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := newTestClientForLogin("login-a")
		c.stopChan = make(chan struct{})
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.Disconnect()
			}()
		}
		wg.Wait()
		if c.stopChan != nil {
			t.Fatal("stopChan not cleared")
		}
	}
}

// TestIsCurrentClient guards the persistState stale-write check: a client
// that bridgev2 has replaced on the same UserLogin must not save.
func TestIsCurrentClient(t *testing.T) {
	old := newTestClientForLogin("login-a")
	old.UserLogin.Client = old
	if !old.isCurrentClient() {
		t.Fatal("attached client reported as replaced")
	}
	successor := &IMClient{UserLogin: old.UserLogin}
	old.UserLogin.Client = successor
	if old.isCurrentClient() {
		t.Fatal("replaced client reported as current")
	}
	if !successor.isCurrentClient() {
		t.Fatal("successor reported as replaced")
	}
}
