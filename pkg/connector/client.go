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
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/lrhodin/imessage/imessage"
	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// IMClient implements bridgev2.NetworkAPI using the rustpush iMessage protocol
// library for real-time messaging. On macOS with Full Disk Access, it also
// opens chat.db for backfill and contact name resolution.
type IMClient struct {
	Main      *IMConnector
	UserLogin *bridgev2.UserLogin

	// Rustpush (primary — real-time send/receive)
	client     *rustpushgo.Client
	config     *rustpushgo.WrappedOsConfig
	users      *rustpushgo.WrappedIdsUsers
	identity   *rustpushgo.WrappedIdsngmIdentity
	connection *rustpushgo.WrappedApsConnection
	handle     string // Primary iMessage handle (e.g., tel:+1234567890)

	// Chat.db supplement (optional — backfill + contacts)
	chatDB *chatDB

	// Background goroutine lifecycle
	stopChan chan struct{}

	// Unsend re-delivery suppression
	recentUnsends     map[string]time.Time
	recentUnsendsLock sync.Mutex

	// SMS portal tracking: portal IDs known to be SMS-only contacts
	smsPortals     map[string]bool
	smsPortalsLock sync.RWMutex
}

var _ bridgev2.NetworkAPI = (*IMClient)(nil)
var _ bridgev2.EditHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.ReactionHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.ReadReceiptHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.TypingHandlingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.IdentifierResolvingNetworkAPI = (*IMClient)(nil)
var _ bridgev2.BackfillingNetworkAPI = (*IMClient)(nil)
var _ rustpushgo.MessageCallback = (*IMClient)(nil)
var _ rustpushgo.UpdateUsersCallback = (*IMClient)(nil)

// ============================================================================
// Lifecycle
// ============================================================================

func (c *IMClient) Connect(ctx context.Context) {
	log := c.UserLogin.Log.With().Str("component", "imessage").Logger()
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})

	rustpushgo.InitLogger()

	client, err := rustpushgo.NewClient(c.connection, c.users, c.identity, c.config, c, c)
	if err != nil {
		log.Err(err).Msg("Failed to create rustpush client")
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Message:    fmt.Sprintf("Failed to connect: %v", err),
		})
		return
	}
	c.client = client

	// Get our handle
	handles := client.GetHandles()
	if len(handles) > 0 {
		c.handle = handles[0]
	}
	log.Info().Strs("handles", handles).Msg("Connected to iMessage")

	// Persist state after connect (APS tokens, IDS keys, device ID)
	c.persistState(log)

	// Start periodic state saver (every 5 minutes)
	c.stopChan = make(chan struct{})
	go c.periodicStateSave(log)

	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})

	// Open chat.db for backfill and contact info (macOS with FDA only)
	c.chatDB = openChatDB(log)
	if c.chatDB != nil {
		log.Info().Msg("Chat.db available for backfill and contacts")
		go c.periodicChatDBSync(log)
	}
}

func (c *IMClient) Disconnect() {
	if c.stopChan != nil {
		close(c.stopChan)
		c.stopChan = nil
	}
	if c.client != nil {
		c.client.Stop()
		c.client.Destroy()
		c.client = nil
	}
	if c.chatDB != nil {
		c.chatDB.Close()
		c.chatDB = nil
	}
}

func (c *IMClient) IsLoggedIn() bool {
	return c.client != nil
}

func (c *IMClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *IMClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return string(userID) == c.handle
}

func (c *IMClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	if portal.RoomType == database.RoomTypeDM {
		return capsDM
	}
	return caps
}

// ============================================================================
// Callbacks from rustpush
// ============================================================================

