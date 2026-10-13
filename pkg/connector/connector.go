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

	// mautrix writes its own stock values for every bridge.* and backfill.*
	// key into config.yaml, so a key still at its stock value is treated as
	// "not chosen" and gets this bridge's default instead (bridge_defaults.go).
	// Any other value is the operator's and is kept. The change is in memory
	// only; config.yaml is never rewritten. The defaults apply whatever
	// network.cloudkit_backfill says: without a history source FetchMessages
	// returns nothing. The boolean defaults (phone_numbers_in_profile,
	// backfill.enabled, backfill.queue.enabled) cannot be turned off, because
	// false is their stock value. An operator-set max_initial_messages below
	// MaxInt32 is an explicit cap: FetchMessages skips backward backfill and
	// the startup attachment pre-upload is skipped.
	c.applyBridgeConfigDefaults()
	c.warnShortUnknownErrorAutoReconnect()

	// Start runs after the DB upgrade and before any login is loaded. Delete a
	// leftover legacy carddav.key if no stored login still needs it; logins
	// that do are migrated one by one in LoadUserLogin, and the last one
	// deletes the file.
	c.removeLegacyCardDAVKeyIfUnused(ctx, c.Bridge.Log.With().Str("component", "carddav_migration").Logger())
	return nil
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

	// One-time move of a legacy encrypted CardDAV password into plain
	// metadata. Local file read + AES decrypt + DB save only; runs before
	// anything below can fail, so every stored login gets migrated.
	c.migrateLegacyCardDAVLogin(ctx, login, meta, login.Log)

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
