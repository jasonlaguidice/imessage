// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

// errNoCarrierRoute fails a send that would go to a text-message relay on an
// account IDS reports has no device registered to relay. Such a send
// is accepted by Apple and then silently discarded, so reporting success is a
// lie; this surfaces it through the normal Matrix failure path instead.
//
// Built with an explicit message because corten's other send failures are bare
// fmt.Errorf, which produce a status event carrying no user-visible text at all.
// SendNotice and the message body are both set: Beeper-hosted config shows the
// status event, self-hosted shows the in-room notice.
var errNoCarrierRoute error = bridgev2.WrapErrorInStatus(
	errors.New("can't deliver: this chat goes over SMS and no device on this " +
		"account is set up to forward text messages")).
	WithIsCertain(true).
	WithErrorAsMessage().
	WithSendNotice(true).
	WithErrorReason(event.MessageStatusUnsupported)

// carrierRouteAction reports whether an outbound send is even a candidate for an
// IDS reachability check. Split out from checkCarrierRoute so the decision is
// testable without a live FFI client or a bridge DB.
type carrierRouteAction int

const (
	// carrierRouteProceed leaves the send on its existing routing, costing no
	// IDS traffic.
	carrierRouteProceed carrierRouteAction = iota
	// carrierRouteVerify marks a 1:1 carrier-flagged DM worth checking.
	carrierRouteVerify
)

// classifyCarrierRoute decides whether a send is a candidate, using only inputs
// that cost nothing to compute.
//
// Everything it can answer must be answered here, before the caller reaches the
// memos, the ceiling, the rate gate or the lookup. Keeping this gate free of
// network work is what keeps the ~98% of sends that are not carrier-flagged at
// exactly zero added IDS cost — for them it is one map read and a return.
//
// Only 1:1 carrier-flagged DMs qualify. Everything else proceeds untouched:
//
//   - Non-carrier portals are the overwhelming majority.
//   - Groups are excluded deliberately, and this is not a gap to be closed later.
//     A carrier group is keyed by its participant list while an iMessage group is
//     keyed by gid:, so "repairing" one is a portal re-ID, not a flag flip — a far
//     riskier operation than anything here. 34fcec8a, d3c97272 and 60987f80 all
//     exist to keep carrier groups' flag *set* and their keying stable.
//   - Portals whose ID still carries a raw "(smsft)"-style suffix are excluded
//     because isPortalSMS documents them as unable to ever transition to
//     iMessage. This costs no coverage: migrateSmsSuffixPortals (client.go:959)
//     re-IDs them to the stripped form at startup, after which they arrive here
//     as ordinary DMs.
func classifyCarrierRoute(portalID string, isSms bool) carrierRouteAction {
	if !isSms {
		return carrierRouteProceed
	}
	if isGroupPortalID(portalID) {
		return carrierRouteProceed
	}
	if stripSmsSuffix(portalID) != portalID {
		return carrierRouteProceed
	}
	return carrierRouteVerify
}

