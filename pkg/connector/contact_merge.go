// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

// Contact-based DM portal merging.
//
// When a contact has multiple phone numbers or emails, iMessage stores each
// as a separate conversation. Without merging, the bridge creates separate
// Matrix rooms for each number. This file provides helpers to redirect
// incoming messages from a secondary phone number to an existing primary portal.

import (
	"context"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/imessage/imessage"
)

// resolveContactPortalID checks if the given DM identifier belongs to a contact
// that already has an existing portal under a different phone number or email.
// Returns the original identifier (as a PortalID) if no existing portal is found.
func (c *IMClient) resolveContactPortalID(identifier string) networkid.PortalID {
	defaultID := networkid.PortalID(identifier)

	if strings.Contains(identifier, ",") {
		return defaultID
	}

	contact := c.lookupContact(identifier)
	if contact == nil || !contact.HasName() {
		return defaultID
	}

	altIDs := contactPortalIDs(contact)
	if len(altIDs) <= 1 {
		return defaultID
	}

	ctx := context.Background()
	for _, altID := range altIDs {
		if altID == identifier {
			continue
		}
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       networkid.PortalID(altID),
			Receiver: c.UserLogin.ID,
		})
		if err == nil && portal != nil && portal.MXID != "" {
			c.UserLogin.Log.Debug().
				Str("original", identifier).
				Str("resolved", altID).
				Msg("Resolved contact portal to existing portal")
			return networkid.PortalID(altID)
		}
	}

	return defaultID
}

// resolveSendTarget determines the best identifier to send to for a DM portal.
func (c *IMClient) resolveSendTarget(portalID string) string {
	if c.client == nil || strings.Contains(portalID, ",") {
		return portalID
	}

	contact := c.lookupContact(portalID)
	if contact == nil || len(contactPortalIDs(contact)) <= 1 {
		return portalID
	}

	valid := c.client.ValidateTargets([]string{portalID}, c.handle)
	if len(valid) > 0 {
		return portalID
	}

	c.UserLogin.Log.Info().
		Str("portal_id", portalID).
		Msg("Portal ID not reachable on iMessage, trying alternate contact numbers")

	for _, altID := range contactPortalIDs(contact) {
		if altID == portalID {
			continue
		}
		valid := c.client.ValidateTargets([]string{altID}, c.handle)
		if len(valid) > 0 {
			c.UserLogin.Log.Info().
				Str("portal_id", portalID).
				Str("send_target", altID).
				Msg("Resolved send target to alternate contact number")
			return altID
		}
	}

	c.UserLogin.Log.Warn().
		Str("portal_id", portalID).
		Msg("No reachable number found for contact")
	return portalID
}

// lookupContact resolves a portal/identifier string to a Contact using
// cloud contacts (iCloud CardDAV).
func (c *IMClient) lookupContact(identifier string) *imessage.Contact {
	localID := stripIdentifierPrefix(identifier)
	if localID == "" {
		return nil
	}

	if c.contacts != nil {
		contact, _ := c.contacts.GetContactInfo(localID)
		return contact
	}
	return nil
}

// contactPortalIDs returns all portal ID strings for a contact's phone numbers
// and emails.
func contactPortalIDs(contact *imessage.Contact) []string {
	if contact == nil {
		return nil
	}

	seen := make(map[string]bool)
	var ids []string

	for _, phone := range contact.Phones {
		normalized := normalizePhoneForPortalID(phone)
		if normalized == "" {
			continue
		}
		pid := "tel:" + normalized
		if !seen[pid] {
			seen[pid] = true
			ids = append(ids, pid)
		}
	}

	for _, email := range contact.Emails {
		pid := "mailto:" + strings.ToLower(email)
		if !seen[pid] {
			seen[pid] = true
			ids = append(ids, pid)
		}
	}

	return ids
}

// normalizePhoneForPortalID converts a phone number to E.164-like format.
func normalizePhoneForPortalID(phone string) string {
	n := normalizePhone(phone)
	if n == "" {
		return ""
	}
	if strings.HasPrefix(n, "+") {
		return n
	}
	if len(n) == 10 {
		return "+1" + n
	}
	if len(n) == 11 && n[0] == '1' {
		return "+" + n
	}
	return "+" + n
}
