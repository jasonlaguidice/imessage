package connector

import (
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/lrhodin/imessage/imessage"
)

func TestCardDAVConfig_IsConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  CardDAVConfig
		want bool
	}{
		{"both set", CardDAVConfig{Email: "a@b.com", Password: "pw"}, true},
		{"no email", CardDAVConfig{Password: "pw"}, false},
		{"no password", CardDAVConfig{Email: "a@b.com"}, false},
		{"empty", CardDAVConfig{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsConfigured(); got != tt.want {
				t.Errorf("IsConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCardDAVConfig_GetUsername(t *testing.T) {
	t.Run("explicit username", func(t *testing.T) {
		cfg := CardDAVConfig{Email: "a@b.com", Username: "user"}
		if got := cfg.GetUsername(); got != "user" {
			t.Errorf("GetUsername() = %q, want %q", got, "user")
		}
	})
	t.Run("falls back to email", func(t *testing.T) {
		cfg := CardDAVConfig{Email: "a@b.com"}
		if got := cfg.GetUsername(); got != "a@b.com" {
			t.Errorf("GetUsername() = %q, want %q", got, "a@b.com")
		}
	})
}

func TestNewExternalCardDAVClient_UsesPlainPassword(t *testing.T) {
	// URL set, so no discovery (no network): the client must carry the
	// metadata password as-is.
	cfg := CardDAVConfig{Email: "a@b.com", URL: "https://dav.example.com/", Password: "app-pw"}
	client := newExternalCardDAVClient(cfg, nil, zerolog.Nop())
	if client == nil {
		t.Fatal("newExternalCardDAVClient returned nil for a configured login")
	}
	if client.password != "app-pw" || client.username != "a@b.com" || client.baseURL != "https://dav.example.com" {
		t.Errorf("client = {%q %q %q}, want {https://dav.example.com a@b.com app-pw}", client.baseURL, client.username, client.password)
	}
}

func TestNewExternalCardDAVClient_NotConfigured(t *testing.T) {
	cfg := CardDAVConfig{Email: "a@b.com", URL: "https://dav.example.com/"}
	if client := newExternalCardDAVClient(cfg, nil, zerolog.Nop()); client != nil {
		t.Error("newExternalCardDAVClient should return nil without a password")
	}
}

func TestIMClient_ExternalCardDAVConfig_PerUserOnly(t *testing.T) {
	t.Run("from metadata", func(t *testing.T) {
		c := &IMClient{Main: &IMConnector{}, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{Metadata: &UserLoginMetadata{
			CardDAVEmail:    "a@b.com",
			CardDAVURL:      "https://dav.example.com/",
			CardDAVUsername: "user",
			CardDAVPassword: "pw",
		}}}}
		want := CardDAVConfig{Email: "a@b.com", URL: "https://dav.example.com/", Username: "user", Password: "pw"}
		if got := c.externalCardDAVConfig(); got != want {
			t.Errorf("externalCardDAVConfig() = %+v, want %+v", got, want)
		}
	})
	t.Run("legacy encrypted only is not configured", func(t *testing.T) {
		c := &IMClient{Main: &IMConnector{}, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{Metadata: &UserLoginMetadata{
			CardDAVEmail:             "a@b.com",
			CardDAVPasswordEncrypted: "enc",
		}}}}
		cfg := c.externalCardDAVConfig()
		if cfg.IsConfigured() {
			t.Errorf("externalCardDAVConfig() = %+v, should not be configured from the legacy field", cfg)
		}
	})
	t.Run("no metadata, no global fallback", func(t *testing.T) {
		c := &IMClient{Main: &IMConnector{}, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{}}}
		if got := c.externalCardDAVConfig(); got != (CardDAVConfig{}) {
			t.Errorf("externalCardDAVConfig() = %+v, want zero value", got)
		}
	})
}

func TestResolveProviderURL(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"user@gmail.com", "https://www.googleapis.com/carddav/v1/principals/user@gmail.com/lists/default/"},
		{"user@googlemail.com", "https://www.googleapis.com/carddav/v1/principals/user@googlemail.com/lists/default/"},
		{"user@fastmail.com", "https://carddav.fastmail.com/dav/addressbooks/user/user@fastmail.com/Default/"},
		{"user@fastmail.fm", "https://carddav.fastmail.com/dav/addressbooks/user/user@fastmail.fm/Default/"},
		{"user@icloud.com", "https://contacts.icloud.com"},
		{"user@me.com", "https://contacts.icloud.com"},
		{"user@mac.com", "https://contacts.icloud.com"},
		{"user@yahoo.com", "https://carddav.address.yahoo.com/dav/user@yahoo.com/"},
		{"user@unknown-provider.com", ""},
		{"invalid-email", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := ResolveProviderURL(tt.email)
			if got != tt.want {
				t.Errorf("ResolveProviderURL(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

func TestResolveRedirect(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		location string
		want     string
	}{
		{"absolute URL", "https://example.com/.well-known/carddav", "https://other.com/dav/", "https://other.com/dav/"},
		{"http absolute", "https://example.com/path", "http://other.com/dav/", "http://other.com/dav/"},
		{"relative path", "https://example.com/.well-known/carddav", "/dav/principals/", "https://example.com/dav/principals/"},
		{"just path no match", "https://example.com", "relative", "relative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRedirect(tt.baseURL, tt.location)
			if got != tt.want {
				t.Errorf("resolveRedirect(%q, %q) = %q, want %q", tt.baseURL, tt.location, got, tt.want)
			}
		})
	}
}

func TestExternalCardDAVClient_ResolveURL(t *testing.T) {
	client := &externalCardDAVClient{
		baseURL: "https://carddav.example.com/dav",
	}

	tests := []struct {
		href string
		want string
	}{
		{"https://other.com/path", "https://other.com/path"},
		{"http://other.com/path", "http://other.com/path"},
		{"/dav/addressbooks/user/", "https://carddav.example.com/dav/addressbooks/user/"},
		{"/path", "https://carddav.example.com/path"},
	}
	for _, tt := range tests {
		t.Run(tt.href, func(t *testing.T) {
			got := client.resolveURL(tt.href)
			if got != tt.want {
				t.Errorf("resolveURL(%q) = %q, want %q", tt.href, got, tt.want)
			}
		})
	}
}

func TestExternalCardDAVClient_GetContactInfo_Empty(t *testing.T) {
	client := &externalCardDAVClient{
		byPhone: make(map[string]*imessage.Contact),
		byEmail: make(map[string]*imessage.Contact),
	}
	contact, err := client.GetContactInfo("+15551234567")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contact != nil {
		t.Error("expected nil contact for empty directory")
	}
}

func TestExternalCardDAVClient_GetContactInfo_ByPhone(t *testing.T) {
	alice := &imessage.Contact{FirstName: "Alice", Phones: []string{"+15551234567"}}
	client := &externalCardDAVClient{
		byPhone: map[string]*imessage.Contact{
			"+15551234567": alice,
			"15551234567":  alice,
			"5551234567":   alice,
			"1234567":      alice,
		},
		byEmail: make(map[string]*imessage.Contact),
	}

	contact, err := client.GetContactInfo("+15551234567")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contact == nil || contact.FirstName != "Alice" {
		t.Error("expected Alice contact")
	}
}

func TestExternalCardDAVClient_GetContactInfo_ByEmail(t *testing.T) {
	bob := &imessage.Contact{FirstName: "Bob", Emails: []string{"bob@example.com"}}
	client := &externalCardDAVClient{
		byPhone: make(map[string]*imessage.Contact),
		byEmail: map[string]*imessage.Contact{
			"bob@example.com": bob,
		},
	}

	contact, err := client.GetContactInfo("bob@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contact == nil || contact.FirstName != "Bob" {
		t.Error("expected Bob contact")
	}
}

func TestExternalCardDAVClient_GetAllContacts(t *testing.T) {
	alice := &imessage.Contact{FirstName: "Alice"}
	bob := &imessage.Contact{FirstName: "Bob"}
	client := &externalCardDAVClient{
		contacts: []*imessage.Contact{alice, bob},
	}

	all := client.GetAllContacts()
	if len(all) != 2 {
		t.Fatalf("got %d contacts, want 2", len(all))
	}
}
