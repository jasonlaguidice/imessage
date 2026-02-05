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
	ChatsSynced bool `json:"chats_synced,omitempty"`
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
