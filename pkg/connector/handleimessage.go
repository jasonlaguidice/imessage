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

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/lrhodin/imessage/imessage"
)

func (c *IMClient) handleIMessage(log zerolog.Logger, msg *imessage.Message) {
	// When a rustpush login is active, skip real-time message forwarding from
	// the mac connector to avoid double-delivery. Rustpush handles real-time
	// messages faster and with richer feature support (edits, unsends, etc.).
	// The mac connector still provides backfill and chat sync.
	if c.Main.hasActiveRustpushLogin() {
		log.Debug().Str("guid", msg.GUID).Msg("Skipping mac connector message (rustpush active)")
		return
	}
	if msg.ItemType == imessage.ItemTypeName {
		c.handleGroupNameChange(log, msg)
		return
	}
	if msg.ItemType == imessage.ItemTypeMember {
		// Group member changes - just trigger a resync
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatResync,
				PortalKey: makePortalKey(msg.ChatGUID, c.UserLogin.ID),
			},
			GetChatInfoFunc: c.GetChatInfo,
		})
		return
	}
	if msg.Tapback != nil {
		c.handleTapback(log, msg)
		return
	}

	// Skip "from me" messages with no text — these are echoes of messages sent
	// by another connector (e.g., rustpush) that appear in chat.db without body text.
	if msg.IsFromMe && msg.Text == "" && msg.Subject == "" && len(msg.Attachments) == 0 {
		return
	}

	sender := c.makeEventSender(msg)
	portalKey := makePortalKey(msg.ChatGUID, c.UserLogin.ID)

	// Handle text messages
	if msg.Text != "" || msg.Subject != "" {
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*imessage.Message]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       sender,
				Timestamp:    msg.Time,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("im_guid", msg.GUID).Str("im_chat", msg.ChatGUID)
				},
			},
			Data:               msg,
			ID:                 makeMessageID(msg.GUID),
			ConvertMessageFunc: convertIMessage,
		})
	}

	// Handle attachments (each as a separate message part if there's also text,
	// or as the message if text is empty)
	for i, att := range msg.Attachments {
		if att == nil {
			continue
		}
		attMsg := &attachmentMessage{
			Message:    msg,
			Attachment: att,
			Index:      i,
		}
		partID := msg.GUID
		if msg.Text != "" || i > 0 {
			partID = fmt.Sprintf("%s_att%d", msg.GUID, i)
		} else {
			// First attachment with no text uses the main message ID
		}
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*attachmentMessage]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       sender,
				Timestamp:    msg.Time,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("im_guid", partID).Str("im_att_guid", att.GUID)
				},
			},
			Data:               attMsg,
			ID:                 makeMessageID(partID),
			ConvertMessageFunc: convertAttachment,
		})
	}
}

type attachmentMessage struct {
	*imessage.Message
	Attachment *imessage.Attachment
	Index      int
}

func convertIMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *imessage.Message) (*bridgev2.ConvertedMessage, error) {
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
	if msg.IsAudioMessage {
		content.MsgType = event.MsgAudio
	}
	if msg.ErrorNotice != "" {
		content.MsgType = event.MsgNotice
		content.Body = fmt.Sprintf("Error: %s", msg.ErrorNotice)
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			ID:      networkid.PartID(""),
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

func convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, attMsg *attachmentMessage) (*bridgev2.ConvertedMessage, error) {
	att := attMsg.Attachment
	mimeType := att.GetMimeType()
	fileName := att.GetFileName()

	// Read the file
	data, err := att.Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read attachment %s: %w", att.PathOnDisk, err)
	}

	// Determine message type from mime
	msgType := event.MsgFile
	if strings.HasPrefix(mimeType, "image/") {
		msgType = event.MsgImage
	} else if strings.HasPrefix(mimeType, "video/") {
		msgType = event.MsgVideo
	} else if strings.HasPrefix(mimeType, "audio/") {
		msgType = event.MsgAudio
	}

	if attMsg.IsAudioMessage {
		msgType = event.MsgAudio
	}

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
		},
	}

	// Upload to Matrix if we have an intent
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
			ID:      networkid.PartID(fmt.Sprintf("att%d", attMsg.Index)),
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

func (c *IMClient) handleTapback(log zerolog.Logger, msg *imessage.Message) {
	tapback := msg.Tapback
	if tapback == nil {
		return
	}

	sender := c.makeEventSender(msg)
	portalKey := makePortalKey(msg.ChatGUID, c.UserLogin.ID)
	emoji := tapback.Type.Emoji()

	evtType := bridgev2.RemoteEventReaction
	if tapback.Remove {
		evtType = bridgev2.RemoteEventReactionRemove
	}
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: msg.Time,
		},
		TargetMessage: makeMessageID(tapback.TargetGUID),
		EmojiID:       "",
		Emoji:         emoji,
	})
}

func (c *IMClient) handleGroupNameChange(log zerolog.Logger, msg *imessage.Message) {
	portalKey := makePortalKey(msg.ChatGUID, c.UserLogin.ID)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg),
			Timestamp: msg.Time,
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			ChatInfo: &bridgev2.ChatInfo{
				Name: &msg.NewGroupName,
			},
		},
	})
}

func (c *IMClient) handleIMessageReadReceipt(log zerolog.Logger, receipt *imessage.ReadReceipt) {
	if c.Main.hasActiveRustpushLogin() {
		return
	}
	portalKey := makePortalKey(receipt.ChatGUID, c.UserLogin.ID)
	senderID := receipt.SenderGUID
	if receipt.IsFromMe && senderID == "" {
		senderID = string(c.UserLogin.ID)
	} else if senderID != "" {
		// SenderGUID may be a chat GUID (e.g. "any;-;user@example.com") when
		// the mac connector infers the sender from a DM read receipt.
		// Extract the actual user identifier so the correct ghost is used.
		if parsed := imessage.ParseIdentifier(senderID); parsed.LocalID != "" {
			senderID = parsed.LocalID
		}
	}
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: portalKey,
			Sender: bridgev2.EventSender{
				IsFromMe: receipt.IsFromMe,
				Sender:   makeUserID(senderID),
			},
			Timestamp: receipt.ReadAt,
		},
		LastTarget: makeMessageID(receipt.ReadUpTo),
	})
}

func (c *IMClient) makeEventSender(msg *imessage.Message) bridgev2.EventSender {
	if msg.IsFromMe {
		// Use the login ID as the sender to ensure a valid ghost reference.
		// The bridgev2 framework will double-puppet the event using the user's MXID.
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(string(c.UserLogin.ID)),
		}
	}
	return bridgev2.EventSender{
		IsFromMe: false,
		Sender:   makeUserID(msg.Sender.LocalID),
	}
}


