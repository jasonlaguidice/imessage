// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/status"
)

// Per-user setting commands that complement the login flow: every setting the
// login asks for can be changed later without logging in again. This file
// holds set-facetime-notices, set-statuskit, set-statuskit-style and
// set-cloudkit-backfill; set-video-transcoding and set-heic-conversion live in
// commands.go next to the shared setBoolSetting helper.
//
// How each change takes effect at runtime:
//   - FaceTime notices, StatusKit notifications and StatusKit style are read
//     on every use (per call / per presence update / per pull tick), so
//     saving the metadata is enough.
//   - CloudKit backfill is decided once in Connect (cloud sync controller
//     start, APNs portal-creation gate), so a change that flips the effective
//     value reconnects just this login (see reconnectLogin).

// ============================================================================
// Argument parsing (pure helpers, unit-tested)
// ============================================================================

// toggleArg is a parsed on/off/reset argument of a set-* command.
type toggleArg int

const (
	toggleInvalid toggleArg = iota
	toggleQuery             // no argument: report the current value
	toggleOn
	toggleOff
	toggleReset // clear the per-user override, use the global config default
)

// parseToggleArg parses the argument of an on/off/reset setting command.
// Matching is case-insensitive and ignores surrounding whitespace.
func parseToggleArg(raw string) toggleArg {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return toggleQuery
	case "on", "yes", "enable", "true", "1":
		return toggleOn
	case "off", "no", "disable", "false", "0":
		return toggleOff
	case "reset", "default", "unset":
		return toggleReset
	default:
		return toggleInvalid
	}
}

// override returns the per-user override a set-able toggleArg stores: true
// for on, false for off, nil for reset. Only valid for toggleOn, toggleOff
// and toggleReset.
func (a toggleArg) override() *bool {
	switch a {
	case toggleOn:
		return boolPtr(true)
	case toggleOff:
		return boolPtr(false)
	default:
		return nil
	}
}

// invertBoolPtr returns nil for nil, otherwise a pointer to !*v.
func invertBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	return boolPtr(!*v)
}

// The FaceTime setting is stored inverted: UserLoginMetadata.DisableFaceTime
// (true = notices suppressed), while the command speaks in terms of notices
// (on = notices posted). These two functions are the only place the mapping
// lives; nil (no override, use config) maps to nil both ways.
//
//	set-facetime-notices on    -> DisableFaceTime = false
//	set-facetime-notices off   -> DisableFaceTime = true
//	set-facetime-notices reset -> DisableFaceTime = nil
func faceTimeNoticesToDisableFaceTime(notices *bool) *bool { return invertBoolPtr(notices) }
func disableFaceTimeToFaceTimeNotices(disable *bool) *bool { return invertBoolPtr(disable) }

// styleArg is a parsed set-statuskit-style argument.
type styleArg struct {
	query bool
	reset bool
	// style is the normalized value ("topic" or "notice") when neither query
	// nor reset is set.
	style string
}

// parseStatusKitStyleArg parses a set-statuskit-style argument. Style values
// go through normalizeStatusKitNotificationStyle, the same normalization the
// config and statusKitNotificationStyle() use. ok is false for anything else.
func parseStatusKitStyleArg(raw string) (arg styleArg, ok bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	switch trimmed {
	case "":
		return styleArg{query: true}, true
	case "reset", "default", "unset":
		return styleArg{reset: true}, true
	}
	style, valid := normalizeStatusKitNotificationStyle(trimmed)
	if !valid {
		return styleArg{}, false
	}
	return styleArg{style: style}, true
}

// cloudKitBackfillVerdict is the outcome of planning a set-cloudkit-backfill
// change.
type cloudKitBackfillVerdict int

const (
	cloudKitBackfillAllowed cloudKitBackfillVerdict = iota
	// cloudKitBackfillRefuseGlobalOff: "on" requested but CloudKit backfill is
	// off bridge-wide, so a per-user value can't turn it on.
	cloudKitBackfillRefuseGlobalOff
	// cloudKitBackfillRefuseNoKeychain: the change would turn backfill on but
	// the login hasn't joined iCloud Keychain, so CloudKit sync can't decrypt.
	cloudKitBackfillRefuseNoKeychain
)

