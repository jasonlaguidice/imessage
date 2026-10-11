// mautrix-imessage - A Matrix-iMessage puppeting bridge.
//
// Ghost profile helpers derived from an address-book contact: the Matrix
// identifier list and the avatar handed to bridgev2 in GetUserInfo. Kept free
// of IMClient state so both contact sources (iCloud CardDAV and external
// CardDAV) share one tested code path.

package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/imessage/imessage"
)

// errContactPhotoPending is returned by a deferred contact avatar's Get while
// the photo bytes have not been downloaded yet.
var errContactPhotoPending = errors.New("contact photo pending download")

// contactIdentifiers returns the Matrix identifier list for a ghost whose
// address-book entry is contact. The ghost's own identifier always comes first.
//
// Identifiers go through contactPortalIDs, so phones are E.164-normalised and
// emails lowercased and deduped. Raw vCard phone strings carry display
// formatting (e.g. "(XXX) XXX-XXXX"), and "tel:"+phone would publish a
// malformed URI that clients can't parse as a number.
//
// When the contact has any phone number, only tel: identifiers are published.
// bridgev2 sorts the identifier list, so "mailto:" sorts ahead of "tel:", and
// clients resolve a DM's contact card (avatar, call button) from the first
// identifier. An email next to the phone would hijack that card. Email-only
// contacts still publish their emails.
func contactIdentifiers(identifier string, contact *imessage.Contact) []string {
	ids := []string{identifier}
	portalIDs := contactPortalIDs(contact)
	hasPhone := strings.HasPrefix(identifier, "tel:")
	for _, id := range portalIDs {
		if strings.HasPrefix(id, "tel:") {
			hasPhone = true
			break
		}
	}
	seen := map[string]bool{identifier: true}
	for _, id := range portalIDs {
		if seen[id] {
			continue
		}
		if hasPhone && strings.HasPrefix(id, "mailto:") {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// contactAvatar returns the ghost avatar for an address-book contact, or nil
// when the contact has no photo at all.
//
// When the address book knows a photo URL but the bytes aren't cached yet
// (still downloading, or the last fetch hit a transient failure such as an
// iCloud rate limit), a deferred avatar is returned instead of nil. bridgev2
// treats a nil avatar as "never expecting one" and latches AvatarSet=true on
// an empty MXC, after which UpdateInfoIfNecessary never retries. The deferred
// avatar's Get re-reads the contact through lookup and errors until the bytes
// arrive, so the framework leaves AvatarSet=false and the next reconcile
// retries. Its ID is keyed on the photo URL, so it stays stable until the
// contact actually changes their picture.
func contactAvatar(identifier string, contact *imessage.Contact, lookup func() *imessage.Contact) *bridgev2.Avatar {
	if len(contact.Avatar) > 0 {
		avatarHash := sha256.Sum256(contact.Avatar)
		avatarData := contact.Avatar // capture for closure
		return &bridgev2.Avatar{
			ID: networkid.AvatarID(fmt.Sprintf("contact:%s:%s", identifier, hex.EncodeToString(avatarHash[:8]))),
			Get: func(ctx context.Context) ([]byte, error) {
				return avatarData, nil
			},
		}
	}
	if contact.AvatarURL == "" {
		return nil
	}
	urlHash := sha256.Sum256([]byte(contact.AvatarURL))
	return &bridgev2.Avatar{
		ID: networkid.AvatarID(fmt.Sprintf("contact:%s:url:%s", identifier, hex.EncodeToString(urlHash[:8]))),
		Get: func(ctx context.Context) ([]byte, error) {
			// Re-read through the contact source (under its lock) for the
			// freshest bytes rather than racing the download goroutines.
			if fresh := lookup(); fresh != nil && len(fresh.Avatar) > 0 {
				return fresh.Avatar, nil
			}
			return nil, errContactPhotoPending
		},
	}
}

// ghostProfileSnapshot captures the parts of a ghost's profile that clients
// see as m.room.member changes, so the contact reconcile can tell whether an
// UpdateInfo call actually changed anything worth pacing.
type ghostProfileSnapshot struct {
	name        string
	avatarMXC   string
	identifiers string
}

func snapshotGhostProfile(ghost *bridgev2.Ghost) ghostProfileSnapshot {
	return ghostProfileSnapshot{
		name:        ghost.Name,
		avatarMXC:   string(ghost.AvatarMXC),
		identifiers: strings.Join(ghost.Identifiers, "\x00"),
	}
}

func (s ghostProfileSnapshot) equal(other ghostProfileSnapshot) bool {
	return s == other
}
