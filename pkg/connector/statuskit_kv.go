package connector

import (
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// statusKitLoginKey scopes a StatusKit KV key to one login:
// "<prefix><loginID>.<suffix>". Every StatusKit latch, cooldown, presence and
// alias row is per login: two logins that share a contact must not share
// invite latches, presence dedup or alias mappings. Pre-scoping global rows
// are simply never read again. Every prefix used with this must be listed in
// loginScopedKVPrefixes so logout deletes its keys.
func statusKitLoginKey(loginID networkid.UserLoginID, prefix, suffix string) database.Key {
	return database.Key(prefix + string(loginID) + "." + suffix)
}

// statusKitKey returns this login's KV key for prefix+suffix.
func (c *IMClient) statusKitKey(prefix, suffix string) database.Key {
	return statusKitLoginKey(c.UserLogin.ID, prefix, suffix)
}

// statusKitPresenceKeyPrefix persists the last-seen presence mode per raw handle.
const statusKitPresenceKeyPrefix = "statuskit.presence."

// statusKitPresencePortalKeyPrefix persists the last-seen presence mode per portal (canonical dedup).
const statusKitPresencePortalKeyPrefix = "statuskit.presence.portal."

// statusKitLastPostInviteShareKVKey returns this login's post-invite
// share_status cooldown key ("statuskit.last_post_invite_share.<loginID>").
func (c *IMClient) statusKitLastPostInviteShareKVKey() database.Key {
	return statusKitLastPostInviteShareKVKeyFor(c.UserLogin.ID)
}

// statusKitLastPostInviteShareKVKeyFor returns loginID's post-invite
// share_status cooldown key.
func statusKitLastPostInviteShareKVKeyFor(loginID networkid.UserLoginID) database.Key {
	return database.Key(statusKitLastPostInviteShareKey + "." + string(loginID))
}