// cloudKitBackfillPlan is the result of planCloudKitBackfillChange.
type cloudKitBackfillPlan struct {
	verdict cloudKitBackfillVerdict
	// before / after are the effective values (what useCloudKitBackfill
	// returns) with the current and the requested override.
	before, after bool
}

// reconnect reports whether an allowed change must rebuild the login's client
// to take effect: the cloud sync controller and the APNs portal gate are set
// up once in Connect, so only a change of the effective value needs it.
func (p cloudKitBackfillPlan) reconnect() bool {
	return p.verdict == cloudKitBackfillAllowed && p.before != p.after
}

// effectiveCloudKitBackfill is the rule behind IMClient.useCloudKitBackfill:
// the global switch must be on (and sourced from CloudKit), and then the
// per-user override can only turn it off. nil means on.
func effectiveCloudKitBackfill(globalOn bool, override *bool) bool {
	if !globalOn {
		return false
	}
	return override == nil || *override
}

// planCloudKitBackfillChange decides whether moving the per-user override from
// current to requested is allowed. keychainJoined is only consulted when the
// change would newly turn backfill on.
func planCloudKitBackfillChange(globalOn bool, current, requested *bool, keychainJoined func() bool) cloudKitBackfillPlan {
	plan := cloudKitBackfillPlan{
		before: effectiveCloudKitBackfill(globalOn, current),
		after:  effectiveCloudKitBackfill(globalOn, requested),
	}
	switch {
	case requested != nil && *requested && !globalOn:
		plan.verdict = cloudKitBackfillRefuseGlobalOff
	case plan.after && !plan.before && !keychainJoined():
		plan.verdict = cloudKitBackfillRefuseNoKeychain
	default:
		plan.verdict = cloudKitBackfillAllowed
	}
	return plan
}

// ============================================================================
// set-facetime-notices / set-statuskit / set-statuskit-style
// ============================================================================

var cmdSetFaceTimeNotices = &commands.FullHandler{
	Name:    "set-facetime-notices",
	Aliases: []string{"facetime-notices"},
	Func:    fnSetFaceTimeNotices,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Enable or disable notices for incoming and missed FaceTime calls. Usage: `$cmdprefix set-facetime-notices on|off|reset`",
	},
	RequiresLogin: true,
}

var cmdSetStatusKit = &commands.FullHandler{
	Name:    "set-statuskit",
	Aliases: []string{"statuskit-notifications"},
	Func:    fnSetStatusKit,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Enable or disable StatusKit notifications (a contact's Focus / Do Not Disturb state). Usage: `$cmdprefix set-statuskit on|off|reset`",
	},
	RequiresLogin: true,
}

var cmdSetStatusKitStyle = &commands.FullHandler{
	Name:    "set-statuskit-style",
	Aliases: []string{"statuskit-style"},
	Func:    fnSetStatusKitStyle,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Choose how StatusKit notifications appear in DMs: `topic` sets the room topic, `notice` posts a message (group chats always get a notice). Usage: `$cmdprefix set-statuskit-style topic|notice|reset`",
	},
	RequiresLogin: true,
}

// fnSetFaceTimeNotices toggles DisableFaceTime through the inverted mapping
// above. postFaceTimeNotice checks disableFaceTime() on every call, so the
// saved value applies to the next call without a reconnect.
func fnSetFaceTimeNotices(ce *commands.Event) {
	setBoolSetting(ce, "FaceTime call notice posting",
		func(meta *UserLoginMetadata, val *bool) {
			meta.DisableFaceTime = faceTimeNoticesToDisableFaceTime(val)
		},
		func(meta *UserLoginMetadata) *bool {
			return disableFaceTimeToFaceTimeNotices(meta.DisableFaceTime)
		},
	)
}

// fnSetStatusKit toggles StatusKitNotifications. OnStatusUpdate,
// OnStatusDecryptFailed and the StatusKit pull loop check
// statusKitNotifications() on every event / tick, and StatusKit itself is
// initialized and subscribed regardless of this setting, so saving is enough.
func fnSetStatusKit(ce *commands.Event) {
	setBoolSetting(ce, "StatusKit notification posting",
		func(meta *UserLoginMetadata, val *bool) { meta.StatusKitNotifications = val },
		func(meta *UserLoginMetadata) *bool { return meta.StatusKitNotifications },
	)
}

