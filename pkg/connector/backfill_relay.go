package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/lrhodin/imessage/imessage"
)

// backfillRelay proxies chat.db queries to the NAC relay server running on a Mac.
type backfillRelay struct {
	baseURL    string
	httpClient *http.Client
}

// newBackfillRelay creates a backfill relay from the contact relay's base URL.
func newBackfillRelay(baseURL string) *backfillRelay {
	return &backfillRelay{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// RelayMessage mirrors the JSON from the relay's /messages endpoint.
type RelayMessage struct {
	GUID        string            `json:"guid"`
	TimestampMs int64             `json:"timestamp_ms"`
	Subject     string            `json:"subject,omitempty"`
	Text        string            `json:"text"`
	ChatGUID    string            `json:"chat_guid"`
	SenderID    string            `json:"sender_id,omitempty"`
	SenderSvc   string            `json:"sender_service,omitempty"`
	IsFromMe    bool              `json:"is_from_me"`
	IsEmote     bool              `json:"is_emote,omitempty"`
	IsAudio     bool              `json:"is_audio_message,omitempty"`
	ReplyToGUID string            `json:"reply_to_guid,omitempty"`
	ReplyToPart int               `json:"reply_to_part,omitempty"`
	TapbackGUID string            `json:"tapback_guid,omitempty"`
	TapbackType int               `json:"tapback_type,omitempty"`
	GroupTitle   string            `json:"group_title,omitempty"`
	ItemType    int               `json:"item_type"`
	GroupAction int               `json:"group_action_type,omitempty"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Attachments []RelayAttachment `json:"attachments,omitempty"`
	Service     string            `json:"service,omitempty"`
}

// RelayAttachment mirrors the relay's attachment metadata.
type RelayAttachment struct {
	GUID       string `json:"guid"`
	PathOnDisk string `json:"path_on_disk"`
	MimeType   string `json:"mime_type,omitempty"`
	FileName   string `json:"file_name"`
}

// RelayChatInfo mirrors the relay's /chats response.
type RelayChatInfo struct {
	ChatGUID    string   `json:"chat_guid"`
	DisplayName string   `json:"display_name,omitempty"`
	Identifier  string   `json:"identifier"`
	Service     string   `json:"service"`
	Members     []string `json:"members,omitempty"`
	ThreadID    string   `json:"thread_id,omitempty"`
}

// GetChats fetches recent chats from the relay.
func (br *backfillRelay) GetChats(sinceDays int) ([]RelayChatInfo, error) {
	resp, err := br.httpClient.Get(fmt.Sprintf("%s/chats?since_days=%d", br.baseURL, sinceDays))
	if err != nil {
		return nil, fmt.Errorf("relay /chats request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("relay /chats returned %d: %s", resp.StatusCode, body)
	}
	var chats []RelayChatInfo
	if err := json.NewDecoder(resp.Body).Decode(&chats); err != nil {
		return nil, fmt.Errorf("failed to decode /chats response: %w", err)
	}
	return chats, nil
}

// GetMessages fetches messages for a chat GUID from the relay.
func (br *backfillRelay) GetMessages(chatGUID string, sinceTs *int64, beforeTs *int64, limit int) ([]RelayMessage, error) {
	u := fmt.Sprintf("%s/messages?chat_guid=%s", br.baseURL, url.QueryEscape(chatGUID))
	if sinceTs != nil {
		u += fmt.Sprintf("&since_ts=%d", *sinceTs)
	}
	if beforeTs != nil {
		u += fmt.Sprintf("&before_ts=%d", *beforeTs)
	}
	if limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}

	resp, err := br.httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("relay /messages request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("relay /messages returned %d: %s", resp.StatusCode, body)
	}
	var messages []RelayMessage
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		return nil, fmt.Errorf("failed to decode /messages response: %w", err)
	}
	return messages, nil
}

// FetchAttachment downloads attachment data from the relay.
func (br *backfillRelay) FetchAttachment(pathOnDisk string) ([]byte, string, error) {
	resp, err := br.httpClient.Get(fmt.Sprintf("%s/attachment?path=%s", br.baseURL, url.QueryEscape(pathOnDisk)))
	if err != nil {
		return nil, "", fmt.Errorf("relay /attachment request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("relay /attachment returned %d: %s", resp.StatusCode, body)
	}
	contentType := resp.Header.Get("Content-Type")
	data, err := io.ReadAll(resp.Body)
	return data, contentType, err
}

// FetchMessages implements backfill via the relay, mirroring chatDB.FetchMessages.
func (br *backfillRelay) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams, c *IMClient) (*bridgev2.FetchMessagesResponse, error) {
	portalID := string(params.Portal.ID)
	log := zerolog.Ctx(ctx)

	var chatGUIDs []string
	if strings.Contains(portalID, ",") {
		chatGUID := br.findGroupChatGUID(portalID, c)
		if chatGUID != "" {
			chatGUIDs = []string{chatGUID}
		}
	} else {
		chatGUIDs = portalIDToChatGUIDs(portalID)
	}

	log.Info().Str("portal_id", portalID).Strs("chat_guids", chatGUIDs).Bool("forward", params.Forward).Msg("FetchMessages via relay")

	if len(chatGUIDs) == 0 {
		log.Warn().Str("portal_id", portalID).Msg("Could not find chat GUID for portal")
		return &bridgev2.FetchMessagesResponse{HasMore: false, Forward: params.Forward}, nil
	}

	count := params.Count
	if count <= 0 {
		count = 50
	}

	var messages []RelayMessage
	var err error
	var usedGUID string

	for _, chatGUID := range chatGUIDs {
		if params.AnchorMessage != nil {
			ts := params.AnchorMessage.Timestamp.UnixMilli()
			if params.Forward {
				messages, err = br.GetMessages(chatGUID, &ts, nil, 0)
			} else {
				messages, err = br.GetMessages(chatGUID, nil, &ts, count)
			}
		} else {
			days := c.Main.Config.GetInitialSyncDays()
			sinceTs := time.Now().AddDate(0, 0, -days).UnixMilli()
			messages, err = br.GetMessages(chatGUID, &sinceTs, nil, 0)
		}
		if err == nil && len(messages) > 0 {
			usedGUID = chatGUID
			break
		}
	}
	if usedGUID == "" && len(chatGUIDs) > 0 {
		usedGUID = chatGUIDs[0]
	}
	if err != nil {
		log.Error().Err(err).Str("chat_guid", usedGUID).Msg("Failed to fetch messages from relay")
		return nil, fmt.Errorf("failed to fetch messages from relay: %w", err)
	}

	log.Info().Str("chat_guid", usedGUID).Int("raw_message_count", len(messages)).Msg("Got messages from relay")

	intent := c.Main.Bridge.Bot
	backfillMessages := make([]*bridgev2.BackfillMessage, 0, len(messages))

	for _, msg := range messages {
		if msg.ItemType != int(imessage.ItemTypeMessage) || msg.TapbackGUID != "" {
			continue
		}
		// Skip messages with no text and no attachments (empty messages
		// show as "unsupported message" in clients)
		if msg.Text == "" && msg.Subject == "" && len(msg.Attachments) == 0 {
			continue
		}
		sender := relayMakeEventSender(msg, c)
		cm := convertRelayMessage(msg)

		msgTime := time.UnixMilli(msg.TimestampMs)
		backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
			ConvertedMessage: cm,
			Sender:           sender,
			ID:               makeMessageID(msg.GUID),
			TxnID:            networkid.TransactionID(msg.GUID),
			Timestamp:        msgTime,
			StreamOrder:      msg.TimestampMs,
		})

		for i, att := range msg.Attachments {
			attCm, err := br.convertRelayAttachment(ctx, intent, att)
			if err != nil {
				log.Warn().Err(err).Str("guid", msg.GUID).Int("att_index", i).Msg("Failed to convert relay attachment, skipping")
				continue
			}
			partID := fmt.Sprintf("%s_att%d", msg.GUID, i)
			backfillMessages = append(backfillMessages, &bridgev2.BackfillMessage{
				ConvertedMessage: attCm,
				Sender:           sender,
				ID:               makeMessageID(partID),
				TxnID:            networkid.TransactionID(partID),
				Timestamp:        msgTime.Add(time.Duration(i+1) * time.Millisecond),
				StreamOrder:      msg.TimestampMs + int64(i+1),
			})
		}
	}

	return &bridgev2.FetchMessagesResponse{
		Messages:                backfillMessages,
		HasMore:                 len(messages) >= count,
		Forward:                 params.Forward,
		AggressiveDeduplication: params.Forward,
	}, nil
}

// findGroupChatGUID finds a group chat by matching portal members via the relay.
func (br *backfillRelay) findGroupChatGUID(portalID string, c *IMClient) string {
	portalMembers := strings.Split(portalID, ",")
	portalMemberSet := make(map[string]struct{})
	for _, m := range portalMembers {
		portalMemberSet[strings.ToLower(stripIdentifierPrefix(m))] = struct{}{}
	}

	days := c.Main.Config.GetInitialSyncDays()
	chats, err := br.GetChats(days)
	if err != nil {
		return ""
	}

	for _, chat := range chats {
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		if !parsed.IsGroup {
			continue
		}
		chatMemberSet := make(map[string]struct{})
		chatMemberSet[strings.ToLower(stripIdentifierPrefix(c.handle))] = struct{}{}
		for _, m := range chat.Members {
			chatMemberSet[strings.ToLower(stripIdentifierPrefix(m))] = struct{}{}
		}
		if len(chatMemberSet) == len(portalMemberSet) {
			match := true
			for m := range portalMemberSet {
				if _, ok := chatMemberSet[m]; !ok {
					match = false
					break
				}
			}
			if match {
				return chat.ChatGUID
			}
		}
	}
	return ""
}

// runInitialSyncViaRelay performs the initial chat sync using the relay.
func (c *IMClient) runInitialSyncViaRelay(ctx context.Context, log zerolog.Logger) {
	meta := c.UserLogin.Metadata.(*UserLoginMetadata)
	if meta.ChatsSynced {
		log.Info().Msg("Initial sync already completed, skipping")
		return
	}

	days := c.Main.Config.GetInitialSyncDays()
	chats, err := c.backfillRelay.GetChats(days)
	if err != nil {
		log.Err(err).Msg("Failed to get chat list from relay for initial sync")
		return
	}

	type chatEntry struct {
		chatGUID  string
		portalKey networkid.PortalKey
		info      RelayChatInfo
	}
	var entries []chatEntry
	for _, chat := range chats {
		parsed := imessage.ParseIdentifier(chat.ChatGUID)
		if parsed.LocalID == "" {
			continue
		}

		var portalKey networkid.PortalKey
		if parsed.IsGroup {
			members := []string{c.handle}
			for _, m := range chat.Members {
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
			info:      chat,
		})
	}

	// Process oldest-activity first so most recent gets highest stream_ordering
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	log.Info().
		Int("chat_count", len(entries)).
		Int("window_days", days).
		Msg("Initial sync via relay: processing chats sequentially (oldest activity first)")

	synced := 0
	for _, entry := range entries {
		done := make(chan struct{})
		chatInfo := relayChatInfoToBridgev2(entry.info, c)

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
					return lc.Str("chat_guid", chatGUID).Str("source", "relay_initial_sync")
				},
			},
			ChatInfo:        chatInfo,
			LatestMessageTS: time.Now(),
		})

		select {
		case <-done:
			synced++
			if synced%10 == 0 || synced == len(entries) {
				log.Info().Int("progress", synced).Int("total", len(entries)).Msg("Initial sync progress")
			}
		case <-time.After(30 * time.Minute):
			synced++
			log.Warn().Str("chat_guid", entry.chatGUID).Msg("Initial sync: timeout, continuing")
		case <-c.stopChan:
			log.Info().Msg("Initial sync stopped")
			return
		}
	}

	meta.ChatsSynced = true
	if err := c.UserLogin.Save(ctx); err != nil {
		log.Err(err).Msg("Failed to save metadata after initial sync")
	}
	log.Info().Int("synced_chats", synced).Int("total_chats", len(entries)).Msg("Initial sync via relay complete")
}

// relayChatInfoToBridgev2 converts a relay chat info to bridgev2 format.
func relayChatInfoToBridgev2(info RelayChatInfo, c *IMClient) *bridgev2.ChatInfo {
	parsed := imessage.ParseIdentifier(info.ChatGUID)
	chatInfo := &bridgev2.ChatInfo{
		CanBackfill: true,
	}

	if parsed.IsGroup {
		displayName := info.DisplayName
		if displayName == "" {
			displayName = c.buildGroupName(info.Members)
		}
		chatInfo.Name = &displayName
		members := &bridgev2.ChatMemberList{
			IsFull:           true,
			TotalMemberCount: len(info.Members) + 1,
			Members:          make([]bridgev2.ChatMember, 0, len(info.Members)+1),
		}
		// Add self
		members.Members = append(members.Members, bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{
				IsFromMe:    true,
				SenderLogin: c.UserLogin.ID,
				Sender:      makeUserID(c.handle),
			},
		})
		// Add other members
		for _, m := range info.Members {
			memberID := addIdentifierPrefix(m)
			members.Members = append(members.Members, bridgev2.ChatMember{
				EventSender: bridgev2.EventSender{
					Sender: makeUserID(memberID),
				},
			})
		}
		chatInfo.Members = members
	}

	return chatInfo
}

func relayMakeEventSender(msg RelayMessage, c *IMClient) bridgev2.EventSender {
	if msg.IsFromMe {
		return bridgev2.EventSender{
			IsFromMe:    true,
			SenderLogin: c.UserLogin.ID,
			Sender:      makeUserID(c.handle),
		}
	}
	return bridgev2.EventSender{
		IsFromMe: false,
		Sender:   makeUserID(addIdentifierPrefix(msg.SenderID)),
	}
}

func convertRelayMessage(msg RelayMessage) *bridgev2.ConvertedMessage {
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
	}
}

func (br *backfillRelay) convertRelayAttachment(ctx context.Context, intent bridgev2.MatrixAPI, att RelayAttachment) (*bridgev2.ConvertedMessage, error) {
	data, contentType, err := br.FetchAttachment(att.PathOnDisk)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch attachment %s: %w", att.PathOnDisk, err)
	}

	mimeType := att.MimeType
	if mimeType == "" {
		mimeType = contentType
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "application/octet-stream"
	}

	content := &event.MessageEventContent{
		MsgType: mimeToMsgType(mimeType),
		Body:    att.FileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
		},
	}

	if intent != nil {
		url, encFile, err := intent.UploadMedia(ctx, "", data, att.FileName, mimeType)
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

// checkRelayBackfillAvailable probes the relay to see if chat.db endpoints are available.
func (br *backfillRelay) checkAvailable() bool {
	resp, err := br.httpClient.Get(br.baseURL + "/chats?since_days=1")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

// count converts a string to int with a default
func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return def
	}
	return v
}
