// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"maunium.net/go/mautrix/bridgev2/database"
)

type PortalMetadata struct {
	ThreadID string `json:"thread_id,omitempty"`
}

type GhostMetadata struct{}

type MessageMetadata struct {
	HasAttachments bool `json:"has_attachments,omitempty"`
}

type UserLoginMetadata struct {
	Platform    string `json:"platform,omitempty"`     // "mac", "rustpush", or "rustpush-local"
	ChatsSynced bool   `json:"chats_synced,omitempty"`
	// rustpush state (persisted across restarts):
	APSState    string `json:"aps_state,omitempty"`
	IDSUsers    string `json:"ids_users,omitempty"`
	IDSIdentity string `json:"ids_identity,omitempty"`
	RelayCode   string `json:"relay_code,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
}

func (c *IMConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal: func() any {
			return &PortalMetadata{}
		},
		Ghost: func() any {
			return &GhostMetadata{}
		},
		Message: func() any {
			return &MessageMetadata{}
		},
		Reaction: nil,
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
	}
}
