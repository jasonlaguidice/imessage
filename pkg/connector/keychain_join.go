// matrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// cmdKeychainJoin joins the iCloud Keychain trust circle for an existing,
// running login. The login flow is otherwise the only way to join, and logging
// in again means a new device registration with Apple; this reuses the login's
// current session instead. It asks which device's escrow bottle to use and
// that device's passcode, deleting the passcode message as soon as it arrives.
var cmdKeychainJoin = &commands.FullHandler{
	Name:          "keychain-join",
	Func:          fnKeychainJoin,
	RequiresLogin: true,
	Help: commands.HelpMeta{
		Section: commands.HelpSectionAdmin,
		Description: "Join iCloud Keychain for this login without logging in again " +
			"(needed for CloudKit backfill). Asks for a device and its passcode; " +
			"the passcode message is deleted. Use in the management room.",
		Args: "[force]",
	},
}

// keychainJoinState carries the device list between the command's prompts.
type keychainJoinState struct {
	client  *IMClient
	tp      *rustpushgo.WrappedTokenProvider
	devices []rustpushgo.EscrowDeviceInfo
	// selected indexes devices; -1 lets the join try every bottle in order.
	selected int
}

func fnKeychainJoin(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("Not logged in.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}
	if client.tokenProvider == nil || *client.tokenProvider == nil {
		ce.Reply("No token provider for this login.")
		return
	}
	if meta, ok := client.UserLogin.Metadata.(*UserLoginMetadata); ok &&
		hasKeychainCliqueState(client.UserLogin.Log, meta.AccountDSID) &&
		strings.TrimSpace(ce.RawArgs) != "force" {
		ce.Reply("This login has already joined iCloud Keychain. Run `$cmdprefix keychain-join force` to join again.")
		return
	}
	if !client.useCloudKitBackfill() {
		ce.Reply("Note: CloudKit backfill is off for this login, so nothing will use the keychain until it is enabled.")
	}

	state := &keychainJoinState{client: client, tp: *client.tokenProvider, selected: -1}
	ce.Reply("Fetching devices with an escrow bottle…")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ce.Reply("Keychain join failed: %v", r)
			}
		}()
		devices, err := state.tp.GetEscrowDevices()
		if err != nil {
			ce.Reply("Keychain join failed: %v", err)
			return
		}
		state.devices = devices
		switch len(devices) {
		case 0:
			ce.Reply("Apple has no escrow bottle this bridge can use, so the keychain can't be joined.")
		case 1:
			state.selected = 0
			state.promptPasscode(ce)
		default:
			state.promptDevice(ce)
		}
	}()
}

func (s *keychainJoinState) promptDevice(ce *commands.Event) {
	var sb strings.Builder
	sb.WriteString("Which device's passcode will you enter? Reply with the number, or `cancel`.\n\n")
	for i, d := range s.devices {
		sb.WriteString(strconv.Itoa(i+1) + ". " + formatDeviceLabel(d) + "\n")
	}
	ce.Reply(sb.String())
	commands.StoreCommandState(ce.User, &commands.CommandState{
		Next:   commands.MinimalCommandHandlerFunc(s.submitDevice),
		Action: "Keychain join",
	})
}

func (s *keychainJoinState) submitDevice(ce *commands.Event) {
	n, err := strconv.Atoi(strings.TrimSpace(ce.RawArgs))
	if err != nil || n < 1 || n > len(s.devices) {
		ce.Reply("Reply with a number from 1 to %d, or `cancel`.", len(s.devices))
		return
	}
	s.selected = n - 1
	s.promptPasscode(ce)
}

func (s *keychainJoinState) promptPasscode(ce *commands.Event) {
	ce.Reply("Enter the passcode of %s (the code that unlocks it). The message will be deleted. Reply `cancel` to stop.",
		formatDeviceLabel(s.devices[s.selected]))
	commands.StoreCommandState(ce.User, &commands.CommandState{
		Next:   commands.MinimalCommandHandlerFunc(s.submitPasscode),
		Action: "Keychain join",
	})
}

func (s *keychainJoinState) submitPasscode(ce *commands.Event) {
	ce.Redact()
	commands.StoreCommandState(ce.User, nil)
	passcode := strings.TrimSpace(ce.RawArgs)
	if passcode == "" {
		ce.Reply("Empty passcode; run the command again.")
		return
	}
	ce.Reply("Joining iCloud Keychain…")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ce.Reply("Keychain join failed: %v", r)
			}
		}()
		log := s.client.UserLogin.Log.With().Str("component", "keychain_join").Logger()
		if err := joinKeychainWithPasscode(log, &s.tp, passcode, s.devices, s.selected); err != nil {
			ce.Reply("Keychain join failed: %v", err)
			return
		}
		ce.Reply("Joined iCloud Keychain. Restart the bridge to start CloudKit backfill.")
	}()
}
