// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
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

	"maunium.net/go/mautrix/bridgev2"

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
		DisplayName:      "iMessage",
		NetworkURL:       "https://support.apple.com/messages",
		NetworkIcon:      "mxc://maunium.net/tManJEpANASZvDVzvRvhILdl",
		NetworkID:        "imessage",
		BeeperBridgeType: "imessagego",
		DefaultPort:      29332,
	}
}

func (c *IMConnector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
}

func (c *IMConnector) Start(ctx context.Context) error {
	log := c.Bridge.Log.With().Str("component", "imessage").Logger()
	if isRunningOnMacOS() {
		// Attempt to read chat.db early so macOS registers the app in the
		// Full Disk Access list.  Without this, the TCC entry only appears
		// after the first login (when Connect() calls openChatDB()), which
		// means new users can't grant FDA before logging in.
		if !canReadChatDB(log) {
			showDialogAndOpenFDA(log)
			waitForFDA(log)
		}
		// Request contact access so macOS prompts early (same as normal install flow).
		requestContactAccess(log)
	} else {
		log.Info().Msg("Running on non-macOS platform — chat.db and contacts unavailable")
	}
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

func (c *IMConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	log := c.Bridge.Log.With().Str("component", "imessage").Logger()

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
	apsStateStr := &meta.APSState

	// Eagerly persist full session state to the backup file so it survives DB resets.
	saveSessionState(log, PersistedSessionState{
		IDSIdentity: meta.IDSIdentity,
		APSState:    meta.APSState,
		IDSUsers:    meta.IDSUsers,
	})

	client := &IMClient{
		Main:          c,
		UserLogin:     login,
		config:        cfg,
		users:         rustpushgo.NewWrappedIdsUsers(usersStr),
		identity:      rustpushgo.NewWrappedIdsngmIdentity(identityStr),
		connection:    rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(apsStateStr)),
		recentUnsends: make(map[string]time.Time),
		smsPortals:    make(map[string]bool),
	}

	login.Client = client
	return nil
}