// checkCarrierRoute reports whether THIS ONE outbound message should be routed
// over iMessage even though its portal is flagged carrier.
//
// A portal is flagged carrier by CloudKit sync, chat.db migration or a live
// inbound SMS — all inbound- or sync-driven — and nothing re-derives it on the
// way out, because the carrier send path never queries the recipient's keys at
// all: rustpush addresses a carrier message to our *own* handle on
// com.apple.private.alloy.sms (messages.rs get_send_participants). So the flag
// sticks until the peer happens to message first.
//
// That is how a message disappears. Text-message forwarding is switched on for a
// while, real carrier conversations are created and their portals flagged, then
// forwarding is switched off — and every later send to those portals is still
// addressed to a relay that no longer exists. Apple accepts the push, the bridge
// returns a UUID, Matrix shows a checkmark, and nothing was delivered.
//
// # Why this returns a value instead of clearing the flag
//
// An earlier version cleared the portal's carrier flag outright, in memory and in
// the portal row. That is durable, portal-wide state, and every retro-active
// handler — edit, unsend, reaction, reaction-remove — reads it through
// portalToConversation. So one ordinary text send silently changed what those
// operations did: a reaction that used to go out as relay text the recipient
// actually received became a tapback against a GUID their device never held, and
// an unsend's honest "not supported for SMS conversations" refusal became a
// silent no-op that still redacted the Matrix event. Removing those handlers'
// calls did not help, because the flag write was upstream of all of them.
//
// The evidence this check gathers is only good enough for the message in hand.
// "The peer's handle is registered on iMessage" says nothing about which service
// carried some earlier message, which is what a retro-active operation needs (and
// which cloud_message.service already records, for whoever fixes those paths).
// So the answer is applied to one send and nothing is persisted.
//
// This never blocks or refuses a send. It can add latency: the rate gate is slept
// on synchronously, up to its interval times the queue depth.
func (c *IMClient) checkCarrierRoute(ctx context.Context, portal *bridgev2.Portal) bool {
	if portal == nil {
		return false
	}

	portalID := string(portal.ID)
	// Cheap gate first. Everything past the memos below can touch IDS, so a portal
	// that is not a carrier-flagged 1:1 DM must never get that far.
	if classifyCarrierRoute(portalID, c.isPortalSMS(portalID)) != carrierRouteVerify {
		return false
	}

	// Already asked, within the hour. Since nothing is persisted, these memos are
	// what stop every send to the same portal re-querying Apple.
	if c.recentlyVerifiedIMessage(portalID) {
		return true
	}
	if c.recentlyCheckedUnreachable(portalID) {
		return false
	}

	// Check the portal's OWN handle rather than the target portalToConversation
	// resolves. A "yes" here implies the portal handle itself validated, which
	// implies resolveSendTarget returned that same handle (contact_merge.go:108),
	// so a yes can never route over iMessage to a handle we did not validate.
	//
	// Known coverage gap, deliberately accepted: a merged contact whose portal
	// handle is dead but whose alternate is live gets no repair. Closing it means
	// validating the resolved target, and resolveSendTarget only yields an
	// alternate *after* IDS reported the portal handle unreachable — so that
	// version answered yes precisely where the evidence said no, and was reverted.
	//
	// Keeping it to the portal handle also means this function issues exactly one
	// lookup, so the ceiling and gate below cover all of its Apple traffic.
	sendTarget := stripSmsSuffix(portalID)

	// Self-chats address our own handle, so the reachability question is moot.
	if c.isMyHandle(sendTarget) {
		return false
	}

	// Placed here rather than at the top so every cheap exit above — classify, both
	// memos, self-chat — stays reachable without an FFI client, which is what makes
	// the zero-cost and ordering invariants testable. Nothing below works without it.
	if c.client == nil {
		return false
	}

	log := zerolog.Ctx(ctx).With().
		Str("component", "carrier_route").
		Str("portal_id", portalID).
		Logger()

	// Absolute ceiling across every portal, covering the single lookup below — all
	// the Apple traffic this function issues — so a bridge with hundreds of
	// carrier-flagged portals cannot walk a long tail of first-time checks into a
	// burst. Skipping the check only forgoes a repair.
	if !c.reserveCarrierCheck() {
		log.Warn().
			Int("max_per_hour", carrierCheckMaxPerHour).
			Msg("Carrier-route lookup ceiling reached; leaving routing unchanged")
		return false
	}

	// Pace the lookup. Sleeping outside the gate's mutex keeps context
	// cancellation observable, matching the StatusKit callers.
	if wait := c.sendPathIDSGate.reserve(); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}

	reachable := len(c.validateTargetsSafe([]string{sendTarget})) > 0

	// Always recordSuccess. On this path an empty result is a legitimate answer —
	// "this contact is not on iMessage" — not a signal that Apple is unhappy.
	// idsRateGate's failure counter has no time-based decay
	// (statuskit_alias_resolver.go:151-167), so counting the common correct answer
	// as a failure would climb the multiplier to its cap and put a 24s wait in
	// front of every later carrier send, on a send path that is synchronous and
	// serialized per portal.
	c.sendPathIDSGate.recordSuccess()

	if !reachable {
		c.noteCheckedUnreachable(portalID)
		log.Debug().
			Str("send_target", sendTarget).
			Msg("Carrier portal recipient not reachable on iMessage; leaving carrier routing in place")
		return false
	}

	c.noteVerifiedIMessage(portalID)
	log.Info().
		Str("send_target", sendTarget).
		Msg("Recipient is reachable on iMessage; routing this message over iMessage instead of the SMS relay")
	return true
}

// carrierCheckTTL matches the Rust KeyCache's EMPTY_REFRESH (identity_manager.rs:24),
// which governs how long an *empty* result stays fresh. A non-empty entry lives
// until Apple's session token expires, normally much longer — so a positive memo
// expiring costs a Rust cache hit and no Apple traffic.
const carrierCheckTTL = time.Hour

// recentlyCheckedUnreachable reports whether an IDS lookup for this portal came
// back empty inside the TTL, in which case asking Apple again buys nothing.
func (c *IMClient) recentlyCheckedUnreachable(portalID string) bool {
	c.carrierCheckedMu.Lock()
	defer c.carrierCheckedMu.Unlock()
	at, ok := c.carrierCheckedAt[portalID]
	return ok && time.Since(at) < carrierCheckTTL
}

// noteCheckedUnreachable records an empty lookup, pruning expired entries so the
// map cannot grow without bound. Mirrors the pruning in trackUnsend.
func (c *IMClient) noteCheckedUnreachable(portalID string) {
	c.carrierCheckedMu.Lock()
	defer c.carrierCheckedMu.Unlock()
	if c.carrierCheckedAt == nil {
		c.carrierCheckedAt = make(map[string]time.Time)
	}
	c.carrierCheckedAt[portalID] = time.Now()
	for k, at := range c.carrierCheckedAt {
		if time.Since(at) >= carrierCheckTTL {
			delete(c.carrierCheckedAt, k)
		}
	}
}