// OnMessage is called by rustpush when a message is received via APNs.
func (c *IMClient) OnMessage(msg rustpushgo.WrappedMessage) {
	log := c.UserLogin.Log.With().
		Str("component", "imessage").
		Str("msg_uuid", msg.Uuid).
		Logger()

	// Send delivery receipt if requested
	if msg.SendDelivered && msg.Sender != nil && !msg.IsDelivered && !msg.IsReadReceipt {
		go func() {
			conv := c.makeConversation(msg.Participants, msg.GroupName)
			if err := c.client.SendDeliveryReceipt(conv, c.handle); err != nil {
				log.Warn().Err(err).Msg("Failed to send delivery receipt")
			}
		}()
	}

	if msg.IsDelivered {
		c.handleDeliveryReceipt(log, msg)
		return
	}

	if msg.IsReadReceipt {
		c.handleReadReceipt(log, msg)
		return
	}
	if msg.IsTyping {
		c.handleTyping(log, msg)
		return
	}
	if msg.IsError {
		log.Warn().
			Str("for_uuid", ptrStringOr(msg.ErrorForUuid, "")).
			Uint64("status", ptrUint64Or(msg.ErrorStatus, 0)).
			Str("status_str", ptrStringOr(msg.ErrorStatusStr, "")).
			Msg("Received iMessage error")
		return
	}
	if msg.IsPeerCacheInvalidate {
		log.Debug().Msg("Peer cache invalidated")
		return
	}
	if msg.IsTapback {
		c.handleTapback(log, msg)
		return
	}
	if msg.IsEdit {
		c.handleEdit(log, msg)
		return
	}
	if msg.IsUnsend {
		c.handleUnsend(log, msg)
		return
	}
	if msg.IsRename {
		c.handleRename(log, msg)
		return
	}
	if msg.IsParticipantChange {
		c.handleParticipantChange(log, msg)
		return
	}

	c.handleMessage(log, msg)
}

// UpdateUsers is called when IDS keys are refreshed.
func (c *IMClient) UpdateUsers(users *rustpushgo.WrappedIdsUsers) {
	log := c.UserLogin.Log.With().Str("component", "imessage").Logger()
	c.users = users

	// Persist all state (APS tokens, IDS keys, identity, device ID) — not just
	// IDSUsers — so a crash between periodic saves doesn't lose APS state.
	c.persistState(log)
	log.Debug().Msg("IDS users updated, full state persisted")
}

// ============================================================================
// Incoming message handlers
// ============================================================================

func (c *IMClient) handleMessage(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	if c.wasUnsent(msg.Uuid) {
		log.Debug().Str("uuid", msg.Uuid).Msg("Suppressing re-delivery of unsent message")
		return
	}

	sender := c.makeEventSender(msg.Sender)
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)

	// Track SMS portals so outbound replies use the correct service type
	if msg.IsSms {
		c.markPortalSMS(string(portalKey.ID))
	}

	if msg.Text != nil && *msg.Text != "" {
		content := &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    *msg.Text,
		}
		if msg.Subject != nil && *msg.Subject != "" {
			content.Body = fmt.Sprintf("**%s**\n%s", *msg.Subject, *msg.Text)
			content.Format = event.FormatHTML
			content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br/>%s", *msg.Subject, *msg.Text)
		}

		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*rustpushgo.WrappedMessage]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       sender,
				Timestamp:    time.UnixMilli(int64(msg.TimestampMs)),
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("msg_uuid", msg.Uuid)
				},
			},
			Data:               &msg,
			ID:                 makeMessageID(msg.Uuid),
			ConvertMessageFunc: convertMessage,
		})
	}

	for i, att := range msg.Attachments {
		attID := msg.Uuid
		if i > 0 || (msg.Text != nil && *msg.Text != "") {
			attID = fmt.Sprintf("%s_att%d", msg.Uuid, i)
		}
		attMsg := &attachmentMessage{
			WrappedMessage: &msg,
			Attachment:     &att,
			Index:          i,
		}
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*attachmentMessage]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       sender,
				Timestamp:    time.UnixMilli(int64(msg.TimestampMs)),
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("msg_uuid", attID)
				},
			},
			Data:               attMsg,
			ID:                 makeMessageID(attID),
			ConvertMessageFunc: convertAttachment,
		})
	}
}

func (c *IMClient) handleTapback(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	targetGUID := ptrStringOr(msg.TapbackTargetUuid, "")
	emoji := tapbackTypeToEmoji(msg.TapbackType, msg.TapbackEmoji)

	evtType := bridgev2.RemoteEventReaction
	if msg.TapbackRemove {
		evtType = bridgev2.RemoteEventReactionRemove
	}

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		TargetMessage: makeMessageID(targetGUID),
		Emoji:         emoji,
	})
}

func (c *IMClient) handleEdit(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	targetGUID := ptrStringOr(msg.EditTargetUuid, "")
	newText := ptrStringOr(msg.EditNewText, "")

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		Data:          newText,
		ID:            makeMessageID(msg.Uuid),
		TargetMessage: makeMessageID(targetGUID),
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, text string) (*bridgev2.ConvertedEdit, error) {
			var targetPart *database.Message
			if len(existing) > 0 {
				targetPart = existing[0]
			}
			return &bridgev2.ConvertedEdit{
				ModifiedParts: []*bridgev2.ConvertedEditPart{{
					Part: targetPart,
					Type: event.EventMessage,
					Content: &event.MessageEventContent{
						MsgType: event.MsgText,
						Body:    text,
					},
				}},
			}, nil
		},
	})
}

