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

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// RustpushClient implements bridgev2.NetworkAPI using the rustpush iMessage protocol library.
type RustpushClient struct {
	Main      *IMConnector
	UserLogin *bridgev2.UserLogin
	client    *rustpushgo.Client

	config     *rustpushgo.WrappedOsConfig
	users      *rustpushgo.WrappedIdsUsers
	identity   *rustpushgo.WrappedIdsngmIdentity
	connection *rustpushgo.WrappedApsConnection

	handle string // The iMessage handle (e.g., tel:+1234567890 or mailto:user@example.com)

	// stopChan is closed when the client disconnects to stop background goroutines.
	stopChan chan struct{}

	// recentUnsends tracks message UUIDs that were recently unsent, to prevent
	// re-delivery of the same message after APNs replays it.
	recentUnsends     map[string]time.Time
	recentUnsendsLock sync.Mutex
}

var _ bridgev2.NetworkAPI = (*RustpushClient)(nil)
var _ bridgev2.EditHandlingNetworkAPI = (*RustpushClient)(nil)
var _ bridgev2.ReactionHandlingNetworkAPI = (*RustpushClient)(nil)
var _ bridgev2.ReadReceiptHandlingNetworkAPI = (*RustpushClient)(nil)
var _ bridgev2.TypingHandlingNetworkAPI = (*RustpushClient)(nil)
var _ bridgev2.IdentifierResolvingNetworkAPI = (*RustpushClient)(nil)
var _ rustpushgo.MessageCallback = (*RustpushClient)(nil)
var _ rustpushgo.UpdateUsersCallback = (*RustpushClient)(nil)

// loadRustpushLogin is called by IMConnector.LoadUserLogin for rustpush platform logins.
func (c *IMConnector) loadRustpushLogin(ctx context.Context, login *bridgev2.UserLogin, meta *UserLoginMetadata) error {
	// Initialize the Rust logger and keystore (must happen before any rustpush calls)
	rustpushgo.InitLogger()

	var cfg *rustpushgo.WrappedOsConfig
	var err error

	if meta.Platform == "rustpush-local" {
		// Use local macOS config (IOKit + NAC, no relay)
		if meta.DeviceID != "" {
			cfg, err = rustpushgo.CreateLocalMacosConfigWithDeviceId(meta.DeviceID)
		} else {
			cfg, err = rustpushgo.CreateLocalMacosConfig()
		}
		if err != nil {
			return fmt.Errorf("failed to create local macOS config: %w", err)
		}
	} else {
		// Relay path
		if meta.RelayCode == "" {
			return fmt.Errorf("rustpush login missing relay code")
		}
		cfg, err = rustpushgo.CreateRelayConfig(meta.RelayCode)
		if err != nil {
			return fmt.Errorf("failed to create relay config: %w", err)
		}
	}

	usersStr := &meta.IDSUsers
	identityStr := &meta.IDSIdentity
	apsStateStr := &meta.APSState

	client := &RustpushClient{
		Main:          c,
		UserLogin:     login,
		config:        cfg,
		users:         rustpushgo.NewWrappedIdsUsers(usersStr),
		identity:      rustpushgo.NewWrappedIdsngmIdentity(identityStr),
		recentUnsends: make(map[string]time.Time),
	}

	// Create APS connection with saved state
	client.connection = rustpushgo.Connect(cfg, rustpushgo.NewWrappedApsState(apsStateStr))

	login.Client = client
	return nil
}

// ============================================================================
// bridgev2.NetworkAPI implementation
// ============================================================================

func (c *RustpushClient) Connect(ctx context.Context) {
	log := c.UserLogin.Log.With().Str("component", "rustpush").Logger()
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
	log.Info().Strs("handles", handles).Msg("Rustpush client connected")

	// Save updated APS state (connection tokens, IDS keys, device ID)
	c.persistState(log)
	log.Debug().Msg("Persisted rustpush state after connect")

	// Start periodic state saver (every 5 minutes) to capture APS token refreshes
	c.stopChan = make(chan struct{})
	go c.periodicStateSave(log)

	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
}

func (c *RustpushClient) Disconnect() {
	if c.stopChan != nil {
		close(c.stopChan)
		c.stopChan = nil
	}
	if c.client != nil {
		c.client.Stop()
		c.client.Destroy()
		c.client = nil
	}
}