// fnSetStatusKitStyle sets StatusKitNotificationStyle. OnStatusUpdate reads
// statusKitNotificationStyle() per update, so saving is enough.
func fnSetStatusKitStyle(ce *commands.Event) {
	login := commandLogin(ce)
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok || meta == nil {
		ce.Reply("Login metadata unavailable.")
		return
	}

	arg, ok := parseStatusKitStyleArg(ce.RawArgs)
	if !ok {
		ce.Reply("Unknown value %q — use `topic`, `notice`, or `reset` (to use global config default).", strings.TrimSpace(ce.RawArgs))
		return
	}
	if arg.query {
		if meta.StatusKitNotificationStyle == nil || strings.TrimSpace(*meta.StatusKitNotificationStyle) == "" {
			ce.Reply("StatusKit notification style is using the global config default. Use `$cmdprefix %s topic` or `notice` to override.", ce.Command)
		} else {
			style, _ := normalizeStatusKitNotificationStyle(*meta.StatusKitNotificationStyle)
			ce.Reply("StatusKit notification style is **%s** (per-user override). Use `$cmdprefix %s topic|notice|reset` to change it.", style, ce.Command)
		}
		return
	}

	if arg.reset {
		meta.StatusKitNotificationStyle = nil
	} else {
		style := arg.style
		meta.StatusKitNotificationStyle = &style
	}
	if err := login.Save(ce.Ctx); err != nil {
		ce.Reply("Failed to save settings: %v", err)
		return
	}

	if arg.reset {
		ce.Reply("StatusKit notification style reset to global config default.")
	} else {
		ce.Reply("StatusKit notification style set to **%s**.", arg.style)
	}
	if client, ok := login.Client.(*IMClient); ok && client != nil && !client.statusKitNotifications() {
		ce.Reply("Note: StatusKit notifications are off for this login, so the style has no effect until you run `$cmdprefix set-statuskit on`.")
	}
}

// ============================================================================
// set-cloudkit-backfill
// ============================================================================

var cmdSetCloudKitBackfill = &commands.FullHandler{
	Name:    "set-cloudkit-backfill",
	Aliases: []string{"cloudkit-backfill"},
	Func:    fnSetCloudKitBackfill,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Enable or disable CloudKit message history backfill for this login (only possible when the bridge admin has enabled it; enabling needs `keychain-join` first). Reconnects the login to apply. Usage: `$cmdprefix set-cloudkit-backfill on|off|reset`",
	},
	RequiresLogin: true,
}

