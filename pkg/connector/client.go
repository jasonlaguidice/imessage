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

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/lrhodin/imessage/imessage"
	_ "github.com/lrhodin/imessage/imessage/mac" // Register "mac" platform
)

type IMClient struct {
	Main      *IMConnector
	UserLogin *bridgev2.UserLogin
	imAPI     imessage.API
	stopChan  chan struct{}
}

var _ bridgev2.NetworkAPI = (*IMClient)(nil)

func (c *IMClient) Connect(ctx context.Context) {
	log := c.UserLogin.Log.With().Str("component", "imessage").Logger()
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})

	adapter := newBridgeAdapter(&log)
	api, err := imessage.NewAPI(adapter)
	if err != nil {
		log.Err(err).Msg("Failed to create iMessage API")
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Message:    fmt.Sprintf("Failed to connect to iMessage: %v", err),
		})
		return
	}
	c.imAPI = api
	c.stopChan = make(chan struct{})

	// Start the mac connector's polling loop in a goroutine.
	// It watches ~/Library/Messages/chat.db via fsnotify and calls readyCallback
	// when it's ready to receive messages.
	go func() {
		err := api.Start(func() {
			log.Info().Msg("iMessage connector ready, starting event listeners")
			c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
			// Start listening for incoming messages and read receipts
			go c.listenMessages(log)
			go c.listenReadReceipts(log)
		})
		if err != nil {
			log.Err(err).Msg("iMessage connector stopped with error")
			c.UserLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateTransientDisconnect,
				Message:    err.Error(),
			})
		}
	}()
}

func (c *IMClient) Disconnect() {
	if c.imAPI != nil {
		c.imAPI.Stop()
	}
	if c.stopChan != nil {
		close(c.stopChan)
		c.stopChan = nil
	}
}

func (c *IMClient) IsLoggedIn() bool {
	return c.imAPI != nil
}

func (c *IMClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *IMClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	// The "self" user in iMessage doesn't have a fixed identifier,
	// but messages from self have IsFromMe=true.
	return false
}

func (c *IMClient) listenMessages(log zerolog.Logger) {
	if c.imAPI == nil {
		return
	}
	msgChan := c.imAPI.MessageChan()
	if msgChan == nil {
		return
	}
	for {
		select {
		case msg, ok := <-msgChan:
			if !ok {
				return
			}
			c.handleIMessage(log, msg)
		case <-c.stopChan:
			return
		}
	}
}

func (c *IMClient) listenReadReceipts(log zerolog.Logger) {
	if c.imAPI == nil {
		return
	}
	receiptChan := c.imAPI.ReadReceiptChan()
	if receiptChan == nil {
		return
	}
	for {
		select {
		case receipt, ok := <-receiptChan:
			if !ok {
				return
			}
			c.handleIMessageReadReceipt(log, receipt)
		case <-c.stopChan:
			return
		}
	}
}
