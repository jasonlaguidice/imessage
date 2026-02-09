// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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
	handle     string   // Primary iMessage handle used for sending (e.g., tel:+1234567890)
	allHandles []string // All registered handles (for IsThisUser checks)

	// Chat.db supplement (optional — backfill + contacts)
	chatDB *chatDB

	// Contact relay (optional — for Linux with NAC relay)
	contactRelay *contactRelayClient

	// Backfill relay (optional — for Linux with NAC relay + chat.db access)
	backfillRelay *backfillRelay

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

	// Get our handle (precedence: config > login metadata > first handle)
	handles := client.GetHandles()
	c.allHandles = handles
	if len(handles) > 0 {
		c.handle = handles[0]
		preferred := c.Main.Config.PreferredHandle
		if preferred == "" {
			if meta, ok := c.UserLogin.Metadata.(*UserLoginMetadata); ok {
				preferred = meta.PreferredHandle
			}
		}
		if preferred != "" {
			found := false
			for _, h := range handles {
				if h == preferred {
					c.handle = h
					found = true
					break
				}
			}
			if !found {
				log.Warn().Str("preferred", preferred).Strs("available", handles).
					Msg("Preferred handle not found among registered handles, using first available")
			}
		}
	}
	log.Info().Str("selected_handle", c.handle).Strs("handles", handles).Msg("Connected to iMessage")

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
		go c.watchContactChanges(log)
	}

	// Set up contact relay if chat.db isn't available (Linux) and we have a relay URL
	if c.chatDB == nil {
		meta := c.UserLogin.Metadata.(*UserLoginMetadata)
		c.contactRelay = newContactRelayFromKey(meta.HardwareKey)
		if c.contactRelay != nil {
			log.Info().Str("relay", c.contactRelay.baseURL).Msg("Contact relay available for name resolution")
			c.contactRelay.SyncContacts(log)
			go c.periodicContactRelaySync(log)

			// Check if the relay also supports chat.db backfill
			br := newBackfillRelay(c.contactRelay.baseURL)
			if br.checkAvailable() {
				c.backfillRelay = br
				log.Info().Msg("Backfill relay available — chat.db backfill enabled via relay")
				go c.periodicRelaySync(log)
			} else {
				log.Info().Msg("Backfill relay not available (relay may lack Full Disk Access)")
			}
		}
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
	uid := string(userID)
	for _, h := range c.allHandles {
		if uid == h {
			return true
		}
	}
	return false
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

	if msg.Text != nil && *msg.Text != "" && strings.TrimRight(*msg.Text, "\ufffc \n") != "" {
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

	attIndex := 0
	for _, att := range msg.Attachments {
		// Skip rich link sideband attachments (handled in convertMessage)
		if att.MimeType == "x-richlink/meta" || att.MimeType == "x-richlink/image" {
			continue
		}
		attID := msg.Uuid
		if attIndex > 0 || (msg.Text != nil && *msg.Text != "") {
			attID = fmt.Sprintf("%s_att%d", msg.Uuid, attIndex)
		}
		attMsg := &attachmentMessage{
			WrappedMessage: &msg,
			Attachment:     &att,
			Index:          attIndex,
		}
		attIndex++
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
	portalKey := c.makeReceiptPortalKey(msg.Participants, msg.GroupName, msg.Sender)
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
	portalKey := c.makeReceiptPortalKey(msg.Participants, msg.GroupName, msg.Sender)
	ctx := context.Background()

	portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, portalKey)
	if err != nil || portal == nil || portal.MXID == "" {
		return
	}

	msgID := makeMessageID(msg.Uuid)
	dbMessages, err := c.Main.Bridge.DB.Message.GetAllPartsByID(ctx, portal.Receiver, msgID)
	if err != nil || len(dbMessages) == 0 {
		return
	}

	senderUserID := makeUserID(ptrStringOr(msg.Sender, ""))
	ghost, err := c.Main.Bridge.GetGhostByID(ctx, senderUserID)
	if err != nil || ghost == nil {
		return
	}

	for _, dbMsg := range dbMessages {
		c.Main.Bridge.Matrix.SendMessageStatus(ctx, &bridgev2.MessageStatus{
			Status:      event.MessageStatusSuccess,
			DeliveredTo: []id.UserID{ghost.Intent.GetMXID()},
		}, &bridgev2.MessageStatusEventInfo{
			RoomID:        portal.MXID,
			SourceEventID: dbMsg.MXID,
			Sender:        dbMsg.SenderMXID,
		})
	}
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

// convertURLPreviewToIMessage encodes a Beeper link preview (or auto-detected
// URL) into the sideband text prefix that Rust parses for rich link sending.
// Follows the pattern from mautrix-whatsapp's urlpreview.go.
func (c *IMClient) convertURLPreviewToIMessage(ctx context.Context, content *event.MessageEventContent) string {
	log := zerolog.Ctx(ctx)
	body := content.Body

	// Priority 1: Explicit BeeperLinkPreviews from Matrix
	if len(content.BeeperLinkPreviews) > 0 {
		lp := content.BeeperLinkPreviews[0]
		canonical := lp.CanonicalURL
		if canonical == "" {
			canonical = lp.MatchedURL
		}
		log.Debug().
			Str("matched_url", lp.MatchedURL).
			Str("canonical_url", canonical).
			Str("title", lp.Title).
			Msg("Encoding Beeper link preview for iMessage")
		return "\x00RL\x01" + lp.MatchedURL + "\x01" + canonical + "\x01" + lp.Title + "\x01" + lp.Description + "\x00" + body
	}

	// Priority 2: Auto-detect URL and fetch preview from homeserver
	if detectedURL := urlRegex.FindString(body); detectedURL != "" {
		fetchURL := normalizeURL(detectedURL)
		log.Debug().Str("detected_url", detectedURL).Msg("Auto-detected URL in outbound message, fetching preview")
		title, desc := "", ""
		if mc, ok := c.Main.Bridge.Matrix.(bridgev2.MatrixConnectorWithURLPreviews); ok {
			if lp, err := mc.GetURLPreview(ctx, fetchURL); err == nil && lp != nil {
				title = lp.Title
				desc = lp.Description
				log.Debug().Str("title", title).Str("description", desc).Msg("Got URL preview from homeserver for outbound")
			} else if err != nil {
				log.Debug().Err(err).Msg("Failed to fetch URL preview from homeserver for outbound")
			}
		}
		return "\x00RL\x01" + detectedURL + "\x01" + fetchURL + "\x01" + title + "\x01" + desc + "\x00" + body
	}

	return body
}

func (c *IMClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	if c.client == nil {
		return nil, bridgev2.ErrNotLoggedIn
	}

	conv := c.portalToConversation(msg.Portal)

	// File/image messages
	if msg.Content.URL != "" || msg.Content.File != nil {
		return c.handleMatrixFile(ctx, msg, conv)
	}

	textToSend := c.convertURLPreviewToIMessage(ctx, msg.Content)

	uuid, err := c.client.SendMessage(conv, textToSend, c.handle)
	if err != nil {
		return nil, fmt.Errorf("failed to send iMessage: %w", err)
	}

	// If the outbound message has a URL but no link previews from the client,
	// edit the Matrix event to add com.beeper.linkpreviews so Beeper renders them.
	if len(msg.Content.BeeperLinkPreviews) == 0 {
		if detectedURL := urlRegex.FindString(msg.Content.Body); detectedURL != "" {
			go c.addOutboundURLPreview(msg.Event.ID, msg.Portal.MXID, msg.Content.Body, msg.Content.MsgType, detectedURL)
		}
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

// addOutboundURLPreview edits an outbound Matrix event to add com.beeper.linkpreviews
// so Beeper displays a URL preview for messages sent from the client.
func (c *IMClient) addOutboundURLPreview(eventID id.EventID, roomID id.RoomID, body string, msgType event.MessageType, detectedURL string) {
	log := c.UserLogin.Log.With().
		Str("component", "url_preview").
		Stringer("event_id", eventID).
		Str("detected_url", detectedURL).
		Logger()
	ctx := log.WithContext(context.Background())

	intent := c.UserLogin.User.DoublePuppet(ctx)
	if intent == nil {
		log.Debug().Msg("No double puppet available, skipping outbound URL preview edit")
		return
	}

	preview := fetchURLPreview(ctx, c.Main.Bridge, intent, detectedURL)

	editContent := &event.MessageEventContent{
		MsgType:            msgType,
		Body:               body,
		BeeperLinkPreviews: []*event.BeeperLinkPreview{preview},
	}
	editContent.SetEdit(eventID)

	wrappedContent := &event.Content{Parsed: editContent}
	_, err := intent.SendMessage(ctx, roomID, event.EventMessage, wrappedContent, nil)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to send outbound URL preview edit")
	} else {
		log.Debug().Str("title", preview.Title).Msg("Sent outbound URL preview edit")
	}
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

	// Convert OGG Opus voice recordings to CAF Opus for native iMessage playback
	data, mimeType, fileName = convertAudioForIMessage(data, mimeType, fileName)

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
	// Groups use comma-separated participants (e.g., "tel:+15551234567,tel:+15559876543")
	isGroup := strings.Contains(portalID, ",")

	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: c.chatDB != nil || c.backfillRelay != nil,
	}

	if isGroup {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDefault)
		memberList := strings.Split(portalID, ",")
		memberMap := make(map[networkid.UserID]bridgev2.ChatMember)
		for _, member := range memberList {
			userID := makeUserID(member)
			if member == c.handle {
				memberMap[userID] = bridgev2.ChatMember{
					EventSender: bridgev2.EventSender{
						IsFromMe:    true,
						SenderLogin: c.UserLogin.ID,
						Sender:      userID,
					},
					Membership: event.MembershipJoin,
				}
			} else {
				memberMap[userID] = bridgev2.ChatMember{
					EventSender: bridgev2.EventSender{Sender: userID},
					Membership:  event.MembershipJoin,
				}
			}
		}
		chatInfo.Members = &bridgev2.ChatMemberList{
			IsFull:    true,
			MemberMap: memberMap,
		}

		// Build group name from members
		chatInfo.Name = ptr.Ptr(c.buildGroupName(memberList))
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

		// Don't set an explicit room name for DMs. With private_chat_portal_meta
		// enabled, the framework derives the room name from the ghost's profile
		// display name, which means it auto-updates when contacts are edited.
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

	// Try contact info from chat.db (Contacts.framework) or relay
	localID := stripIdentifierPrefix(identifier)
	var contact *imessage.Contact
	if c.chatDB != nil {
		contact, _ = c.chatDB.api.GetContactInfo(localID)
	} else if c.contactRelay != nil {
		contact, _ = c.contactRelay.GetContactInfo(localID)
	}
	if contact != nil && contact.HasName() {
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
			avatarHash := sha256.Sum256(contact.Avatar)
			avatarData := contact.Avatar // capture for closure
			ui.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(fmt.Sprintf("contact:%s:%s", identifier, hex.EncodeToString(avatarHash[:8]))),
				Get: func(ctx context.Context) ([]byte, error) {
					return avatarData, nil
				},
			}
		}
		return ui, nil
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
	if c.chatDB != nil {
		return c.chatDB.FetchMessages(ctx, params, c)
	}
	if c.backfillRelay != nil {
		return c.backfillRelay.FetchMessages(ctx, params, c)
	}
	return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
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

// periodicRelaySync runs initial sync via the backfill relay (once), then idles.
func (c *IMClient) periodicRelaySync(log zerolog.Logger) {
	ctx := log.WithContext(context.Background())
	c.runInitialSyncViaRelay(ctx, log)
	<-c.stopChan
}

// periodicContactRelaySync re-fetches contacts from the relay every 15 minutes.
func (c *IMClient) periodicContactRelaySync(log zerolog.Logger) {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.contactRelay.SyncContacts(log)
		case <-c.stopChan:
			return
		}
	}
}