func (c *IMClient) handleUnsend(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	targetGUID := ptrStringOr(msg.UnsendTargetUuid, "")

	c.trackUnsend(targetGUID)

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.MessageRemove{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessageRemove,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		TargetMessage: makeMessageID(targetGUID),
	})
}

func (c *IMClient) handleRename(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	newName := ptrStringOr(msg.NewChatName, "")
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatInfoChange,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{
			ChatInfo: &bridgev2.ChatInfo{
				Name: &newName,
			},
		},
	})
}

func (c *IMClient) handleParticipantChange(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatResync,
			PortalKey: portalKey,
		},
		GetChatInfoFunc: c.GetChatInfo,
	})
}

func (c *IMClient) handleReadReceipt(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		LastTarget: makeMessageID(msg.Uuid),
	})
}

func (c *IMClient) handleDeliveryReceipt(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventDeliveryReceipt,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		LastTarget: makeMessageID(msg.Uuid),
	})
}

func (c *IMClient) handleTyping(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Typing{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventTyping,
			PortalKey: portalKey,
			Sender:    c.makeEventSender(msg.Sender),
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		Timeout: 60 * time.Second,
	})
}

// ============================================================================
// Matrix → iMessage
// ============================================================================

func (c *IMClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)

	// File/image messages
	if msg.Content.URL != "" || msg.Content.File != nil {
		return c.handleMatrixFile(ctx, msg, conv)
	}

	uuid, err := c.client.SendMessage(conv, msg.Content.Body, c.handle)
	if err != nil {
		return nil, fmt.Errorf("failed to send iMessage: %w", err)
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(uuid),
			SenderID:  makeUserID(c.handle),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{},
		},
	}, nil
}

func (c *IMClient) handleMatrixFile(ctx context.Context, msg *bridgev2.MatrixMessage, conv rustpushgo.WrappedConversation) (*bridgev2.MatrixMessageResponse, error) {
	var data []byte
	var err error
	if msg.Content.File != nil {
		data, err = c.Main.Bridge.Bot.DownloadMedia(ctx, msg.Content.File.URL, msg.Content.File)
	} else {
		data, err = c.Main.Bridge.Bot.DownloadMedia(ctx, msg.Content.URL, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to download media: %w", err)
	}

	fileName := msg.Content.Body
	if fileName == "" {
		fileName = "file"
	}

	mimeType := "application/octet-stream"
	if msg.Content.Info != nil && msg.Content.Info.MimeType != "" {
		mimeType = msg.Content.Info.MimeType
	}

	uuid, err := c.client.SendAttachment(conv, data, mimeType, mimeToUTI(mimeType), fileName, c.handle)
	if err != nil {
		return nil, fmt.Errorf("failed to send attachment: %w", err)
	}

	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:        makeMessageID(uuid),
			SenderID:  makeUserID(c.handle),
			Timestamp: time.Now(),
			Metadata:  &MessageMetadata{HasAttachments: true},
		},
	}, nil
}

func (c *IMClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if c.client == nil {
		return nil
	}
	conv := c.portalToConversation(msg.Portal)
	return c.client.SendTyping(conv, msg.IsTyping, c.handle)
}

func (c *IMClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	if c.client == nil {
		return nil
	}
	conv := c.portalToConversation(receipt.Portal)
	return c.client.SendReadReceipt(conv, c.handle)
}

func (c *IMClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	targetGUID := string(msg.EditTarget.ID)

	_, err := c.client.SendEdit(conv, targetGUID, 0, msg.Content.Body, c.handle)
	if err == nil {
		// Work around mautrix-go bridgev2 not incrementing EditCount before saving.
		msg.EditTarget.EditCount++
	}
	return err
}

func (c *IMClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	_, err := c.client.SendUnsend(conv, string(msg.TargetMessage.ID), 0, c.handle)
	return err
}

func (c *IMClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	return bridgev2.MatrixReactionPreResponse{
		SenderID: makeUserID(c.handle),
		Emoji:    msg.Content.RelatesTo.Key,
	}, nil
}

