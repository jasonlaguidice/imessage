// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"fmt"
	"math"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
)

// mautrix gives a network connector no way to change the defaults of the
// bridge.* and backfill.* keys: both `-e` and the config upgrader copy the
// values from mautrix's bridgev2/matrix/mxmain/example-config.yaml into every
// config.yaml. At runtime an unset key is therefore indistinguishable from
// mautrix's stock value. This bridge treats a key that still holds the stock
// value as "not chosen" and applies its own default in memory (config.yaml is
// never rewritten). Any other value is the operator's choice and is kept.
//
// The stock values below are mautrix v0.31.0's; TestMautrixStockValuesMatch
// checks them against the example-config.yaml in the module cache.
const (
	mautrixStockPhoneNumbersInProfile                       = false
	mautrixStockUnknownErrorAutoReconnect     time.Duration = 0 // null in example-config.yaml
	mautrixStockUnknownErrorMaxAutoReconnects               = 10
	mautrixStockBackfillEnabled                             = false
	mautrixStockMaxInitialMessages                          = 50
	mautrixStockMaxCatchupMessages                          = 500
	mautrixStockBackfillQueueEnabled                        = false
	mautrixStockBackfillQueueBatchSize                      = 100
)

// uncappedBackfillMessages is the "uncapped" sentinel for
// backfill.max_initial_messages and max_catchup_messages. FetchMessages and
// preUploadCloudAttachments treat any smaller max_initial_messages as an
// explicit operator cap: backward backfill is skipped and attachments are not
// pre-uploaded at startup.
const uncappedBackfillMessages = math.MaxInt32

// minUnknownErrorAutoReconnect is the smallest bridge.unknown_error_auto_reconnect
// bridgev2 honours; smaller values disable the auto-reconnect
// (bridgestate.go waitForUnknownErrorReconnect).
const minUnknownErrorAutoReconnect = time.Minute

// cannotDisableNote is appended to the reason of a boolean default whose
// stock value is false: an explicit false is indistinguishable from the stock
// value, so the operator cannot turn the setting off from config.yaml.
const cannotDisableNote = " (false is mautrix's stock value, so this cannot be turned off in config.yaml)"

// applyStockDefault returns def when current still equals mautrix's stock
// value, and current (the operator's choice) otherwise.
func applyStockDefault[T comparable](current, stock, def T) (value T, changed bool) {
	if current == stock {
		return def, current != def
	}
	return current, false
}

// appliedDefault describes one bridge default that replaced a stock value.
type appliedDefault struct {
	Key     string
	Stock   string
	Default string
	Reason  string
}

// configDefault is one bridge default for a mautrix config key.
type configDefault interface {
	// apply sets the bridge default in cfg when the key holds mautrix's stock
	// value, and reports whether it did.
	apply(cfg *bridgeconfig.BridgeConfig) (appliedDefault, bool)
}

// stockDefault is a configDefault for one field of type T.
type stockDefault[T comparable] struct {
	key    string                              // YAML path, for logging
	field  func(*bridgeconfig.BridgeConfig) *T // the field in the live config
	stock  T                                   // mautrix's stock value
	def    T                                   // this bridge's default
	reason string
}

func (d stockDefault[T]) apply(cfg *bridgeconfig.BridgeConfig) (appliedDefault, bool) {
	ptr := d.field(cfg)
	value, changed := applyStockDefault(*ptr, d.stock, d.def)
	if !changed {
		return appliedDefault{}, false
	}
	*ptr = value
	return appliedDefault{
		Key:     d.key,
		Stock:   formatConfigValue(d.stock),
		Default: formatConfigValue(d.def),
		Reason:  d.reason,
	}, true
}

// formatConfigValue renders a value the way it would be written in
// config.yaml (a zero duration is mautrix's null).
func formatConfigValue(v any) string {
	if d, ok := v.(time.Duration); ok {
		if d == 0 {
			return "null"
		}
		return d.String()
	}
	return fmt.Sprint(v)
}

