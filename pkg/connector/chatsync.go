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
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

func (c *IMClient) syncChats(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if meta.ChatsSynced {
		log.Debug().Msg("Chats already synced, skipping initial sync")
		return
	}

	// Get all chats with messages in the last 3 months
	minDate := time.Now().AddDate(0, -3, 0)
	chats, err := c.imAPI.GetChatsWithMessagesAfter(minDate)
	if err != nil {
		log.Err(err).Msg("Failed to get chat list for initial sync")
		return
	}

	log.Info().Int("chat_count", len(chats)).Msg("Starting initial chat sync")

	for _, chat := range chats {
		info, err := c.imAPI.GetChatInfo(chat.ChatGUID, chat.ThreadID)
		if err != nil {
			log.Warn().Err(err).Str("chat_guid", chat.ChatGUID).Msg("Failed to get chat info, skipping")
			continue
		}
		if info == nil || info.NoCreateRoom {
			continue
		}

		portalKey := makePortalKey(chat.ChatGUID, c.UserLogin.ID)
		chatInfo := c.imChatInfoToBridgev2(info)

		// Get the latest message timestamp so bridgev2 knows backfill is needed
		var latestTS time.Time
		msgs, err := c.imAPI.GetMessagesWithLimit(chat.ChatGUID, 1, "")
		if err == nil && len(msgs) > 0 {
			latestTS = msgs[0].Time
		}

		evt := &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("chat_guid", chat.ChatGUID)
				},
			},
			ChatInfo:        chatInfo,
			LatestMessageTS: latestTS,
			GetChatInfoFunc: c.GetChatInfo,
		}
		c.UserLogin.QueueRemoteEvent(evt)
	}

	meta.ChatsSynced = true
	err = c.UserLogin.Save(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to save user login metadata after chat sync")
	}
	log.Info().Msg("Initial chat sync complete")
}
