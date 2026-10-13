// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
// Copyright (C) 2026 Jason LaGuidice
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/connector/upgrades"
	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

func isRunningOnMacOS() bool {
	return runtime.GOOS == "darwin"
}

type IMConnector struct {
	Bridge *bridgev2.Bridge
	Config IMConfig
}

var _ bridgev2.NetworkConnector = (*IMConnector)(nil)

func (c *IMConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "iMessage",
		NetworkURL:           "https://support.apple.com/messages",
		NetworkIcon:          "mxc://shadowdrake.org/zUZxFBMoCbfmELVGwjeVyins",
		NetworkID:            "imessage",
		BeeperBridgeType:     "imessagego",
		DefaultPort:          29332,
		DefaultCommandPrefix: "!im",
	}
}

func (c *IMConnector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
	// First FFI call in the bridge process: route Rust logging into the
	// bridge logger before any InitLogger fallback can claim it.
	installRustLogSink(bridge.Log)
}

func (c *IMConnector) Start(ctx context.Context) error {
	// Create or upgrade the connector's own tables before any login loads.
	// bridgev2 has already upgraded its own schema by now.
	if err := upgradeConnectorDB(ctx, c.Bridge.DB.Database, c.Bridge.Log); err != nil {
		return bridgev2.DBUpgradeError{Err: err, Section: "imessage"}
	}

	// Warn once about an unrecognized global StatusKit notification style.
	// Logged here rather than in IMConfig.PostProcess because PostProcess
	// runs during YAML unmarshalling, before the bridge logger is available.
	if c.Config.invalidStatusKitNotificationStyle() {
		c.Bridge.Log.Warn().
			Str("statuskit_notification_style", c.Config.StatusKitNotificationStyle).
			Msg("Invalid statuskit_notification_style in config (expected \"topic\" or \"notice\"); falling back to \"topic\"")
	}

	// config.yaml is operator-owned. Start() never rewrites the operator's
	// bridge/backfill values for preference reasons; it only warns when a
	// value is below what this connector recommends. The single exception is
	// in enforceRequiredBackfillConfig, for a value CloudKit backfill cannot
	// function without.
	c.warnRecommendedBridgeConfig()
	if c.Config.CloudKitBackfill {
		c.enforceRequiredBackfillConfig()
		c.warnRecommendedBackfillConfig()
	}
	return nil
}

// recommendedMaxInitialMessages is the "uncapped" sentinel for
// backfill.max_initial_messages. FetchMessages and preUploadCloudAttachments
// treat any smaller value as an explicit operator cap.
const recommendedMaxInitialMessages = math.MaxInt32

// warnRecommended logs that an operator config value differs from what the
// connector recommends. It never changes the value.
func (c *IMConnector) warnRecommended(key string, current, recommended any, reason string) {
	c.Bridge.Log.Warn().
		Str("config_key", key).
		Interface("current", current).
		Interface("recommended", recommended).
		Msgf("Config %s is %v; recommended %v: %s (value left unchanged)", key, current, recommended, reason)
}

// warnRecommendedBridgeConfig checks the bridge.* keys the connector relies on
// for automatic recovery and contact display.
func (c *IMConnector) warnRecommendedBridgeConfig() {
	cfg := c.Bridge.Config
	// bridgev2 treats any value < 1min as "disabled" (bridgestate.go
	// waitForUnknownErrorReconnect), so the APNs receive-wedge watchdog's
	// StateUnknownError would not rebuild the client.
	if cfg.UnknownErrorAutoReconnect < time.Minute {
		c.warnRecommended("bridge.unknown_error_auto_reconnect", cfg.UnknownErrorAutoReconnect.String(), "5m",
			"values under 1m disable bridgev2's unknown-error auto-reconnect, so the APNs receive-wedge watchdog cannot rebuild the client")
	}
	// bridgev2 never resets the unknown-error reconnect counter in-process, so a
	// low budget is exhausted after a few rebuilds on a long-uptime deploy.
	// Rebuilds are rate-limited (10-min wedge threshold + 5-min reconnect delay)
	// and kept light by the full-connect cooldown, so 100 stays Apple-safe.
	if cfg.UnknownErrorMaxAutoReconnects < 100 {
		c.warnRecommended("bridge.unknown_error_max_auto_reconnects", cfg.UnknownErrorMaxAutoReconnects, 100,
			"bridgev2 never resets this counter until restart, so a low budget leaves wedge recovery manual-restart-only after a few rebuilds")
	}
	// bridgev2 strips every tel: identifier from ghost profiles unless this is
	// set (ghost.go prepareContactInfo). Matrix clients (Beeper) gate the call
	// button and phone-contact handling on that tel: identifier.
	if !cfg.PhoneNumbersInProfile {
		c.warnRecommended("bridge.phone_numbers_in_profile", false, true,
			"phone-only iMessage contacts lose their tel: identifier in Matrix profiles (no call button, email-only contact info)")
	}
}