// ============================================================================
// Contact change watcher
// ============================================================================

// watchContactChanges uses fsnotify to watch the macOS AddressBook database
// directory for writes. When a contact is added, edited, or deleted, macOS
// writes to the .abcddb SQLite files, which we detect and use to trigger a
// full ghost refresh.
func (c *IMClient) watchContactChanges(log zerolog.Logger) {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn().Err(err).Msg("Contact watcher: can't get home dir")
		return
	}

	abDir := filepath.Join(home, "Library", "Application Support", "AddressBook")

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Warn().Err(err).Msg("Contact watcher: failed to create fsnotify watcher")
		return
	}
	defer watcher.Close()

	// Watch the top-level dir and every Sources/<UUID>/ subdirectory,
	// since contacts may live in different account containers.
	if err := watcher.Add(abDir); err != nil {
		log.Warn().Err(err).Str("path", abDir).Msg("Contact watcher: failed to watch AddressBook dir")
		return
	}
	sourcesDir := filepath.Join(abDir, "Sources")
	if entries, err := os.ReadDir(sourcesDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				subdir := filepath.Join(sourcesDir, e.Name())
				if err := watcher.Add(subdir); err != nil {
					log.Warn().Err(err).Str("path", subdir).Msg("Contact watcher: failed to watch subdirectory")
				}
			}
		}
	}

	log.Info().Str("path", abDir).Msg("Watching for macOS contact changes via fsnotify")

	// debounceTimer is nil when idle, non-nil when a change was detected and
	// we're waiting for edits to settle before refreshing.
	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case evt, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Only react to writes/creates on the .abcddb files
			if evt.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			base := filepath.Base(evt.Name)
			if !strings.Contains(base, "abcddb") {
				continue
			}
			// Start or reset the 2s debounce timer
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(2 * time.Second)
				debounceCh = debounceTimer.C
			} else {
				debounceTimer.Reset(2 * time.Second)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Warn().Err(err).Msg("Contact watcher: fsnotify error")

		case <-debounceCh:
			debounceTimer = nil
			debounceCh = nil
			c.refreshAllGhosts(log)

		case <-c.stopChan:
			return
		}
	}
}