func (c *IMClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	reaction, emoji := emojiToTapbackType(msg.Content.RelatesTo.Key)

	_, err := c.client.SendTapback(conv, string(msg.TargetMessage.ID), 0, reaction, emoji, false, c.handle)
	if err != nil {
		return nil, fmt.Errorf("failed to send tapback: %w", err)
	}

	return &database.Reaction{
		MessageID: msg.TargetMessage.ID,
		SenderID:  makeUserID(c.handle),
		Emoji:     msg.Content.RelatesTo.Key,
		Metadata:  &MessageMetadata{},
		MXID:      msg.Event.ID,
	}, nil
}

func (c *IMClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	reaction, emoji := emojiToTapbackType(msg.TargetReaction.Emoji)
	_, err := c.client.SendTapback(conv, string(msg.TargetReaction.MessageID), 0, reaction, emoji, true, c.handle)
	return err
}

// ============================================================================
// Chat & user info
// ============================================================================

func (c *IMClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	portalID := string(portal.ID)
	isGroup := strings.Contains(portalID, ",") || strings.HasPrefix(portalID, "any;+;")

	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: c.chatDB != nil,
	}

	if isGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)
		members := &bridgev2.ChatMemberList{
			IsFull: false,
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				makeUserID(c.handle): {
					EventSender: bridgev2.EventSender{
						IsFromMe:    true,
						SenderLogin: c.UserLogin.ID,
						Sender:      makeUserID(c.handle),
					},
					Membership: event.MembershipJoin,
				},
			},
		}
		chatInfo.Members = members

		// Set group name from chat.db
		if c.chatDB != nil {
			chatGUID := portalID // group portal IDs are the full GUID
			if info, err := c.chatDB.api.GetChatInfo(chatGUID, ""); err == nil && info != nil {
				name := info.DisplayName
				if name == "" {
					name = c.buildGroupName(info.Members)
				}
				chatInfo.Name = &name
			}
		}
	} else {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDM)
		otherUser := makeUserID(portalID)
		members := &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: otherUser,
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				makeUserID(c.handle): {
					EventSender: bridgev2.EventSender{
						IsFromMe:    true,
						SenderLogin: c.UserLogin.ID,
						Sender:      makeUserID(c.handle),
					},
					Membership: event.MembershipJoin,
				},
				otherUser: {
					EventSender: bridgev2.EventSender{Sender: otherUser},
					Membership:  event.MembershipJoin,
				},
			},
		}

		// Always set a room name for DMs so Element doesn't show the ugly MXID.
		localID := stripIdentifierPrefix(portalID)
		name := localID // fallback: raw phone number or email
		if c.chatDB != nil {
			if contact, err := c.chatDB.api.GetContactInfo(localID); err == nil && contact != nil && contact.HasName() {
				name = c.Main.Config.FormatDisplayname(DisplaynameParams{
					FirstName: contact.FirstName,
					LastName:  contact.LastName,
					Nickname:  contact.Nickname,
					ID:        localID,
				})
			}
		}
		chatInfo.Name = &name
		chatInfo.Members = members
	}

	return chatInfo, nil
}

func (c *IMClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	identifier := string(ghost.ID)
	if identifier == "" {
		return nil, nil
	}

	isBot := false
	ui := &bridgev2.UserInfo{
		IsBot:       &isBot,
		Identifiers: []string{identifier},
	}

	// Try contact info from chat.db (Contacts.framework)
	if c.chatDB != nil {
		localID := stripIdentifierPrefix(identifier)
		if contact, err := c.chatDB.api.GetContactInfo(localID); err == nil && contact != nil && contact.HasName() {
			name := c.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				ID:        localID,
			})
			ui.Name = &name
			for _, phone := range contact.Phones {
				ui.Identifiers = append(ui.Identifiers, "tel:"+phone)
			}
			for _, email := range contact.Emails {
				ui.Identifiers = append(ui.Identifiers, "mailto:"+email)
			}
			if len(contact.Avatar) > 0 {
				ui.Avatar = &bridgev2.Avatar{
					ID: networkid.AvatarID(fmt.Sprintf("contact:%s", identifier)),
					Get: func(ctx context.Context) ([]byte, error) {
						return contact.Avatar, nil
					},
				}
			}
			return ui, nil
		}
	}

	// Fallback: format from identifier
	name := c.Main.Config.FormatDisplayname(identifierToDisplaynameParams(identifier))
	ui.Name = &name
	return ui, nil
}