// enforceRequiredBackfillConfig sets the one backfill value that CloudKit
// backfill cannot work without. bridgev2 only calls FetchMessages
// (portal.go doForwardBackfill on room creation and chat resync) and only
// accepts RemoteBackfill events when backfill.enabled is true. With it off,
// network.cloudkit_backfill would deliver no history at all, and the APNs
// buffer that waits for the initial forward backfills (pendingInitialBackfills)
// would hold live messages until its 5-minute no-progress timeout. The change
// is in-memory only; config.yaml is not rewritten.
func (c *IMConnector) enforceRequiredBackfillConfig() {
	cfg := &c.Bridge.Config.Backfill
	if !cfg.Enabled {
		cfg.Enabled = true
		c.Bridge.Log.Warn().
			Str("config_key", "backfill.enabled").
			Msg("network.cloudkit_backfill is true but backfill.enabled is false; enabling backfill in memory because CloudKit backfill cannot deliver history without it. Set backfill.enabled: true (or network.cloudkit_backfill: false) in config.yaml to silence this warning")
	}
}

// warnRecommendedBackfillConfig checks the backfill.* values recommended for
// CloudKit history. The mautrix defaults (max_initial_messages=50,
// batch_size=100) are low for chats with tens of thousands of messages, and
// many small backward batch_send requests create fragmented DAG branches
// that clients cannot paginate through.
func (c *IMConnector) warnRecommendedBackfillConfig() {
	cfg := c.Bridge.Config.Backfill
	if cfg.MaxInitialMessages < recommendedMaxInitialMessages {
		reason := "any smaller value is treated as a cap: only that many messages are backfilled per chat and backward backfill is skipped"
		if cfg.MaxInitialMessages <= 0 {
			reason = "values <= 0 skip forward backfill entirely, so no CloudKit history is delivered and live messages wait for the 5-minute APNs buffer timeout on initial sync"
		}
		c.warnRecommended("backfill.max_initial_messages", cfg.MaxInitialMessages, recommendedMaxInitialMessages, reason)
	}
	// Catch-up should match the initial cap: unlimited when uncapped, capped
	// when the operator caps max_initial_messages.
	if cfg.MaxCatchupMessages != cfg.MaxInitialMessages {
		c.warnRecommended("backfill.max_catchup_messages", cfg.MaxCatchupMessages, cfg.MaxInitialMessages,
			"catch-up after restart should use the same limit as backfill.max_initial_messages")
	}
	if !cfg.Queue.Enabled {
		c.warnRecommended("backfill.queue.enabled", false, true,
			"bridgev2's backfill queue (backward backfill of history older than the forward batch) does not run without it")
	}
	if cfg.Queue.BatchSize <= 100 {
		c.warnRecommended("backfill.queue.batch_size", cfg.Queue.BatchSize, 10000,
			"small backward batches create fragmented room DAG branches that clients cannot paginate through")
	}
	// backfill.queue.max_batches is not checked: IMClient.GetBackfillMaxBatchCount
	// always returns -1, which bridgev2 uses in place of the config value, and
	// FetchMessages short-circuits backward requests when max_initial_messages
	// is capped.
}

