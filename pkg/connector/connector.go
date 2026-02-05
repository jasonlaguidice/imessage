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

	"maunium.net/go/mautrix/bridgev2"
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

func (c *IMConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	client := &IMClient{
		Main:      c,
		UserLogin: login,
	}
	login.Client = client
	return nil
}
