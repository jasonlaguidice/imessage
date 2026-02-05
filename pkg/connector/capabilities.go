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
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
)

var imessageCaps = &event.RoomFeatures{
	ID: "fi.mau.imessage.capabilities.2024_01",

	Formatting: map[event.FormattingFeature]event.CapabilitySupportLevel{
		event.FmtBold:   event.CapLevelDropped,
		event.FmtItalic: event.CapLevelDropped,
	},
	File: map[event.CapabilityMsgType]*event.FileFeatures{
		event.MsgImage: {
			MimeTypes: map[string]event.CapabilitySupportLevel{
				"image/jpeg": event.CapLevelFullySupported,
				"image/png":  event.CapLevelFullySupported,
				"image/gif":  event.CapLevelFullySupported,
				"image/heic": event.CapLevelFullySupported,
			},
		},
		event.MsgVideo: {
			MimeTypes: map[string]event.CapabilitySupportLevel{
				"video/mp4":       event.CapLevelFullySupported,
				"video/quicktime": event.CapLevelFullySupported,
			},
		},
		event.MsgAudio: {
			MimeTypes: map[string]event.CapabilitySupportLevel{
				"audio/mpeg": event.CapLevelFullySupported,
				"audio/aac":  event.CapLevelFullySupported,
				"audio/mp4":  event.CapLevelFullySupported,
			},
		},
		event.MsgFile: {
			MimeTypes: map[string]event.CapabilitySupportLevel{
				"*/*": event.CapLevelFullySupported,
			},
		},
	},
	MaxTextLength:       -1,
	Reply:               event.CapLevelUnsupported, // mac connector doesn't support replies
	Edit:                event.CapLevelUnsupported,
	Delete:              event.CapLevelUnsupported,
	Reaction:            event.CapLevelUnsupported, // mac connector can't send tapbacks
	ReadReceipts:        false,
	TypingNotifications: false,
}

var imessageCapsDM *event.RoomFeatures

func init() {
	imessageCapsDM = &(*imessageCaps)
	imessageCapsDM.ID = "fi.mau.imessage.capabilities.2024_01+dm"
}

func (c *IMClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	if portal.RoomType == database.RoomTypeDM {
		return imessageCapsDM
	}
	return imessageCaps
}

var imessageGeneralCaps = &bridgev2.NetworkGeneralCapabilities{
	DisappearingMessages: false,
	AggressiveUpdateInfo: false,
}

func (c *IMConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return imessageGeneralCaps
}

func (c *IMConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}