func (c *IMClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	valid := c.client.ValidateTargets([]string{identifier}, c.handle)
	if len(valid) == 0 {
		return nil, fmt.Errorf("user not found on iMessage: %s", identifier)
	}

	userID := makeUserID(identifier)
	portalID := networkid.PortalKey{
		ID:       networkid.PortalID(identifier),
		Receiver: c.UserLogin.ID,
	}

	ghost, err := c.Main.Bridge.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	}
	portal, err := c.Main.Bridge.GetPortalByKey(ctx, portalID)
	if err != nil {
		return nil, fmt.Errorf("failed to get portal: %w", err)
	}
	ghostInfo, err := c.GetUserInfo(ctx, ghost)
	if err != nil {
		return nil, err
	}

	return &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: ghostInfo,
		Chat: &bridgev2.CreateChatResponse{
			Portal:    portal,
			PortalKey: portalID,
		},
	}, nil
}

// ============================================================================
// Backfill (from chat.db when available)
// ============================================================================

func (c *IMClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	if c.chatDB == nil {
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}

	return c.chatDB.FetchMessages(ctx, params, c)
}

// ============================================================================
// State persistence
// ============================================================================

func (c *IMClient) persistState(log zerolog.Logger) {
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if c.connection != nil {
		meta.APSState = c.connection.State().ToString()
	}
	if c.users != nil {
		meta.IDSUsers = c.users.ToString()
	}
	if c.identity != nil {
		meta.IDSIdentity = c.identity.ToString()
	}
	if c.config != nil {
		meta.DeviceID = c.config.GetDeviceId()
	}
	if err := c.UserLogin.Save(context.Background()); err != nil {
		log.Err(err).Msg("Failed to persist state")
	}
}

func (c *IMClient) periodicStateSave(log zerolog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.persistState(log)
			log.Debug().Msg("Periodic state save completed")
		case <-c.stopChan:
			c.persistState(log)
			log.Debug().Msg("Final state save on disconnect")
			return
		}
	}
}

// ============================================================================
// Helpers
// ============================================================================

func (c *IMClient) makeEventSender(sender *string) bridgev2.EventSender {
	if sender == nil || *sender == "" || *sender == c.handle {
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(c.handle),
		}
	}
	return bridgev2.EventSender{
		IsFromMe: false,
		Sender:   makeUserID(*sender),
	}
}

func (c *IMClient) makePortalKey(participants []string, groupName *string) networkid.PortalKey {
	isGroup := len(participants) > 2 || groupName != nil

	if isGroup {
		portalID := strings.Join(participants, ",")
		return networkid.PortalKey{ID: networkid.PortalID(portalID)}
	}

	for _, p := range participants {
		if p != c.handle {
			return networkid.PortalKey{
				ID:       networkid.PortalID(p),
				Receiver: c.UserLogin.ID,
			}
		}
	}

	if len(participants) > 0 {
		return networkid.PortalKey{
			ID:       networkid.PortalID(participants[0]),
			Receiver: c.UserLogin.ID,
		}
	}

	return networkid.PortalKey{ID: "unknown", Receiver: c.UserLogin.ID}
}

func (c *IMClient) makeConversation(participants []string, groupName *string) rustpushgo.WrappedConversation {
	return rustpushgo.WrappedConversation{
		Participants: participants,
		GroupName:    groupName,
	}
}

func (c *IMClient) portalToConversation(portal *bridgev2.Portal) rustpushgo.WrappedConversation {
	portalID := string(portal.ID)
	isSms := c.isPortalSMS(portalID)

	if strings.Contains(portalID, ",") {
		participants := strings.Split(portalID, ",")
		var groupName *string
		if portal.Name != "" {
			groupName = &portal.Name
		}
		return rustpushgo.WrappedConversation{
			Participants: participants,
			GroupName:    groupName,
			IsSms:        isSms,
		}
	}

	return rustpushgo.WrappedConversation{
		Participants: []string{c.handle, portalID},
		IsSms:        isSms,
	}
}

// periodicChatDBSync runs the chat.db health check on a loop. The first
// invocation is the "initial sync" — it creates portals and scans a wide
// window (3 months). Subsequent ticks only look at existing portals and
// backfill any messages that chat.db has but the bridge database doesn't.
func (c *IMClient) periodicChatDBSync(log zerolog.Logger) {
	ctx := log.WithContext(context.Background())

	// Initial sync: create portals for chats with recent activity.
	c.runInitialSync(ctx, log)

	// Then health-check every 5 minutes.
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.runHealthCheck(ctx, log)
		case <-c.stopChan:
			return
		}
	}
}