// refreshAllGhosts re-resolves contact info for every known ghost and pushes
// any changes (name, avatar, identifiers) to Matrix.
func (c *IMClient) refreshAllGhosts(log zerolog.Logger) {
	ctx := log.WithContext(context.Background())

	// Query all ghost IDs from the bridge database.
	rows, err := c.Main.Bridge.DB.Database.Query(ctx,
		"SELECT id FROM ghost WHERE bridge_id=$1",
		c.Main.Bridge.ID,
	)
	if err != nil {
		log.Err(err).Msg("Contact refresh: failed to query ghost IDs")
		return
	}
	defer rows.Close()
	var ghostIDs []networkid.UserID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Err(err).Msg("Contact refresh: failed to scan ghost ID")
			continue
		}
		ghostIDs = append(ghostIDs, networkid.UserID(id))
	}
	if err := rows.Err(); err != nil {
		log.Err(err).Msg("Contact refresh: row iteration error")
	}

	updated := 0
	for _, ghostID := range ghostIDs {
		ghost, err := c.Main.Bridge.GetGhostByID(ctx, ghostID)
		if err != nil {
			log.Warn().Err(err).Str("ghost_id", string(ghostID)).Msg("Contact refresh: failed to load ghost")
			continue
		}
		info, err := c.GetUserInfo(ctx, ghost)
		if err != nil || info == nil {
			continue
		}
		ghost.UpdateInfo(ctx, info)
		updated++
	}

	log.Info().Int("ghosts_checked", len(ghostIDs)).Int("updated", updated).
		Msg("Contact change detected — refreshed ghost profiles")
}

