// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// Tests for contact-photo failure classification and the ghost profile
// helpers built from address-book contacts. Adapted from upstream
// corten-matrix 108a196e.

package connector

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/lrhodin/imessage/imessage"
)

// TestPhotoFailurePermanentForHost pins the host-aware classification: a
// 401/403 from Apple's own photo CDN is transient (rate limiting), so it is not
// dead-cached. A 404/410 is a gone photo for every host, and a third-party
// 401/403 is a genuine block, so both stay permanent.
func TestPhotoFailurePermanentForHost(t *testing.T) {
	appleURL := "https://gateway.icloud.com/contacts/photo/abc"
	otherURL := "https://img.contactsplus.com/photo/abc"

	cases := []struct {
		name string
		err  error
		url  string
		want bool
	}{
		{"apple 403 transient", errors.New("HTTP 403 fetching " + appleURL), appleURL, false},
		{"apple 401 transient", errors.New("HTTP 401 fetching " + appleURL), appleURL, false},
		{"apple 404 permanent", errors.New("HTTP 404 fetching " + appleURL), appleURL, true},
		{"apple 410 permanent", errors.New("HTTP 410 fetching " + appleURL), appleURL, true},
		{"third-party 403 permanent", errors.New("HTTP 403 fetching " + otherURL), otherURL, true},
		{"third-party 404 permanent", errors.New("HTTP 404 fetching " + otherURL), otherURL, true},
		{"apple 500 transient", errors.New("HTTP 500 fetching " + appleURL), appleURL, false},
		{"apple 429 transient", errors.New("HTTP 429 fetching " + appleURL), appleURL, false},
		{"nil error", nil, appleURL, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := photoFailurePermanentForHost(tc.err, tc.url); got != tc.want {
				t.Errorf("photoFailurePermanentForHost(%v, %s) = %v, want %v", tc.err, tc.url, got, tc.want)
			}
		})
	}
}

func TestContactIdentifiers(t *testing.T) {
	cases := []struct {
		name       string
		identifier string
		contact    *imessage.Contact
		want       []string
	}{
		{
			name:       "phone formatting normalised and deduped",
			identifier: "tel:+15555550123",
			contact:    &imessage.Contact{Phones: []string{"(555) 555-0123", "+1 555 555 0199"}},
			want:       []string{"tel:+15555550123", "tel:+15555550199"},
		},
		{
			name:       "email dropped when a phone exists",
			identifier: "tel:+15555550123",
			contact: &imessage.Contact{
				Phones: []string{"555-555-0123"},
				Emails: []string{"Someone@Example.com"},
			},
			want: []string{"tel:+15555550123"},
		},
		{
			name:       "email ghost with phone contact publishes phones only",
			identifier: "mailto:someone@example.com",
			contact: &imessage.Contact{
				Phones: []string{"555-555-0123"},
				Emails: []string{"someone@example.com", "other@example.com"},
			},
			want: []string{"mailto:someone@example.com", "tel:+15555550123"},
		},
		{
			name:       "email-only contact keeps lowercased emails",
			identifier: "mailto:someone@example.com",
			contact:    &imessage.Contact{Emails: []string{"Someone@Example.com", "Other@Example.com"}},
			want:       []string{"mailto:someone@example.com", "mailto:other@example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contactIdentifiers(tc.identifier, tc.contact)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("contactIdentifiers() = %v, want %v", got, tc.want)
			}
			for _, id := range got {
				if strings.ContainsAny(id, " ()-") && strings.HasPrefix(id, "tel:") {
					t.Errorf("malformed tel identifier %q", id)
				}
			}
		})
	}
}

func TestContactAvatar(t *testing.T) {
	ctx := context.Background()
	const id = "tel:+15555550123"

	t.Run("no photo returns nil", func(t *testing.T) {
		if av := contactAvatar(id, &imessage.Contact{}, func() *imessage.Contact { return nil }); av != nil {
			t.Fatalf("expected nil avatar, got %+v", av)
		}
	})

	t.Run("cached bytes", func(t *testing.T) {
		ct := &imessage.Contact{Avatar: []byte("jpeg"), AvatarURL: "https://p.icloud.com/x"}
		av := contactAvatar(id, ct, func() *imessage.Contact { return nil })
		if av == nil || strings.Contains(string(av.ID), ":url:") {
			t.Fatalf("expected byte-keyed avatar, got %+v", av)
		}
		data, err := av.Get(ctx)
		if err != nil || string(data) != "jpeg" {
			t.Fatalf("Get() = %q, %v", data, err)
		}
	})

	t.Run("known URL without bytes is deferred, not nil", func(t *testing.T) {
		ct := &imessage.Contact{AvatarURL: "https://p.icloud.com/x"}
		var fresh *imessage.Contact
		av := contactAvatar(id, ct, func() *imessage.Contact { return fresh })
		if av == nil {
			t.Fatal("nil avatar would latch AvatarSet=true in bridgev2")
		}
		if !strings.Contains(string(av.ID), ":url:") {
			t.Errorf("deferred avatar ID should be URL-keyed, got %q", av.ID)
		}
		if _, err := av.Get(ctx); !errors.Is(err, errContactPhotoPending) {
			t.Errorf("Get() before download err = %v, want errContactPhotoPending", err)
		}
		fresh = &imessage.Contact{Avatar: []byte("later")}
		if data, err := av.Get(ctx); err != nil || string(data) != "later" {
			t.Errorf("Get() after download = %q, %v", data, err)
		}
		again := contactAvatar(id, ct, func() *imessage.Contact { return nil })
		if again.ID != av.ID {
			t.Errorf("deferred avatar ID not stable: %q vs %q", again.ID, av.ID)
		}
	})
}

// TestDownloadContactPhotosKeepsURLAndCachesOnlyDead checks that a successful
// download keeps AvatarURL (so carryOverAvatars can match it next sync), that
// an Apple-host 403 is retried rather than dead-cached, and that a 404 is
// dead-cached in the calling user's cache only.
func TestDownloadContactPhotosKeepsURLAndCachesOnlyDead(t *testing.T) {
	okURL := "https://p1.icloud.com/test/ok"
	limitedURL := "https://p1.icloud.com/test/ratelimited"
	goneURL := "https://p1.icloud.com/test/gone"
	var dead, otherUser deadPhotoURLCache

	fetch := func(_ context.Context, url string) ([]byte, error) {
		switch url {
		case okURL:
			return []byte("jpeg"), nil
		case limitedURL:
			return nil, errors.New("HTTP 403 fetching " + url)
		default:
			return nil, errors.New("HTTP 404 fetching " + url)
		}
	}
	ok := &imessage.Contact{AvatarURL: okURL}
	limited := &imessage.Contact{AvatarURL: limitedURL}
	gone := &imessage.Contact{AvatarURL: goneURL}
	downloadContactPhotos([]*imessage.Contact{ok, limited, gone}, &dead, zerolog.Nop(), fetch)

	if string(ok.Avatar) != "jpeg" || ok.AvatarURL != okURL {
		t.Errorf("successful download: Avatar=%q AvatarURL=%q, want bytes and URL kept", ok.Avatar, ok.AvatarURL)
	}
	if dead.isDead(limitedURL) {
		t.Error("Apple-host 403 must not be dead-cached")
	}
	if limited.AvatarURL == "" {
		t.Error("Apple-host 403 must keep AvatarURL so the ghost gets a deferred avatar")
	}
	if !dead.isDead(goneURL) {
		t.Error("404 should be dead-cached")
	}
	if otherUser.isDead(goneURL) {
		t.Error("a dead URL must not leak into another user's cache")
	}
}