// recentlyVerifiedIMessage reports whether a lookup recently proved this portal
// reachable on iMessage.
func (c *IMClient) recentlyVerifiedIMessage(portalID string) bool {
	c.carrierCheckedMu.Lock()
	defer c.carrierCheckedMu.Unlock()
	at, ok := c.carrierVerifiedAt[portalID]
	return ok && time.Since(at) < carrierCheckTTL
}

// noteVerifiedIMessage records a non-empty lookup so repeat sends to the same
// portal inside the hour reuse the answer instead of re-querying Apple. Nothing
// outside this file reads it — the routing decision it feeds is applied to one
// send and never persisted.
func (c *IMClient) noteVerifiedIMessage(portalID string) {
	c.carrierCheckedMu.Lock()
	defer c.carrierCheckedMu.Unlock()
	if c.carrierVerifiedAt == nil {
		c.carrierVerifiedAt = make(map[string]time.Time)
	}
	c.carrierVerifiedAt[portalID] = time.Now()
	for k, at := range c.carrierVerifiedAt {
		if time.Since(at) >= carrierCheckTTL {
			delete(c.carrierVerifiedAt, k)
		}
	}
}

// carrierCheckMaxPerHour is an absolute ceiling on carrier-route lookups across
// every portal, independent of the per-portal TTLs and the rate gate. Those bound
// the steady state, but a bridge with hundreds of carrier-flagged portals could
// still walk a long tail of first-time checks; this caps the total an account can
// generate in any rolling hour.
//
// Well above real use: reaching it means sending to 60 distinct never-checked
// carrier contacts within an hour.
const carrierCheckMaxPerHour = 60

// reserveCarrierCheck books one slot against the hourly ceiling, reporting
// whether the lookup may proceed.
func (c *IMClient) reserveCarrierCheck() bool {
	c.carrierCheckedMu.Lock()
	defer c.carrierCheckedMu.Unlock()

	cutoff := time.Now().Add(-time.Hour)
	kept := c.carrierCheckLog[:0]
	for _, at := range c.carrierCheckLog {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	c.carrierCheckLog = kept

	if len(c.carrierCheckLog) >= carrierCheckMaxPerHour {
		return false
	}
	c.carrierCheckLog = append(c.carrierCheckLog, time.Now())
	return true
}

// hasSMSRelay reports whether any device on this account other than us is
// registered to forward text messages.
//
// A carrier-routed send is addressed to our own handle on
// com.apple.private.alloy.sms for such a device to transmit; with none present
// Apple accepts the push and the message is silently discarded. The Rust side
// answers this by mirroring exactly how the send resolves its targets, rather
// than inferring it — inference cannot work, because the strongest positive
// signal is Apple's delivery confirmation, which only arrives in response to a
// relayed send.
//
// Memoized for carrierCheckTTL because relay presence is a property of the
// ACCOUNT, not of a portal. Without this the lookup sits outside both
// reserveCarrierCheck's hourly ceiling and sendPathIDSGate, so if Apple returns
// nothing to cache on the Rust side every carrier send re-queries, unpaced and
// uncapped — on a bridge with hundreds of carrier portals that is a rate-limit
// exposure. One memo collapses it to at most one lookup an hour.
//
// Errs toward true on every failure, including an FFI panic: a wrong true only
// preserves the pre-existing behaviour, while a wrong false refuses a message
// that would have been delivered.
func (c *IMClient) hasSMSRelay() (has bool) {
	if c.client == nil {
		return true
	}
	handle := c.getHandle()

	c.carrierCheckedMu.Lock()
	if !c.smsRelayCheckedAt.IsZero() && time.Since(c.smsRelayCheckedAt) < carrierCheckTTL && c.smsRelayHandle == handle {
		cached := c.smsRelayValue
		c.carrierCheckedMu.Unlock()
		return cached
	}
	c.carrierCheckedMu.Unlock()

	// Recovered for the same reason validateTargetsSafe is (contact_merge.go:75):
	// this crosses into the identity-manager FFI path, which has panic sites
	// outside the guarded seam — get_keys reaches is_valid, which can panic on a
	// clock step. HasSmsRelay returns a bare bool, so a Rust panic surfaces here
	// as a Go panic rather than an error.
	defer func() {
		if r := recover(); r != nil {
			c.UserLogin.Log.Error().Interface("panic", r).
				Msg("HasSmsRelay panicked in FFI path; assuming a relay exists")
			has = true
		}
	}()

	has = c.client.HasSmsRelay(handle)

	c.carrierCheckedMu.Lock()
	c.smsRelayCheckedAt = time.Now()
	c.smsRelayValue = has
	c.smsRelayHandle = handle
	c.carrierCheckedMu.Unlock()
	return has
}