// periodicStateSave persists APS state every 5 minutes to capture token
// refreshes and other state changes that happen during normal operation.
func (c *RustpushClient) periodicStateSave(log zerolog.Logger) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.persistState(log)
			log.Debug().Msg("Periodic state save completed")
		case <-c.stopChan:
			// Final save on disconnect
			c.persistState(log)
			log.Debug().Msg("Final state save on disconnect")
			return
		}
	}
}

func (c *RustpushClient) IsLoggedIn() bool {
	return c.client != nil
}

func (c *RustpushClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *RustpushClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return string(userID) == c.handle
}

func (c *RustpushClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	if portal.RoomType == database.RoomTypeDM {
		return rustpushCapsDM
	}
	return rustpushCaps
}

// ============================================================================
// Callbacks from rustpush
// ============================================================================

// OnMessage is called by rustpush when a message is received.
func (c *RustpushClient) OnMessage(msg rustpushgo.WrappedMessage) {
	log := c.UserLogin.Log.With().
		Str("component", "rustpush").
		Str("msg_uuid", msg.Uuid).
		Logger()

	// Send delivery receipt if requested
	if msg.SendDelivered && msg.Sender != nil && !msg.IsDelivered && !msg.IsReadReceipt {
		go func() {
			conv := c.makeConversation(msg.Participants, msg.GroupName)
			if err := c.client.SendReadReceipt(conv, c.handle); err != nil {
				log.Warn().Err(err).Msg("Failed to send delivery receipt")
			}
		}()
	}

	// Skip non-payload messages
	if msg.IsDelivered {
		return
	}

	// Handle read receipts
	if msg.IsReadReceipt {
		c.handleReadReceipt(log, msg)
		return
	}

	// Handle typing
	if msg.IsTyping {
		c.handleTyping(log, msg, true)
		return
	}

	// Handle errors
	if msg.IsError {
		log.Warn().
			Str("for_uuid", ptrStringOr(msg.ErrorForUuid, "")).
			Uint64("status", ptrUint64Or(msg.ErrorStatus, 0)).
			Str("status_str", ptrStringOr(msg.ErrorStatusStr, "")).
			Msg("Received iMessage error")
		return
	}

	// Handle peer cache invalidate
	if msg.IsPeerCacheInvalidate {
		log.Debug().Msg("Peer cache invalidated")
		return
	}

	// Handle tapbacks
	if msg.IsTapback {
		c.handleTapback(log, msg)
		return
	}

	// Handle edits
	if msg.IsEdit {
		c.handleEdit(log, msg)
		return
	}

	// Handle unsends
	if msg.IsUnsend {
		c.handleUnsend(log, msg)
		return
	}

	// Handle renames
	if msg.IsRename {
		c.handleRename(log, msg)
		return
	}

	// Handle participant changes
	if msg.IsParticipantChange {
		c.handleParticipantChange(log, msg)
		return
	}

	// Handle regular messages
	c.handleMessage(log, msg)
}

// UpdateUsers is called when IDS keys are refreshed.
func (c *RustpushClient) UpdateUsers(users *rustpushgo.WrappedIdsUsers) {
	log := c.UserLogin.Log.With().Str("component", "rustpush").Logger()
	c.users = users

	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	meta.IDSUsers = users.ToString()
	if err := c.UserLogin.Save(context.Background()); err != nil {
		log.Err(err).Msg("Failed to save updated IDS users")
	}
	log.Debug().Msg("IDS users updated and saved")
}

// ============================================================================
// Message handling
// ============================================================================