// runInitialSync creates Matrix portals for recent chats (first login only)
// and then runs a health check to backfill any missing messages.
func (c *IMClient) runInitialSync(ctx context.Context, log zerolog.Logger) {
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if !meta.ChatsSynced {
		minDate := time.Now().Add(-7 * 24 * time.Hour)
		chats, err := c.chatDB.api.GetChatsWithMessagesAfter(minDate)
		if err != nil {
			log.Err(err).Msg("Failed to get chat list for initial sync")
			return
		}

		log.Info().Int("chat_count", len(chats)).Msg("Creating portals for initial chat sync")

		for _, chat := range chats {
			info, err := c.chatDB.api.GetChatInfo(chat.ChatGUID, chat.ThreadID)
			if err != nil || info == nil || info.NoCreateRoom {
				continue
			}

			parsed := imessage.ParseIdentifier(chat.ChatGUID)
			if parsed.LocalID == "" {
				continue
			}

			portalID := identifierToPortalID(parsed)
			portalKey := networkid.PortalKey{
				ID:       portalID,
				Receiver: c.UserLogin.ID,
			}
			if parsed.IsGroup {
				portalKey.Receiver = ""
			}

			chatInfo := c.chatDBInfoToBridgev2(info)

			c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
				EventMeta: simplevent.EventMeta{
					Type:         bridgev2.RemoteEventChatResync,
					PortalKey:    portalKey,
					CreatePortal: true,
					LogContext: func(lc zerolog.Context) zerolog.Context {
						return lc.Str("chat_guid", chat.ChatGUID)
					},
				},
				ChatInfo:        chatInfo,
				GetChatInfoFunc: c.GetChatInfo,
			})
		}

		meta.ChatsSynced = true
		if err := c.UserLogin.Save(ctx); err != nil {
			log.Err(err).Msg("Failed to save metadata after initial sync")
		}
		log.Info().Msg("Initial portal creation complete")
	}

	// Whether first login or not, run a health check with wide window on startup.
	c.runHealthCheckWithWindow(ctx, log, 7*24*time.Hour)
}

// runHealthCheck scans recent chats for messages missing from the bridge.
func (c *IMClient) runHealthCheck(ctx context.Context, log zerolog.Logger) {
	c.runHealthCheckWithWindow(ctx, log, 24*time.Hour)
}

// runHealthCheckWithWindow scans chats with activity in the given window,
// compares message GUIDs between chat.db and the bridge database, and
// if any are missing, nukes the portal and re-creates it from scratch
// so that all messages are inserted in the correct chronological order.
func (c *IMClient) runHealthCheckWithWindow(ctx context.Context, log zerolog.Logger, window time.Duration) {
	if c.chatDB == nil {
		return
	}

	minDate := time.Now().Add(-window)
	chats, err := c.chatDB.api.GetChatsWithMessagesAfter(minDate)
	if err != nil {
		log.Err(err).Msg("Health check: failed to get recent chats from chat.db")
		return
	}

	resynced := 0
	for _, chat := range chats {
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		if parsed.LocalID == "" {
			continue
		}

		portalID := identifierToPortalID(parsed)
		portalKey := networkid.PortalKey{
			ID:       portalID,
			Receiver: c.UserLogin.ID,
		}
		if parsed.IsGroup {
			portalKey.Receiver = ""
		}

		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
		if err != nil {
			continue
		}

		needsResync := false
		if portal == nil || portal.MXID == "" {
			// No portal exists — needs to be created.
			needsResync = true
			log.Info().
				Str("chat_guid", chat.ChatGUID).
				Msg("Health check: no portal exists, creating")
		} else {
			// Portal exists — check for missing messages.
			missing, err := c.chatDB.GetMissingMessages(ctx, chat.ChatGUID, portalKey, c.Main.Bridge, window)
			if err != nil {
				log.Warn().Err(err).Str("chat_guid", chat.ChatGUID).Msg("Health check: failed to get missing messages")
				continue
			}
			if len(missing) > 0 {
				needsResync = true
				log.Info().
					Str("chat_guid", chat.ChatGUID).
					Int("missing_count", len(missing)).
					Str("portal_mxid", string(portal.MXID)).
					Msg("Health check: missing messages detected, nuking and re-creating portal")

				// Delete the Matrix room
				err = c.Main.Bridge.Bot.DeleteRoom(ctx, portal.MXID, false)
				if err != nil {
					log.Err(err).Str("chat_guid", chat.ChatGUID).Msg("Health check: failed to delete Matrix room")
				}

				// Delete portal from bridge database (messages + portal row)
				err = portal.Delete(ctx)
				if err != nil {
					log.Err(err).Str("chat_guid", chat.ChatGUID).Msg("Health check: failed to delete portal from bridge DB")
					continue
				}
			}
		}

		if !needsResync {
			continue
		}

		// Create (or re-create) via ChatResync — framework will create room + backfill in order
		info, err := c.chatDB.api.GetChatInfo(chat.ChatGUID, chat.ThreadID)
		if err != nil || info == nil {
			log.Warn().Err(err).Str("chat_guid", chat.ChatGUID).Msg("Health check: failed to get chat info for re-creation")
			continue
		}
		chatInfo := c.chatDBInfoToBridgev2(info)

		c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("chat_guid", chat.ChatGUID).Str("source", "health_check_resync")
				},
			},
			ChatInfo:        chatInfo,
			GetChatInfoFunc: c.GetChatInfo,
		})

		resynced++
	}

	if resynced > 0 {
		log.Info().Int("portals_resynced", resynced).Int("chats_checked", len(chats)).Msg("Health check complete — nuked and re-created portals with missing messages")
	} else {
		log.Debug().Int("chats_checked", len(chats)).Msg("Health check complete — no missing messages")
	}
}

