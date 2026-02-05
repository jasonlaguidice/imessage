// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/imessage/imessage"
)

// makeUserID creates a networkid.UserID from an iMessage identifier (phone/email).
func makeUserID(identifier string) networkid.UserID {
	return networkid.UserID(identifier)
}

// makePortalID creates a networkid.PortalID from an iMessage chat GUID.
// Chat GUIDs are like "iMessage;-;+1234567890" or "iMessage;+;chat123456".
func makePortalID(chatGUID string) networkid.PortalID {
	return networkid.PortalID(chatGUID)
}

// makeMessageID creates a networkid.MessageID from an iMessage message GUID.
func makeMessageID(guid string) networkid.MessageID {
	return networkid.MessageID(guid)
}

// makePortalKey creates a networkid.PortalKey from a chat GUID.
// iMessage chats are per-user (single login), so receiver = login ID.
func makePortalKey(chatGUID string, loginID networkid.UserLoginID) networkid.PortalKey {
	parsed := imessage.ParseIdentifier(chatGUID)
	key := networkid.PortalKey{
		ID: makePortalID(chatGUID),
	}
	// DM portals get a receiver (per-user), group chats are shared
	if !parsed.IsGroup {
		key.Receiver = loginID
	}
	return key
}

// makeUserLoginID creates a UserLoginID. Since iMessage has only one "login"
// (the local macOS user), we use a fixed ID.
func makeUserLoginID() networkid.UserLoginID {
	return "imessage"
}