func (c *RustpushClient) handleMessage(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	// Suppress re-delivery of messages that were already unsent.
	if c.wasUnsent(msg.Uuid) {
		log.Debug().Str("uuid", msg.Uuid).Msg("Suppressing re-delivery of unsent message")
		return
	}

	sender := c.makeEventSender(msg.Sender)
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)

	// Text message
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
					return lc.Str("rp_uuid", msg.Uuid)
				},
			},
			Data:               &msg,
			ID:                 makeMessageID(msg.Uuid),
			ConvertMessageFunc: convertRustpushMessage,
		})
	}

	// Attachments
	for i, att := range msg.Attachments {
		attID := msg.Uuid
		if i > 0 || (msg.Text != nil && *msg.Text != "") {
			attID = fmt.Sprintf("%s_att%d", msg.Uuid, i)
		}
		attMsg := &rustpushAttachment{
			WrappedMessage: &msg,
			Attachment:     &att,
			Index:          i,
		}
		c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[*rustpushAttachment]{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventMessage,
				PortalKey:    portalKey,
				CreatePortal: true,
				Sender:       sender,
				Timestamp:    time.UnixMilli(int64(msg.TimestampMs)),
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("rp_uuid", attID)
				},
			},
			Data:               attMsg,
			ID:                 makeMessageID(attID),
			ConvertMessageFunc: convertRustpushAttachment,
		})
	}
}

type rustpushAttachment struct {
	*rustpushgo.WrappedMessage
	Attachment *rustpushgo.WrappedAttachment
	Index      int
}

func convertRustpushMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *rustpushgo.WrappedMessage) (*bridgev2.ConvertedMessage, error) {
	text := ""
	if msg.Text != nil {
		text = *msg.Text
	}
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
			ID:      networkid.PartID(""),
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

func convertRustpushAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, attMsg *rustpushAttachment) (*bridgev2.ConvertedMessage, error) {
	att := attMsg.Attachment

	msgType := event.MsgFile
	if strings.HasPrefix(att.MimeType, "image/") {
		msgType = event.MsgImage
	} else if strings.HasPrefix(att.MimeType, "video/") {
		msgType = event.MsgVideo
	} else if strings.HasPrefix(att.MimeType, "audio/") {
		msgType = event.MsgAudio
	}

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    att.Filename,
		Info: &event.FileInfo{
			MimeType: att.MimeType,
			Size:     int(att.Size),
		},
	}

	// Upload inline attachments to Matrix
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

func (c *RustpushClient) handleTapback(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	sender := c.makeEventSender(msg.Sender)
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)

	targetGUID := ""
	if msg.TapbackTargetUuid != nil {
		targetGUID = *msg.TapbackTargetUuid
	}

	emoji := tapbackTypeToEmoji(msg.TapbackType, msg.TapbackEmoji)

	evtType := bridgev2.RemoteEventReaction
	if msg.TapbackRemove {
		evtType = bridgev2.RemoteEventReactionRemove
	}

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Reaction{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		TargetMessage: makeMessageID(targetGUID),
		EmojiID:       "",
		Emoji:         emoji,
	})
}

func (c *RustpushClient) handleEdit(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	sender := c.makeEventSender(msg.Sender)
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)

	targetGUID := ""
	if msg.EditTargetUuid != nil {
		targetGUID = *msg.EditTargetUuid
	}
	newText := ""
	if msg.EditNewText != nil {
		newText = *msg.EditNewText
	}

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: portalKey,
			Sender:    sender,
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

func (c *RustpushClient) handleUnsend(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	sender := c.makeEventSender(msg.Sender)
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)

	targetGUID := ""
	if msg.UnsendTargetUuid != nil {
		targetGUID = *msg.UnsendTargetUuid
	}

	// Track this unsend so we can suppress re-delivery of the same message UUID.
	c.trackUnsend(targetGUID)

	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.MessageRemove{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessageRemove,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		TargetMessage: makeMessageID(targetGUID),
	})
}

func (c *RustpushClient) handleRename(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	newName := ""
	if msg.NewChatName != nil {
		newName = *msg.NewChatName
	}

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

func (c *RustpushClient) handleParticipantChange(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventChatResync,
			PortalKey: portalKey,
		},
		GetChatInfoFunc: c.GetChatInfo,
	})
}

func (c *RustpushClient) handleReadReceipt(log zerolog.Logger, msg rustpushgo.WrappedMessage) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)

	sender := c.makeEventSender(msg.Sender)
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
	})
}

func (c *RustpushClient) handleTyping(log zerolog.Logger, msg rustpushgo.WrappedMessage, typing bool) {
	portalKey := c.makePortalKey(msg.Participants, msg.GroupName)
	sender := c.makeEventSender(msg.Sender)

	evtType := bridgev2.RemoteEventTyping
	c.Main.Bridge.QueueRemoteEvent(c.UserLogin, &simplevent.Typing{
		EventMeta: simplevent.EventMeta{
			Type:      evtType,
			PortalKey: portalKey,
			Sender:    sender,
			Timestamp: time.UnixMilli(int64(msg.TimestampMs)),
		},
		Timeout: 60 * time.Second,
	})
}