// ============================================================================
// Helpers
// ============================================================================

func (c *IMClient) isMyHandle(handle string) bool {
	for _, h := range c.allHandles {
		if handle == h {
			return true
		}
	}
	return false
}

func (c *IMClient) makeEventSender(sender *string) bridgev2.EventSender {
	if sender == nil || *sender == "" || c.isMyHandle(*sender) {
		c.ensureDoublePuppet()
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

// ensureDoublePuppet retries double puppet setup if it previously failed.
//
// The mautrix bridgev2 framework permanently caches a nil DoublePuppet() on
// first failure (user.go sets doublePuppetInitialized=true BEFORE calling
// NewUserIntent). On macOS Ventura, transient IDS registration issues can
// cause the initial setup to fail, and without a retry the nil is cached
// forever — making all IsFromMe messages fall through to the ghost intent,
// which flips their direction (sent appears as received).
//
// This workaround detects the cached nil and re-attempts login using the
// saved access token, which succeeds once IDS registration stabilizes.
func (c *IMClient) ensureDoublePuppet() {
	ctx := context.Background()
	user := c.UserLogin.User
	if user.DoublePuppet(ctx) != nil {
		return // already working
	}
	token := user.AccessToken
	if token == "" {
		return // no token to retry with
	}
	user.LogoutDoublePuppet(ctx)
	if err := user.LoginDoublePuppet(ctx, token); err != nil {
		c.UserLogin.Log.Warn().Err(err).Msg("Failed to re-establish double puppet")
	} else {
		c.UserLogin.Log.Info().Msg("Re-established double puppet after previous failure")
	}
}

func (c *IMClient) makePortalKey(participants []string, groupName *string) networkid.PortalKey {
	isGroup := len(participants) > 2 || groupName != nil

	if isGroup {
		// Sort for consistent portal ID regardless of participant order
		sorted := make([]string, len(participants))
		copy(sorted, participants)
		sort.Strings(sorted)
		portalID := strings.Join(sorted, ",")
		return networkid.PortalKey{ID: networkid.PortalID(portalID), Receiver: c.UserLogin.ID}
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

// makeReceiptPortalKey handles receipt messages where participants may be empty.
// When participants is empty (rustpush sets conversation: None for receipts),
// use the sender field to identify the DM portal.
func (c *IMClient) makeReceiptPortalKey(participants []string, groupName *string, sender *string) networkid.PortalKey {
	if len(participants) > 0 {
		return c.makePortalKey(participants, groupName)
	}
	if sender != nil && *sender != "" {
		return networkid.PortalKey{
			ID:       networkid.PortalID(*sender),
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

// periodicChatDBSync runs the initial sync (once) and then idles, keeping
// the goroutine alive so it can be stopped cleanly via stopChan.
func (c *IMClient) periodicChatDBSync(log zerolog.Logger) {
	ctx := log.WithContext(context.Background())

	// Initial sync: create portals for chats with recent activity (first login only).
	c.runInitialSync(ctx, log)

	// Keep goroutine alive for clean shutdown.
	<-c.stopChan
}

// runInitialSync creates portals and backfills messages for all recent chats.
//
// To get correct room ordering in clients (which sort by stream_ordering),
// chats are processed sequentially from oldest-activity to newest-activity.
// Each chat's portal is created and fully backfilled before the next chat
// starts, so the most recently active chat ends up with the highest
// stream_ordering and appears at the top of the room list.
func (c *IMClient) runInitialSync(ctx context.Context, log zerolog.Logger) {
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if meta.ChatsSynced {
		log.Info().Msg("Initial sync already completed, skipping")
		return
	}

	days := c.Main.Config.GetInitialSyncDays()
	minDate := time.Now().AddDate(0, 0, -days)
	chats, err := c.chatDB.api.GetChatsWithMessagesAfter(minDate)
	if err != nil {
		log.Err(err).Msg("Failed to get chat list for initial sync")
		return
	}

	// Build entries with portal keys, filtering out invalid chats.
	type chatEntry struct {
		chatGUID  string
		portalKey networkid.PortalKey
		info      *imessage.ChatInfo
	}
	var entries []chatEntry
	for _, chat := range chats {
		info, err := c.chatDB.api.GetChatInfo(chat.ChatGUID, chat.ThreadID)
		if err != nil || info == nil || info.NoCreateRoom {
			continue
		}
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		if parsed.LocalID == "" {
			continue
		}

		var portalKey networkid.PortalKey
		if parsed.IsGroup {
			// For groups, use comma-separated members (matching rustpush format)
			members := []string{c.handle}
			for _, m := range info.Members {
				members = append(members, addIdentifierPrefix(m))
			}
			sort.Strings(members)
			portalKey = networkid.PortalKey{
				ID:       networkid.PortalID(strings.Join(members, ",")),
				Receiver: c.UserLogin.ID,
			}
		} else {
			portalKey = networkid.PortalKey{
				ID:       identifierToPortalID(parsed),
				Receiver: c.UserLogin.ID,
			}
		}
		entries = append(entries, chatEntry{
			chatGUID:  chat.ChatGUID,
			portalKey: portalKey,
			info:      info,
		})
	}

	// GetChatsWithMessagesAfter returns chats ordered by MAX(message.date)
	// DESC (newest first). Reverse to process oldest-activity first, so the
	// most recent chat gets the highest stream_ordering.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	log.Info().
		Int("chat_count", len(entries)).
		Int("window_days", days).
		Msg("Initial sync: processing chats sequentially (oldest activity first)")

	synced := 0
	for _, entry := range entries {
		done := make(chan struct{})
		chatInfo := c.chatDBInfoToBridgev2(entry.info)

		// Queue a ChatResync event for this chat. The framework will:
		// 1. Create the Matrix room (portal) if it doesn't exist
		// 2. Call doForwardBackfill → FetchMessages → send all messages
		// 3. Call PostHandleFunc to signal completion
		// All within the portal's sequential event loop.
		chatGUID := entry.chatGUID
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    entry.portalKey,
				CreatePortal: true,
				PostHandleFunc: func(ctx context.Context, portal *bridgev2.Portal) {
					close(done)
				},
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("chat_guid", chatGUID).Str("source", "initial_sync")
				},
			},
			ChatInfo:        chatInfo,
			LatestMessageTS: time.Now(),
		})

		// Wait for the chat to be fully processed before starting the next.
		select {
		case <-done:
			synced++
			if synced%10 == 0 || synced == len(entries) {
				log.Info().
					Int("progress", synced).
					Int("total", len(entries)).
					Msg("Initial sync progress")
			}
		case <-time.After(30 * time.Minute):
			synced++
			log.Warn().
				Str("chat_guid", entry.chatGUID).
				Msg("Initial sync: timeout waiting for chat, continuing")
		case <-c.stopChan:
			log.Info().Msg("Initial sync stopped")
			return
		}
	}

	meta.ChatsSynced = true
	if err := c.UserLogin.Save(ctx); err != nil {
		log.Err(err).Msg("Failed to save metadata after initial sync")
	}
	log.Info().
		Int("synced_chats", synced).
		Int("total_chats", len(entries)).
		Int("window_days", days).
		Msg("Initial sync complete")
}

// chatDBInfoToBridgev2 converts a chat.db ChatInfo to a bridgev2 ChatInfo.
func (c *IMClient) chatDBInfoToBridgev2(info *imessage.ChatInfo) *bridgev2.ChatInfo {
	parsed := imessage.ParseIdentifier(info.JSONChatGUID)
	if parsed.LocalID == "" {
		parsed = info.Identifier
	}

	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: true,
	}

	// Only set an explicit room name for group chats. For DMs, the framework
	// derives the room name from the ghost's profile (private_chat_portal_meta),
	// which auto-updates when contacts are edited.
	if parsed.IsGroup {
		displayName := info.DisplayName
		if displayName == "" {
			displayName = c.buildGroupName(info.Members)
		}
		chatInfo.Name = &displayName
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
			userID := makeUserID(addIdentifierPrefix(memberID))
			members.MemberMap[userID] = bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{Sender: userID},
				Membership:  event.MembershipJoin,
			}
		}
		chatInfo.Members = members
	} else {
		chatInfo.Type = ptr.Ptr(database.RoomTypeDM)
		otherUser := makeUserID(addIdentifierPrefix(parsed.LocalID))
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
		// Strip tel:/mailto: prefix for contact lookup
		lookupID := stripIdentifierPrefix(memberID)
		name := ""
		var contact *imessage.Contact
		if c.chatDB != nil {
			contact, _ = c.chatDB.api.GetContactInfo(lookupID)
		} else if c.contactRelay != nil {
			contact, _ = c.contactRelay.GetContactInfo(lookupID)
		}
		if contact != nil && contact.HasName() {
			name = c.Main.Config.FormatDisplayname(DisplaynameParams{
				FirstName: contact.FirstName,
				LastName:  contact.LastName,
				Nickname:  contact.Nickname,
				ID:        lookupID,
			})
		}
		if name == "" {
			name = lookupID // raw phone/email without prefix
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

// convertURLPreviewToBeeper parses rich link sideband attachments from an
// inbound iMessage and returns Beeper link previews. Follows the pattern
// from mautrix-whatsapp's urlpreview.go.
func convertURLPreviewToBeeper(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *rustpushgo.WrappedMessage, bodyText string) []*event.BeeperLinkPreview {
	log := zerolog.Ctx(ctx)

	// Find sideband attachments encoded by Rust
	var rlMeta, rlImage *rustpushgo.WrappedAttachment
	for i := range msg.Attachments {
		switch msg.Attachments[i].MimeType {
		case "x-richlink/meta":
			rlMeta = &msg.Attachments[i]
		case "x-richlink/image":
			rlImage = &msg.Attachments[i]
		}
	}

	if rlMeta != nil && rlMeta.InlineData != nil {
		fields := bytes.SplitN(*rlMeta.InlineData, []byte{0x01}, 5)
		originalURL := string(fields[0])
		canonicalURL := originalURL
		if len(fields) > 1 && len(fields[1]) > 0 {
			canonicalURL = string(fields[1])
		}
		title := ""
		if len(fields) > 2 && len(fields[2]) > 0 {
			title = string(fields[2])
		}
		description := ""
		if len(fields) > 3 && len(fields[3]) > 0 {
			description = string(fields[3])
		}
		imageMime := ""
		if len(fields) > 4 && len(fields[4]) > 0 {
			imageMime = string(fields[4])
		}

		log.Debug().
			Str("original_url", originalURL).
			Str("canonical_url", canonicalURL).
			Str("title", title).
			Str("description", description).
			Str("image_mime", imageMime).
			Msg("Parsed rich link sideband data from iMessage")

		// MatchedURL must exactly match a URL in the body text so Beeper
		// can associate the preview with the inline URL. Use regex to find
		// the URL in the body rather than trusting the NSURL-converted value.
		matchedURL := originalURL
		if bodyURL := urlRegex.FindString(bodyText); bodyURL != "" {
			matchedURL = bodyURL
		}

		preview := &event.BeeperLinkPreview{
			MatchedURL: matchedURL,
			LinkPreview: event.LinkPreview{
				CanonicalURL: canonicalURL,
				Title:        title,
				Description:  description,
			},
		}

		// Upload preview image if available
		if rlImage != nil && rlImage.InlineData != nil && intent != nil {
			if imageMime == "" {
				imageMime = "image/jpeg"
			}
			log.Debug().Int("image_bytes", len(*rlImage.InlineData)).Str("mime", imageMime).Msg("Uploading rich link preview image")
			url, encFile, err := intent.UploadMedia(ctx, "", *rlImage.InlineData, "preview", imageMime)
			if err == nil {
				if encFile != nil {
					preview.ImageEncryption = encFile
					preview.ImageURL = encFile.URL
				} else {
					preview.ImageURL = url
				}
				preview.ImageType = imageMime
			} else {
				log.Warn().Err(err).Msg("Failed to upload rich link preview image")
			}
		}

		log.Debug().Str("matched_url", matchedURL).Str("title", title).Msg("Inbound rich link preview ready")
		return []*event.BeeperLinkPreview{preview}
	}

	// No rich link from iMessage — auto-detect URL and fetch og: metadata + image
	if detectedURL := urlRegex.FindString(bodyText); detectedURL != "" {
		log.Debug().Str("detected_url", detectedURL).Msg("No iMessage rich link, fetching URL preview")
		return []*event.BeeperLinkPreview{fetchURLPreview(ctx, portal.Bridge, intent, detectedURL)}
	}

	return nil
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

	content.BeeperLinkPreviews = convertURLPreviewToBeeper(ctx, portal, intent, msg, text)

	return &bridgev2.ConvertedMessage{
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: content,
		}},
	}, nil
}

func convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, attMsg *attachmentMessage) (*bridgev2.ConvertedMessage, error) {
	att := attMsg.Attachment
	mimeType := att.MimeType
	fileName := att.Filename
	var durationMs int

	// Convert CAF Opus voice messages to OGG Opus for Matrix clients
	var inlineData []byte
	if att.IsInline && att.InlineData != nil {
		inlineData = *att.InlineData
		if att.UtiType == "com.apple.coreaudio-format" || mimeType == "audio/x-caf" {
			inlineData, mimeType, fileName, durationMs = convertAudioForMatrix(inlineData, mimeType, fileName)
		}
	}

	msgType := mimeToMsgType(mimeType)

	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     int(att.Size),
		},
	}

	// Mark as voice message if this was a CAF voice recording
	if durationMs > 0 {
		content.MSC3245Voice = &event.MSC3245Voice{}
		content.MSC1767Audio = &event.MSC1767Audio{
			Duration: durationMs,
		}
		content.Info.Size = len(inlineData)
	}

	if inlineData != nil && intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", inlineData, fileName, mimeType)
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
	case mime == "audio/x-caf":
		return "com.apple.coreaudio-format"
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

// urlRegex matches URLs in message text for rich link matching.
// Matches explicit schemes (https://...) and bare domains (example.com, example.com/path).
var urlRegex = regexp.MustCompile(`(?:https?://\S+|(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}(?:/\S*)?)`)

// normalizeURL ensures a URL has a scheme for HTTP fetching.
func normalizeURL(u string) string {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "https://" + u
	}
	return u
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
