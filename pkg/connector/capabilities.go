// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

var caps = &event.RoomFeatures{
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
	Reply:               event.CapLevelFullySupported,
	Edit:                event.CapLevelFullySupported,
	Delete:              event.CapLevelFullySupported,
	Reaction:            event.CapLevelFullySupported,
	ReactionCount:       1,
	ReadReceipts:        true,
	TypingNotifications: true,
}

var capsDM *event.RoomFeatures

func init() {
	c := *caps
	capsDM = &c
	capsDM.ID = "fi.mau.imessage.capabilities.2024_01+dm"
}

var generalCaps = &bridgev2.NetworkGeneralCapabilities{
	DisappearingMessages: false,
	AggressiveUpdateInfo: false,
}

func (c *IMConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return generalCaps
}

func (c *IMConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}
