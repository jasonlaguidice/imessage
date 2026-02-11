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
// as a separate conversation in chat.db. Without merging, the bridge creates
// separate Matrix rooms for each number. This file provides helpers to:
//
//  1. During initial sync: detect and skip duplicate DM entries for the same
//     contact, keeping only the most recently active phone number's portal.
//
//  2. During backfill (FetchMessages): include chat.db GUIDs from ALL of
//     the contact's phone numbers so messages from both numbers appear in
//     one room.
//
//  3. During real-time message routing: redirect incoming messages from a
//     secondary phone number to the existing primary portal.

import (
	"context"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/imessage/imessage"
)

// resolveContactPortalID checks if the given DM identifier belongs to a contact
// that already has an existing portal under a different phone number or email.
// This merges conversations for contacts with multiple numbers into a single room.
// Returns the original identifier (as a PortalID) if no existing portal is found.
func (c *IMClient) resolveContactPortalID(identifier string) networkid.PortalID {
	defaultID := networkid.PortalID(identifier)

	// Only resolve DM identifiers (not groups)
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

	// Check if any alternate identifier already has a portal in the database
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
// For merged contacts with multiple numbers, the portal ID might be a number
// that's no longer active on iMessage (no IDS keys). This validates the target
// and falls back to alternate contact numbers that are reachable.
func (c *IMClient) resolveSendTarget(portalID string) string {
	if c.client == nil || strings.Contains(portalID, ",") {
		return portalID
	}

	// Only do validation for contacts with multiple identifiers —
	// single-number contacts skip the overhead entirely.
	contact := c.lookupContact(portalID)
	if contact == nil || len(contactPortalIDs(contact)) <= 1 {
		return portalID
	}

	// Multi-number contact: validate the portal ID is reachable on iMessage
	valid := c.client.ValidateTargets([]string{portalID}, c.handle)
	if len(valid) > 0 {
		return portalID
	}

	// Portal ID is not reachable — try alternate numbers for this contact
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

	// No valid target found, return original (will likely fail but gives
	// a clear error rather than silently dropping)
	c.UserLogin.Log.Warn().
		Str("portal_id", portalID).
		Msg("No reachable number found for contact")
	return portalID
}

// getContactChatGUIDs returns all possible chat.db GUIDs for a DM portal,
// including GUIDs for alternate phone numbers/emails belonging to the same contact.
// For contacts with a single phone number, this is equivalent to portalIDToChatGUIDs.
func (c *IMClient) getContactChatGUIDs(portalID string) []string {
	guids := portalIDToChatGUIDs(portalID)

	contact := c.lookupContact(portalID)
	if contact == nil {
		return guids
	}

	for _, altID := range contactPortalIDs(contact) {
		if altID == portalID {
			continue
		}
		guids = append(guids, portalIDToChatGUIDs(altID)...)
	}

	return guids
}

// lookupContact resolves a portal/identifier string to a Contact using
// whatever contact source is available (cloud contacts or chat.db).
func (c *IMClient) lookupContact(identifier string) *imessage.Contact {
	localID := stripIdentifierPrefix(identifier)
	if localID == "" {
		return nil
	}

	var contact *imessage.Contact
	if c.cloudContacts != nil {
		contact, _ = c.cloudContacts.GetContactInfo(localID)
	}
	if contact == nil && c.chatDB != nil {
		contact, _ = c.chatDB.api.GetContactInfo(localID)
	}
	return contact
}

// contactPortalIDs returns all portal ID strings for a contact's phone numbers
// and emails, normalized to match the format used in portal keys (e.g., "tel:+15551234567").
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

// normalizePhoneForPortalID converts a phone number from any format to the
// E.164-like format used in chat.db identifiers and portal IDs.
//
// Examples:
//
//	"+1 (555) 111-1111" → "+15551111111"
//	"(555) 111-1111"    → "+15551111111"  (assumes US)
//	"+15551111111"      → "+15551111111"
//	"+447911123456"     → "+447911123456"
//
// Note: 10-digit numbers without a country code are assumed to be US (+1).
// International numbers stored without a country code may not normalize correctly.
func normalizePhoneForPortalID(phone string) string {
	n := normalizePhone(phone)
	if n == "" {
		return ""
	}
	// Already has country code prefix
	if strings.HasPrefix(n, "+") {
		return n
	}
	// 10 digits: US number without country code
	if len(n) == 10 {
		return "+1" + n
	}
	// 11 digits starting with 1: US number with country code but missing +
	if len(n) == 11 && n[0] == '1' {
		return "+" + n
	}
	// Best effort for other formats
	return "+" + n
}

// contactKeyFromContact returns a stable identity key for grouping a contact's
// DM entries during initial sync deduplication. Returns "" if no merging is
// needed (single phone, no name, etc.).
func contactKeyFromContact(contact *imessage.Contact) string {
	if contact == nil || !contact.HasName() {
		return ""
	}
	phones := make([]string, 0, len(contact.Phones))
	for _, p := range contact.Phones {
		n := normalizePhoneForPortalID(p)
		if n != "" {
			phones = append(phones, n)
		}
	}
	if len(phones) <= 1 {
		return "" // No merging needed for single-phone contacts
	}
	sort.Strings(phones)
	return strings.Join(phones, "|")
}
