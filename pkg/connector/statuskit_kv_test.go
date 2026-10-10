package connector

import (
	"strings"
	"testing"
)

func TestStatusKitLoginKeyScopesByLogin(t *testing.T) {
	a := statusKitLoginKey("loginA", statusKitInvitedOkKeyPrefix, "tel:+15551234567")
	if a != "statuskit.invited_ok.loginA.tel:+15551234567" {
		t.Fatalf("unexpected key %q", a)
	}
	b := statusKitLoginKey("loginB", statusKitInvitedOkKeyPrefix, "tel:+15551234567")
	if a == b {
		t.Fatalf("keys for different logins must differ: %q", a)
	}
	p := statusKitLoginKey("loginA", statusKitAliasPortalKeyPrefix, "")
	if !strings.HasPrefix(string(p), "statuskit.alias_portal.loginA.") {
		t.Fatalf("unexpected scan prefix %q", p)
	}
}