// ============================================================================
// Matrix→iMessage handling
// ============================================================================

func (c *RustpushClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)

	// Check if this is a file/image message
	if msg.Content.URL != "" || msg.Content.File != nil {
		return c.handleMatrixFile(ctx, msg, conv)
	}

	text := msg.Content.Body
	uuid, err := c.client.SendMessage(conv, text, c.handle)
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

func (c *RustpushClient) handleMatrixFile(ctx context.Context, msg *bridgev2.MatrixMessage, conv rustpushgo.WrappedConversation) (*bridgev2.MatrixMessageResponse, error) {
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

	// UTI type (basic mapping)
	utiType := mimeToUTI(mimeType)

	uuid, err := c.client.SendAttachment(conv, data, mimeType, utiType, fileName, c.handle)
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

func (c *RustpushClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	if c.client == nil {
		return nil
	}
	conv := c.portalToConversation(msg.Portal)
	return c.client.SendTyping(conv, msg.IsTyping, c.handle)
}

func (c *RustpushClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	if c.client == nil {
		return nil
	}
	conv := c.portalToConversation(receipt.Portal)
	return c.client.SendReadReceipt(conv, c.handle)
}

func (c *RustpushClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	targetGUID := string(msg.EditTarget.ID)

	newText := msg.Content.Body

	_, err := c.client.SendEdit(conv, targetGUID, 0, newText, c.handle)
	if err == nil {
		// Work around mautrix-go bridgev2 not incrementing EditCount before saving.
		msg.EditTarget.EditCount++
	}
	return err
}

func (c *RustpushClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	targetGUID := string(msg.TargetMessage.ID)

	_, err := c.client.SendUnsend(conv, targetGUID, 0, c.handle)
	return err
}

func (c *RustpushClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	return bridgev2.MatrixReactionPreResponse{
		SenderID: makeUserID(c.handle),
		EmojiID:  networkid.EmojiID(""),
		Emoji:    msg.Content.RelatesTo.Key,
	}, nil
}

func (c *RustpushClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	targetGUID := string(msg.TargetMessage.ID)

	reaction, emoji := emojiToTapbackType(msg.Content.RelatesTo.Key)

	_, err := c.client.SendTapback(conv, targetGUID, 0, reaction, emoji, false, c.handle)
	if err != nil {
		return nil, fmt.Errorf("failed to send tapback: %w", err)
	}

	return &database.Reaction{
		MessageID: msg.TargetMessage.ID,
		SenderID:  makeUserID(c.handle),
		EmojiID:   "",
		Emoji:     msg.Content.RelatesTo.Key,
		Metadata: &MessageMetadata{},
		MXID:     msg.Event.ID,
	}, nil
}

func (c *RustpushClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	if c.client == nil {
		return bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)
	targetGUID := string(msg.TargetReaction.MessageID)
	reaction, emoji := emojiToTapbackType(msg.TargetReaction.Emoji)

	_, err := c.client.SendTapback(conv, targetGUID, 0, reaction, emoji, true, c.handle)
	return err
}

// ============================================================================
// Chat/user info
// ============================================================================

func (c *RustpushClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	// For rustpush, we construct chat info from the portal ID
	portalID := string(portal.ID)
	isGroup := strings.Contains(portalID, ";+;")

	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: false, // rustpush has no local history
	}

	if isGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)
		// We don't have full member info without the mac connector,
		// but we can add self
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
					EventSender: bridgev2.EventSender{
						Sender: otherUser,
					},
					Membership: event.MembershipJoin,
				},
			},
		}
		chatInfo.Members = members
	}

	return chatInfo, nil
}