// chatDBInfoToBridgev2 converts a chat.db ChatInfo to a bridgev2 ChatInfo.
func (c *IMClient) chatDBInfoToBridgev2(info *imessage.ChatInfo) *bridgev2.ChatInfo {
	parsed := imessage.ParseIdentifier(info.JSONChatGUID)
	if parsed.LocalID == "" {
		parsed = info.Identifier
	}

	// Build a display name: use chat.db display_name if set,
	// otherwise build from participant names/numbers.
	displayName := info.DisplayName
	if displayName == "" && parsed.IsGroup {
		displayName = c.buildGroupName(info.Members)
	}

	chatInfo := &bridgev2.ChatInfo{
		Name:        &displayName,
		CanBackfill: true,
	}

	if parsed.IsGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)
		members := &bridgev2.ChatMemberList{
			IsFull:    true,
			MemberMap: make(map[networkid.UserID]bridgev2.ChatMember),
		}
		members.MemberMap[makeUserID(c.handle)] = bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{
				IsFromMe:    true,
				SenderLogin: c.UserLogin.ID,
				Sender:      makeUserID(c.handle),
			},
			Membership: event.MembershipJoin,
		}
		for _, memberID := range info.Members {
			userID := makeUserID(memberID)
			members.MemberMap[userID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{Sender: userID},
				Membership:  event.MembershipJoin,
			}
		}
		chatInfo.Members = members
	} else {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDM)
		otherUser := makeUserID(parsed.LocalID)
		members := &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: otherUser,
			MemberMap: map[networkid.UserID]bridgev2.ChatMember{
				makeUserID(c.handle): {
					EventSender: bridgev2.EventSender{
						IsFromMe:    true,
						SenderLogin: c.UserLogin.ID,
						Sender:      makeUserID(c.handle),
					},
					Membership: event.MembershipJoin,
				},
				otherUser: {
					EventSender: bridgev2.EventSender{Sender: otherUser},
					Membership:  event.MembershipJoin,
				},
			},
		}
		chatInfo.Members = members
	}

	return chatInfo
}

