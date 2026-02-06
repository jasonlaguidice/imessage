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
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/lrhodin/imessage/imessage"
	_ "github.com/lrhodin/imessage/imessage/mac" // Register mac platform for chat.db
)

// chatDB wraps the macOS chat.db iMessage API for backfill and contact
// resolution. It does NOT listen for incoming messages (rustpush handles that).
type chatDB struct {
	api imessage.API
}

// openChatDB attempts to open the local iMessage chat.db database.
// Returns nil if chat.db is not accessible (e.g., no Full Disk Access).
func openChatDB(log zerolog.Logger) *chatDB {
	if !canReadChatDB(log) {
		log.Warn().Msg("Chat.db not accessible — backfill and contact lookup will be unavailable")
		return nil
	}

	adapter := newBridgeAdapter(&log)
	api, err := imessage.NewAPI(adapter)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to initialize chat.db API via imessage.NewAPI")
		return nil
	}

	return &chatDB{api: api}
}

// Close stops the chat.db API.
func (db *chatDB) Close() {
	if db.api != nil {
		db.api.Stop()
	}
}

// FetchMessages retrieves historical messages from chat.db for backfill.
func (db *chatDB) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams, c *IMClient) (*bridgev2.FetchMessagesResponse, error) {
	portalID := string(params.Portal.ID)
	chatGUID := portalIDToChatGUID(portalID)

	log := zerolog.Ctx(ctx)
	log.Info().Str("portal_id", portalID).Str("chat_guid", chatGUID).Bool("forward", params.Forward).Msg("FetchMessages called")

	if chatGUID == "" {
		log.Warn().Str("portal_id", portalID).Msg("portalIDToChatGUID returned empty")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}

	count := params.Count
	if count <= 0 {
		count = 50
	}

	var messages []*imessage.Message
	var err error

	if params.AnchorMessage != nil {
		if params.Forward {
			messages, err = db.api.GetMessagesSinceDate(chatGUID, params.AnchorMessage.Timestamp, "")
		} else {
			messages, err = db.api.GetMessagesBeforeWithLimit(chatGUID, params.AnchorMessage.Timestamp, count)
		}
	} else {
		messages, err = db.api.GetMessagesWithLimit(chatGUID, count, "")
	}
	if err != nil {
		log.Error().Err(err).Str("chat_guid", chatGUID).Msg("Failed to fetch messages from chat.db")
		return nil, fmt.Errorf("failed to fetch messages from chat.db: %w", err)
	}

	log.Info().Str("chat_guid", chatGUID).Int("raw_message_count", len(messages)).Msg("Got messages from chat.db")

	backfillMessages := make([]*bridgev2.BackfillMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.ItemType != imessage.ItemTypeMessage || msg.Tapback != nil {
			continue
		}
		sender := chatDBMakeEventSender(msg, c)
		cm, err := convertChatDBMessage(ctx, params.Portal, nil, msg)
		if err != nil {
			continue
		}

		backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
			ConvertedMessage: cm,
			Sender:           sender,
			ID:               makeMessageID(msg.GUID),
			TxnID:            networkid.TransactionID(msg.GUID),
			Timestamp:        msg.Time,
			StreamOrder:      msg.Time.UnixMilli(),
		})

		for i, att := range msg.Attachments {
			if att == nil {
				continue
			}
			attCm, err := convertChatDBAttachment(ctx, params.Portal, nil, msg, att)
			if err != nil {
				continue
			}
			partID := fmt.Sprintf("%s_att%d", msg.GUID, i)
			backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
				ConvertedMessage: attCm,
				Sender:           sender,
				ID:               makeMessageID(partID),
				TxnID:            networkid.TransactionID(partID),
				Timestamp:        msg.Time.Add(time.Duration(i+1) * time.Millisecond),
				StreamOrder:      msg.Time.UnixMilli() + int64(i+1),
			})
		}
	}

	return &bridgev2.FetchMessagesResponse{
		Messages: backfillMessages,
		HasMore:  len(messages) >= count,
		Forward:  params.Forward,
	}, nil
}

// ============================================================================
// chat.db ↔ portal ID conversion
// ============================================================================

// portalIDToChatGUID converts a clean portal ID (e.g., "mailto:user@example.com")
// to a chat.db GUID (e.g., "any;-;user@example.com").
// Modern macOS (Ventura+) uses "any" as the service prefix for all chats.
func portalIDToChatGUID(portalID string) string {
	localID := stripIdentifierPrefix(portalID)
	if localID == "" {
		return ""
	}
	// Group chats already contain the full GUID format
	if strings.HasPrefix(portalID, "any;") {
		return portalID
	}
	return "any;-;" + localID
}

// identifierToPortalID converts a chat.db Identifier to a clean portal ID.
func identifierToPortalID(id imessage.Identifier) networkid.PortalID {
	if id.IsGroup {
		// Group chats keep the full GUID as portal ID
		return networkid.PortalID(id.String())
	}
	// DMs: use the local ID with appropriate prefix
	if strings.HasPrefix(id.LocalID, "+") {
		return networkid.PortalID("tel:" + id.LocalID)
	}
	if strings.Contains(id.LocalID, "@") {
		return networkid.PortalID("mailto:" + id.LocalID)
	}
	return networkid.PortalID(id.LocalID)
}

// stripIdentifierPrefix removes tel:/mailto: prefixes from identifiers.
func stripIdentifierPrefix(id string) string {
	id = strings.TrimPrefix(id, "tel:")
	id = strings.TrimPrefix(id, "mailto:")
	return id
}

// identifierToDisplaynameParams creates DisplaynameParams from an identifier string.
func identifierToDisplaynameParams(identifier string) DisplaynameParams {
	localID := stripIdentifierPrefix(identifier)
	if strings.HasPrefix(localID, "+") {
		return DisplaynameParams{Phone: localID, ID: localID}
	}
	if strings.Contains(localID, "@") {
		return DisplaynameParams{Email: localID, ID: localID}
	}
	return DisplaynameParams{ID: localID}
}

// ============================================================================
// chat.db message conversion
// ============================================================================

func chatDBMakeEventSender(msg *imessage.Message, c *IMClient) bridgev2.EventSender {
	if msg.IsFromMe {
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(c.handle),
		}
	}
	return bridgev2.EventSender{
		IsFromMe: false,
		Sender:   makeUserID(msg.Sender.LocalID),
	}
}

func convertChatDBMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *imessage.Message) (*bridgev2.ConvertedMessage, error) {
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    msg.Text,
	}
	if msg.Subject != "" {
		if msg.Text != "" {
			content.Body = fmt.Sprintf("**%s**\n%s", msg.Subject, msg.Text)
			content.Format = event.FormatHTML
			content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br/>%s", msg.Subject, msg.Text)
		} else {
			content.Body = msg.Subject
		}
	}
	if msg.IsEmote {
		content.MsgType = event.MsgEmote
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

func convertChatDBAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *imessage.Message, att *imessage.Attachment) (*bridgev2.ConvertedMessage, error) {
	mimeType := att.GetMimeType()
	fileName := att.GetFileName()

	data, err := att.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %s: %w", att.PathOnDisk, err)
	}

	content := &event.MessageEventContent{
		MsgType: mimeToMsgType(mimeType),
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
		},
	}

	if intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", data, fileName, mimeType)
		if err != nil {
			return nil, fmt.Errorf("failed to upload attachment: %w", err)
		}
		if encFile != nil {
			content.File = encFile
		} else {
			content.URL = url
		}
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}