func (c *RustpushClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	identifier := string(ghost.ID)
	if identifier == "" {
		return nil, nil
	}

	isBot := false
	ui := &bridgev2.UserInfo{
		IsBot:       &isBot,
		Identifiers: []string{identifier},
	}

	// Use the identifier as display name since rustpush doesn't have a contacts DB
	name := c.Main.Config.FormatDisplayname(DisplaynameParams{
		ID: identifier,
	})

	if strings.HasPrefix(identifier, "+") || strings.HasPrefix(identifier, "tel:") {
		phone := strings.TrimPrefix(identifier, "tel:")
		name = c.Main.Config.FormatDisplayname(DisplaynameParams{
			Phone: phone,
			ID:    identifier,
		})
		if !strings.HasPrefix(identifier, "tel:") {
			ui.Identifiers = append(ui.Identifiers, "tel:"+identifier)
		}
	} else if strings.Contains(identifier, "@") {
		email := strings.TrimPrefix(identifier, "mailto:")
		name = c.Main.Config.FormatDisplayname(DisplaynameParams{
			Email: email,
			ID:    identifier,
		})
	}

	ui.Name = &name
	return ui, nil
}

func (c *RustpushClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	// Validate the target exists on iMessage
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
// Backfill (not supported for rustpush — no local history)
// ============================================================================

func (c *RustpushClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	return &bridgev2.FetchMessagesResponse{
		Messages: nil,
		HasMore:  false,
		Forward:  params.Forward,
	}, nil
}

// ============================================================================
// Helpers
// ============================================================================

func (c *RustpushClient) makeEventSender(sender *string) bridgev2.EventSender {
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

func (c *RustpushClient) makePortalKey(participants []string, groupName *string) networkid.PortalKey {
	isGroup := len(participants) > 2 || groupName != nil

	if isGroup {
		// For groups, use all participants sorted as the portal ID
		portalID := strings.Join(participants, ",")
		return networkid.PortalKey{
			ID: networkid.PortalID(portalID),
		}
	}

	// For DMs, find the other participant
	for _, p := range participants {
		if p != c.handle {
			return networkid.PortalKey{
				ID:       networkid.PortalID(p),
				Receiver: c.UserLogin.ID,
			}
		}
	}

	// Fallback: use first participant
	if len(participants) > 0 {
		return networkid.PortalKey{
			ID:       networkid.PortalID(participants[0]),
			Receiver: c.UserLogin.ID,
		}
	}

	return networkid.PortalKey{
		ID:       "unknown",
		Receiver: c.UserLogin.ID,
	}
}

func (c *RustpushClient) makeConversation(participants []string, groupName *string) rustpushgo.WrappedConversation {
	return rustpushgo.WrappedConversation{
		Participants: participants,
		GroupName:    groupName,
		SenderGuid:   nil,
	}
}

func (c *RustpushClient) portalToConversation(portal *bridgev2.Portal) rustpushgo.WrappedConversation {
	portalID := string(portal.ID)

	// Check if this is a group chat (has comma-separated participants)
	if strings.Contains(portalID, ",") {
		participants := strings.Split(portalID, ",")
		var groupName *string
		if portal.Name != "" {
			groupName = &portal.Name
		}
		return rustpushgo.WrappedConversation{
			Participants: participants,
			GroupName:    groupName,
			SenderGuid:   nil,
		}
	}

	// DM: participants are self + other
	return rustpushgo.WrappedConversation{
		Participants: []string{c.handle, portalID},
		GroupName:    nil,
		SenderGuid:   nil,
	}
}

func (c *RustpushClient) persistState(log zerolog.Logger) {
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
		log.Err(err).Msg("Failed to persist rustpush state")
	}
}

// Tapback helpers

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

// trackUnsend records a message UUID as unsent so re-deliveries are suppressed.
func (c *RustpushClient) trackUnsend(uuid string) {
	c.recentUnsendsLock.Lock()
	defer c.recentUnsendsLock.Unlock()
	c.recentUnsends[uuid] = time.Now()
	// Prune entries older than 5 minutes to avoid unbounded growth.
	for k, t := range c.recentUnsends {
		if time.Since(t) > 5*time.Minute {
			delete(c.recentUnsends, k)
		}
	}
}

// wasUnsent checks if a message UUID was recently unsent.
func (c *RustpushClient) wasUnsent(uuid string) bool {
	c.recentUnsendsLock.Lock()
	defer c.recentUnsendsLock.Unlock()
	if t, ok := c.recentUnsends[uuid]; ok {
		// Keep blocking re-delivery for 5 minutes after unsend.
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