// buildGroupName creates a human-readable group name from member identifiers
// by resolving contact names where possible, falling back to phone/email.
func (c *IMClient) buildGroupName(members []string) string {
	var names []string
	for _, memberID := range members {
		if memberID == c.handle {
			continue // skip self
		}
		name := ""
		if c.chatDB != nil {
			if contact, err := c.chatDB.api.GetContactInfo(memberID); err == nil && contact != nil && contact.HasName() {
				name = c.Main.Config.FormatDisplayname(DisplaynameParams{
					FirstName: contact.FirstName,
					LastName:  contact.LastName,
					Nickname:  contact.Nickname,
					ID:        memberID,
				})
			}
		}
		if name == "" {
			name = memberID // raw phone/email
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "Group Chat"
	}
	if len(names) <= 4 {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, %s, %s +%d more", names[0], names[1], names[2], len(names)-3)
}

// ============================================================================
// Message conversion
// ============================================================================

type attachmentMessage struct {
	*rustpushgo.WrappedMessage
	Attachment *rustpushgo.WrappedAttachment
	Index      int
}

func convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *rustpushgo.WrappedMessage) (*bridgev2.ConvertedMessage, error) {
	text := ptrStringOr(msg.Text, "")
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}
	if msg.Subject != nil && *msg.Subject != "" {
		if text != "" {
			content.Body = fmt.Sprintf("**%s**\n%s", *msg.Subject, text)
			content.Format = event.FormatHTML
			content.FormattedBody = fmt.Sprintf("<strong>%s</strong><br/>%s", *msg.Subject, text)
		} else {
			content.Body = *msg.Subject
		}
	}

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

func convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, attMsg *attachmentMessage) (*bridgev2.ConvertedMessage, error) {
	att := attMsg.Attachment
	msgType := mimeToMsgType(att.MimeType)

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    att.Filename,
		Info: &event.FileInfo{
			MimeType: att.MimeType,
			Size:     int(att.Size),
		},
	}

	if att.IsInline && att.InlineData != nil && intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", *att.InlineData, att.Filename, att.MimeType)
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

// ============================================================================
// Static helpers
// ============================================================================

func tapbackTypeToEmoji(tapbackType *uint32, tapbackEmoji *string) string {
	if tapbackType == nil {
		return "❤️"
	}
	switch *tapbackType {
	case 0:
		return "❤️"
	case 1:
		return "👍"
	case 2:
		return "👎"
	case 3:
		return "😂"
	case 4:
		return "❗"
	case 5:
		return "❓"
	case 6:
		if tapbackEmoji != nil {
			return *tapbackEmoji
		}
		return "👍"
	default:
		return "❤️"
	}
}

func emojiToTapbackType(emoji string) (uint32, *string) {
	switch emoji {
	case "❤️", "♥️":
		return 0, nil
	case "👍":
		return 1, nil
	case "👎":
		return 2, nil
	case "😂":
		return 3, nil
	case "❗", "‼️":
		return 4, nil
	case "❓":
		return 5, nil
	default:
		return 6, &emoji
	}
}

func mimeToUTI(mime string) string {
	switch {
	case mime == "image/jpeg":
		return "public.jpeg"
	case mime == "image/png":
		return "public.png"
	case mime == "image/gif":
		return "com.compuserve.gif"
	case mime == "image/heic":
		return "public.heic"
	case mime == "video/mp4":
		return "public.mpeg-4"
	case mime == "video/quicktime":
		return "com.apple.quicktime-movie"
	case mime == "audio/mpeg", mime == "audio/mp3":
		return "public.mp3"
	case mime == "audio/aac", mime == "audio/mp4":
		return "public.aac-audio"
	case strings.HasPrefix(mime, "image/"):
		return "public.image"
	case strings.HasPrefix(mime, "video/"):
		return "public.movie"
	case strings.HasPrefix(mime, "audio/"):
		return "public.audio"
	default:
		return "public.data"
	}
}

func mimeToMsgType(mime string) event.MessageType {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return event.MsgImage
	case strings.HasPrefix(mime, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(mime, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

func (c *IMClient) markPortalSMS(portalID string) {
	c.smsPortalsLock.Lock()
	defer c.smsPortalsLock.Unlock()
	c.smsPortals[portalID] = true
}

func (c *IMClient) isPortalSMS(portalID string) bool {
	c.smsPortalsLock.RLock()
	defer c.smsPortalsLock.RUnlock()
	return c.smsPortals[portalID]
}

func (c *IMClient) trackUnsend(uuid string) {
	c.recentUnsendsLock.Lock()
	defer c.recentUnsendsLock.Unlock()
	c.recentUnsends[uuid] = time.Now()
	for k, t := range c.recentUnsends {
		if time.Since(t) > 5*time.Minute {
			delete(c.recentUnsends, k)
		}
	}
}

func (c *IMClient) wasUnsent(uuid string) bool {
	c.recentUnsendsLock.Lock()
	defer c.recentUnsendsLock.Unlock()
	if t, ok := c.recentUnsends[uuid]; ok {
		return time.Since(t) < 5*time.Minute
	}
	return false
}

func ptrStringOr(s *string, def string) string {
	if s != nil {
		return *s
	}
	return def
}

func ptrUint64Or(v *uint64, def uint64) uint64 {
	if v != nil {
		return *v
	}
	return def
}