// fnSetCloudKitBackfill sets the per-user CloudKit backfill override.
//
// The global network.cloudkit_backfill (with backfill_source: cloudkit) must
// be on; the per-user value can only turn it off, so "on" is refused when the
// global is off. Turning it on also needs the login's iCloud Keychain to be
// joined: without it the CloudKit bootstrap can't decrypt and retries every
// minute with the APNs portal-creation gate closed, so new chats would never
// get portals. That change is refused (nothing saved) with a pointer to
// keychain-join, which works whether or not backfill is on.
//
// The cloud sync controller is started, and the portal gate opened, once in
// Connect, so a change of the effective value reconnects just this login.
func fnSetCloudKitBackfill(ce *commands.Event) {
	login := commandLogin(ce)
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	meta, ok := login.Metadata.(*UserLoginMetadata)
	if !ok || meta == nil {
		ce.Reply("Login metadata unavailable.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}
	globalOn := client.Main.Config.UseCloudKitBackfill()

	arg := parseToggleArg(ce.RawArgs)
	switch arg {
	case toggleInvalid:
		ce.Reply("Unknown value %q — use `on`, `off`, or `reset` (to use global config default).", strings.TrimSpace(ce.RawArgs))
		return
	case toggleQuery:
		state := "off"
		if client.useCloudKitBackfill() {
			state = "on"
		}
		switch {
		case !globalOn:
			ce.Reply("CloudKit backfill is **off**: it is disabled bridge-wide, so it can't be turned on per login.")
		case meta.CloudKitBackfill == nil:
			ce.Reply("CloudKit backfill is **%s** (global config default). Use `$cmdprefix %s on` or `off` to override.", state, ce.Command)
		default:
			ce.Reply("CloudKit backfill is **%s** (per-user override). Use `$cmdprefix %s on|off|reset` to change it.", state, ce.Command)
		}
		return
	}

	requested := arg.override()
	plan := planCloudKitBackfillChange(globalOn, meta.CloudKitBackfill, requested, func() bool {
		return hasKeychainCliqueState(login.Log, meta.AccountDSID)
	})
	switch plan.verdict {
	case cloudKitBackfillRefuseGlobalOff:
		ce.Reply("CloudKit backfill is disabled bridge-wide (`cloudkit_backfill` is off or `backfill_source` isn't `cloudkit` in the bridge config), so it can't be turned on for one login. Ask the bridge admin to enable it.")
		return
	case cloudKitBackfillRefuseNoKeychain:
		ce.Reply("CloudKit backfill needs this login to join iCloud Keychain first. Run `$cmdprefix keychain-join`, then run `$cmdprefix %s %s` again. Nothing was changed.", ce.Command, strings.ToLower(strings.TrimSpace(ce.RawArgs)))
		return
	}

	meta.CloudKitBackfill = requested
	if err := login.Save(ce.Ctx); err != nil {
		ce.Reply("Failed to save settings: %v", err)
		return
	}

	state := "disabled"
	if plan.after {
		state = "enabled"
	}
	if requested == nil {
		ce.Reply("CloudKit backfill reset to global config default (%s).", state)
	} else {
		ce.Reply("CloudKit backfill %s.", state)
	}
	if !plan.reconnect() {
		return
	}
	ce.Reply("Reconnecting this login to apply the change…")
	if err := reconnectLogin(login, func(err error) {
		if err != nil {
			ce.Reply("Reconnect failed: %v. The setting is saved and applies on the next reconnect or bridge restart.", err)
			return
		}
		if plan.after {
			ce.Reply("Reconnected. CloudKit backfill is starting; new chats get portals once the first sync finishes.")
		} else {
			ce.Reply("Reconnected without CloudKit backfill.")
		}
	}); err != nil {
		ce.Reply("Couldn't reconnect: %v. The setting is saved and applies on the next reconnect or bridge restart.", err)
	}
}

// ============================================================================
// Login reconnect
// ============================================================================

// reconnectsInFlight holds the IDs of logins reconnectLogin is rebuilding, so
// two commands can't rebuild the same login at once.
var reconnectsInFlight sync.Map // map[networkid.UserLoginID]struct{}

var errReconnectInFlight = errors.New("a reconnect of this login is already running")

// reconnectLogin rebuilds one login's network client in the background, the
// same way bridgev2 does on an unknown-error reconnect or a network reset
// (Bridge.ResetNetworkConnections): disconnect the client, LoadUserLogin
// (which builds a fresh IMClient from the saved metadata) and Connect it.
// Other logins are untouched. If the rebuild fails, an unknown-error bridge
// state is sent so bridgev2's own auto-reconnect can retry, as
// ResetNetworkConnections does. done (may be nil) runs after Connect returns
// or the rebuild fails. The error return is only for a reconnect that
// couldn't start.
func reconnectLogin(login *bridgev2.UserLogin, done func(error)) error {
	if _, busy := reconnectsInFlight.LoadOrStore(login.ID, struct{}{}); busy {
		return errReconnectInFlight
	}
	go func() {
		defer reconnectsInFlight.Delete(login.ID)
		log := login.Log.With().Str("action", "settings reconnect").Logger()
		ctx := log.WithContext(login.Bridge.BackgroundCtx)
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Msg("Settings reconnect panicked")
					err = fmt.Errorf("reconnect panicked: %v", r)
				}
			}()
			log.Info().Msg("Reconnecting login to apply a settings change")
			if login.Client != nil {
				login.Client.Disconnect()
			}
			if err := login.Bridge.Network.LoadUserLogin(ctx, login); err != nil {
				log.Err(err).Msg("Failed to rebuild client after settings change")
				login.BridgeState.Send(status.BridgeState{
					StateEvent: status.StateUnknownError,
					Error:      "im-settings-reconnect-fail",
					Info:       map[string]any{"go_error": err.Error()},
				})
				return err
			}
			login.Client.Connect(ctx)
			log.Info().Msg("Settings reconnect finished")
			return nil
		}()
		if done != nil {
			done(err)
		}
	}()
	return nil
}