func (c *IMConnector) GetLoginFlows() []bridgev2.LoginFlow {
	flows := []bridgev2.LoginFlow{}
	if isRunningOnMacOS() {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        "Apple ID",
			Description: "Log in with your Apple ID to send and receive iMessages",
			ID:          LoginFlowIDAppleID,
		})
	}
	flows = append(flows, bridgev2.LoginFlow{
		Name:        "Apple ID (External Key)",
		Description: "Log in using a hardware key extracted from a Mac. Works on any platform.",
		ID:          LoginFlowIDExternalKey,
	})
	return flows
}

func (c *IMConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowIDAppleID:
		if !isRunningOnMacOS() {
			return nil, fmt.Errorf("Apple ID login requires macOS. Use 'External Key' login on other platforms.")
		}
		return &AppleIDLogin{User: user, Main: c}, nil
	case LoginFlowIDExternalKey:
		return &ExternalKeyLogin{User: user, Main: c}, nil
	default:
		return nil, fmt.Errorf("unknown login flow: %s", flowID)
	}
}

// upgradeConnectorDB creates or upgrades the tables the connector owns in the
// bridge database. They are versioned in their own table (imessage_version),
// next to bridgev2's, on a child of the bridge's database handle.
func upgradeConnectorDB(ctx context.Context, db *dbutil.Database, log zerolog.Logger) error {
	child := db.Child(upgrades.VersionTable, upgrades.Table,
		dbutil.ZeroLogger(log.With().Str("db_section", "imessage").Logger()))
	return child.Upgrade(ctx)
}

// LoadUserLogin builds the login's IMClient without touching the network:
// bridgev2 calls it under its global cache lock. The APNs connection is
// opened by IMClient.Connect, and the connector's tables are created once by
// the migrations in Start.
func (c *IMConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)

	rustpushgo.InitLogger()

	var cfg *rustpushgo.WrappedOsConfig
	var err error

	if meta.HardwareKey != "" {
		// Cross-platform mode: use hardware key with open-absinthe NAC emulation.
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateConfigFromHardwareKeyWithDeviceId(meta.HardwareKey, meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateConfigFromHardwareKey(meta.HardwareKey)
		}
	} else if isRunningOnMacOS() {
		// Local macOS mode: use IOKit + AAAbsintheContext.
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateLocalMacosConfigWithDeviceId(meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateLocalMacosConfig()
		}
	} else {
		return fmt.Errorf("no hardware key configured and not running on macOS — re-login with 'External Key' flow")
	}
	if err != nil {
		return fmt.Errorf("failed to create config: %w", err)
	}

	usersStr := &meta.IDSUsers
	identityStr := &meta.IDSIdentity

	client := &IMClient{
		Main:                    c,
		UserLogin:               login,
		config:                  cfg,
		users:                   rustpushgo.NewWrappedIdsUsers(usersStr),
		identity:                rustpushgo.NewWrappedIdsngmIdentity(identityStr),
		contactsReady:           false,
		contactsReadyCh:         make(chan struct{}),
		cloudStore:              newCloudBackfillStore(c.Bridge.DB.Database, login.ID),
		sharedProfileStore:      newSharedProfileStore(c.Bridge.DB.Database, login.ID),
		pendingAttachments:      newPendingAttachmentStore(c.Bridge.DB.Database, login.ID),
		fordCache:               NewFordKeyCache(),
		recentUnsends:           make(map[string]time.Time),
		recentOutboundUnsends:   make(map[string]time.Time),
		recentSmsReactionEchoes: make(map[string]time.Time),
		smsPortals:              make(map[string]bool),
		sharedStreamAssetCache:  make(map[string]map[string]struct{}),
		sharedAlbumRooms:        make(map[string]id.RoomID),
		imGroupNames:            make(map[string]string),
		imGroupGuids:            make(map[string]string),
		imGroupParticipants:     make(map[string][]string),
		gidAliases:              make(map[string]string),
		lastGroupForMember:      make(map[string]networkid.PortalKey),
		restorePipelines:        make(map[string]bool),
		forwardBackfillSem:      make(chan struct{}, 3),
	}
	// A login that already has a client is being rebuilt (unknown-error
	// reconnect, network reset). The new client's Connect disconnects the
	// old one, if still live, before opening its own APNs connection.
	if existing, ok := login.Client.(*IMClient); ok && existing != nil {
		client.predecessor = existing
	}

	login.Client = client
	return nil
}