// bridgeConfigDefaults lists this bridge's defaults for mautrix's bridge.* and
// backfill.* keys. They apply whatever network.cloudkit_backfill says: without
// a history source FetchMessages returns no messages, so the backfill values
// are harmless.
//
// backfill.queue.max_batches is not handled: IMClient.GetBackfillMaxBatchCount
// always returns -1, which bridgev2 uses in place of the config value, and
// FetchMessages short-circuits backward requests when max_initial_messages is
// capped.
var bridgeConfigDefaults = []configDefault{
	stockDefault[bool]{
		key:   "bridge.phone_numbers_in_profile",
		field: func(c *bridgeconfig.BridgeConfig) *bool { return &c.PhoneNumbersInProfile },
		stock: mautrixStockPhoneNumbersInProfile,
		def:   true,
		// bridgev2 strips every tel: identifier from ghost profiles unless
		// this is set (ghost.go). Matrix clients (Beeper) gate the call button
		// and phone-contact handling on that identifier.
		reason: "keeps tel: identifiers in ghost profiles so phone-only contacts get a call button and phone contact info" + cannotDisableNote,
	},
	stockDefault[time.Duration]{
		key:    "bridge.unknown_error_auto_reconnect",
		field:  func(c *bridgeconfig.BridgeConfig) *time.Duration { return &c.UnknownErrorAutoReconnect },
		stock:  mautrixStockUnknownErrorAutoReconnect,
		def:    5 * time.Minute,
		reason: "lets bridgev2 rebuild a client the APNs receive-wedge watchdog reports as stuck (unset disables the auto-reconnect)",
	},
	stockDefault[int]{
		key:   "bridge.unknown_error_max_auto_reconnects",
		field: func(c *bridgeconfig.BridgeConfig) *int { return &c.UnknownErrorMaxAutoReconnects },
		stock: mautrixStockUnknownErrorMaxAutoReconnects,
		def:   100,
		// Rebuilds are rate-limited (10-min wedge threshold + 5-min reconnect
		// delay) and kept light by the full-connect cooldown, so 100 stays
		// Apple-safe.
		reason: "bridgev2 never resets this counter until restart, so a low budget leaves wedge recovery manual-restart-only on a long-running bridge",
	},
	stockDefault[bool]{
		key:   "backfill.enabled",
		field: func(c *bridgeconfig.BridgeConfig) *bool { return &c.Backfill.Enabled },
		stock: mautrixStockBackfillEnabled,
		def:   true,
		// bridgev2 only calls FetchMessages and accepts RemoteBackfill events
		// when this is true. Without it CloudKit backfill delivers no history,
		// and the APNs buffer waiting for the initial forward backfills
		// (pendingInitialBackfills) holds live messages until its 5-minute
		// no-progress timeout.
		reason: "CloudKit backfill cannot deliver history without it" + cannotDisableNote,
	},
	stockDefault[int]{
		key:   "backfill.max_initial_messages",
		field: func(c *bridgeconfig.BridgeConfig) *int { return &c.Backfill.MaxInitialMessages },
		stock: mautrixStockMaxInitialMessages,
		def:   uncappedBackfillMessages,
		// An operator value below uncappedBackfillMessages is an explicit cap:
		// FetchMessages skips backward backfill and preUploadCloudAttachments
		// skips the startup attachment pre-upload. Values <= 0 make bridgev2
		// skip forward backfill entirely.
		reason: "backfills each chat's full history; any smaller value is treated as a per-chat cap and turns off backward backfill",
	},
	stockDefault[int]{
		key:    "backfill.max_catchup_messages",
		field:  func(c *bridgeconfig.BridgeConfig) *int { return &c.Backfill.MaxCatchupMessages },
		stock:  mautrixStockMaxCatchupMessages,
		def:    uncappedBackfillMessages,
		reason: "catches up on every message missed while the bridge was down",
	},
	stockDefault[bool]{
		key:    "backfill.queue.enabled",
		field:  func(c *bridgeconfig.BridgeConfig) *bool { return &c.Backfill.Queue.Enabled },
		stock:  mautrixStockBackfillQueueEnabled,
		def:    true,
		reason: "bridgev2's backfill queue backfills history older than the first forward batch" + cannotDisableNote,
	},
	stockDefault[int]{
		key:    "backfill.queue.batch_size",
		field:  func(c *bridgeconfig.BridgeConfig) *int { return &c.Backfill.Queue.BatchSize },
		stock:  mautrixStockBackfillQueueBatchSize,
		def:    10000,
		reason: "many small backward batches create fragmented room history that clients cannot paginate through",
	},
}

// applyConfigDefaults applies every default in defaults to cfg and returns the
// ones it applied, in order.
func applyConfigDefaults(cfg *bridgeconfig.BridgeConfig, defaults []configDefault) []appliedDefault {
	var applied []appliedDefault
	for _, d := range defaults {
		if a, ok := d.apply(cfg); ok {
			applied = append(applied, a)
		}
	}
	return applied
}

// applyBridgeConfigDefaults applies bridgeConfigDefaults to the live bridge
// config (in memory only) and logs one info line per applied default.
func (c *IMConnector) applyBridgeConfigDefaults() {
	logAppliedDefaults(c.Bridge.Log, applyConfigDefaults(c.Bridge.Config, bridgeConfigDefaults))
}

func logAppliedDefaults(log zerolog.Logger, applied []appliedDefault) {
	for _, a := range applied {
		log.Info().
			Str("config_key", a.Key).
			Str("mautrix_default", a.Stock).
			Str("bridge_default", a.Default).
			Msgf("%s is at mautrix's default (%s); using this bridge's default %s: %s", a.Key, a.Stock, a.Default, a.Reason)
	}
}

// warnShortUnknownErrorAutoReconnect warns when the operator set
// bridge.unknown_error_auto_reconnect to a positive value below the minimum
// bridgev2 honours, which silently disables the auto-reconnect.
func (c *IMConnector) warnShortUnknownErrorAutoReconnect() {
	if d := c.Bridge.Config.UnknownErrorAutoReconnect; unknownErrorAutoReconnectTooShort(d) {
		c.Bridge.Log.Warn().
			Str("config_key", "bridge.unknown_error_auto_reconnect").
			Stringer("value", d).
			Msgf("bridge.unknown_error_auto_reconnect is %s; bridgev2 treats values under %s as disabled, so the APNs receive-wedge watchdog cannot rebuild a stuck client", d, minUnknownErrorAutoReconnect)
	}
}

func unknownErrorAutoReconnectTooShort(d time.Duration) bool {
	return d > 0 && d < minUnknownErrorAutoReconnect
}
