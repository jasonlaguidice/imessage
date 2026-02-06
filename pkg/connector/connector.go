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
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

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
		BeeperBridgeType: "imessage",
		DefaultPort:      29332,
	}
}

func (c *IMConnector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
}

func (c *IMConnector) Start(ctx context.Context) error {
	return nil
}

func (c *IMConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		Name:        "Apple ID",
		Description: "Log in with your Apple ID to send and receive iMessages",
		ID:          LoginFlowIDAppleID,
	}}
}

func (c *IMConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != LoginFlowIDAppleID {
		return nil, fmt.Errorf("unknown login flow: %s", flowID)
	}
	return &AppleIDLogin{User: user, Main: c}, nil
}

func (c *IMConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)

	rustpushgo.InitLogger()

	var cfg *rustpushgo.WrappedOsConfig
	var err error

	if meta.DeviceID != "" {
		cfg, err = rustpushgo.CreateLocalMacosConfigWithDeviceId(meta.DeviceID)
	} else {
		cfg, err = rustpushgo.CreateLocalMacosConfig()
	}
	if err != nil {
		return fmt.Errorf("failed to create local config: %w", err)
	}

	usersStr := &meta.IDSUsers
	identityStr := &meta.IDSIdentity
	apsStateStr := &meta.APSState

	client := &IMClient{
		Main:          c,
		UserLogin:     login,
		config:        cfg,
		users:         rustpushgo.NewWrappedIdsUsers(usersStr),
		identity:      rustpushgo.NewWrappedIdsngmIdentity(identityStr),
		connection:    rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(apsStateStr)),
		recentUnsends: make(map[string]time.Time),
	}

	login.Client = client
	return nil
}
