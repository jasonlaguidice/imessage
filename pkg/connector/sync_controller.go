package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// No periodic polling needed: real-time messages arrive via APNs push
// on com.apple.madrid. CloudKit sync is only used for initial backfill
// of historical messages (bootstrap).

type cloudSyncCounters struct {
	Imported int
	Updated  int
	Skipped  int
	Deleted  int
}

func (c *cloudSyncCounters) add(other cloudSyncCounters) {
	c.Imported += other.Imported
	c.Updated += other.Updated
	c.Skipped += other.Skipped
	c.Deleted += other.Deleted
}

func (c *IMClient) setContactsReady(log zerolog.Logger) {
	firstTime := false
	c.contactsReadyLock.Lock()
	if !c.contactsReady {
		c.contactsReady = true
		firstTime = true
		readyCh := c.contactsReadyCh
		c.contactsReadyLock.Unlock()
		if readyCh != nil {
			close(readyCh)
		}
		log.Info().Msg("Contacts readiness gate satisfied")
	} else {
		c.contactsReadyLock.Unlock()
	}

	// Re-resolve ghost and group names from contacts on every sync,
	// not just the first time. Contacts may have been added/edited in iCloud.
	if firstTime {
		log.Info().Msg("Running initial contact name resolution for ghosts and group portals")
	} else {
		log.Info().Msg("Re-syncing contact names for ghosts and group portals")
	}
	go c.refreshGhostNamesFromContacts(log)
	go c.refreshGroupPortalNamesFromContacts(log)
}

func (c *IMClient) refreshGhostNamesFromContacts(log zerolog.Logger) {
	if c.contacts == nil {
		return
	}
	ctx := context.Background()

	// Get all ghost IDs from the database via the raw DB handle
	rows, err := c.Main.Bridge.DB.RawDB.QueryContext(ctx, "SELECT id, name FROM ghost")
	if err != nil {
		log.Err(err).Msg("Failed to query ghosts for contact name refresh")
		return
	}
	defer rows.Close()

	updated := 0
	total := 0
	for rows.Next() {
		var ghostID, ghostName string
		if err := rows.Scan(&ghostID, &ghostName); err != nil {
			continue
		}
		total++
		localID := stripIdentifierPrefix(ghostID)
		if localID == "" {
			continue
		}
		contact, _ := c.contacts.GetContactInfo(localID)
		if contact == nil || !contact.HasName() {
			continue
		}
		name := c.Main.Config.FormatDisplayname(DisplaynameParams{
			FirstName: contact.FirstName,
			LastName:  contact.LastName,
			Nickname:  contact.Nickname,
			ID:        localID,
		})
		if ghostName != name {
			ghost, err := c.Main.Bridge.GetGhostByID(ctx, networkid.UserID(ghostID))
			if err != nil || ghost == nil {
				continue
			}
			ghost.UpdateInfo(ctx, &bridgev2.UserInfo{Name: &name})
			updated++
		}
	}
	log.Info().Int("updated", updated).Int("total", total).Msg("Refreshed ghost names from contacts")
}

// refreshGroupPortalNamesFromContacts re-resolves group portal names using
// contact data. Portals created before contacts loaded may have raw phone
// numbers / email addresses as the room name. This also picks up contact
// edits on subsequent periodic syncs.
func (c *IMClient) refreshGroupPortalNamesFromContacts(log zerolog.Logger) {
	if c.contacts == nil {
		return
	}
	ctx := context.Background()

	portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to load portals for group name refresh")
		return
	}

	updated := 0
	total := 0
	for _, portal := range portals {
		if portal.Receiver != c.UserLogin.ID {
			continue
		}
		portalID := string(portal.ID)
		isGroup := strings.HasPrefix(portalID, "gid:") || strings.Contains(portalID, ",")
		if !isGroup {
			continue
		}
		total++

		newName := c.resolveGroupName(ctx, portalID)
		if newName == "" || newName == portal.Name {
			continue
		}

		c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventChatInfoChange,
				PortalKey: networkid.PortalKey{
					ID:       portal.ID,
					Receiver: c.UserLogin.ID,
				},
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("portal_id", portalID).Str("source", "group_name_refresh")
				},
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{
				ChatInfo: &bridgev2.ChatInfo{
					Name: &newName,
				},
			},
		})
		updated++
	}
	log.Info().Int("updated", updated).Int("total_groups", total).Msg("Refreshed group portal names from contacts")
}

func (c *IMClient) waitForContactsReady(log zerolog.Logger) bool {
	c.contactsReadyLock.RLock()
	alreadyReady := c.contactsReady
	readyCh := c.contactsReadyCh
	c.contactsReadyLock.RUnlock()
	if alreadyReady {
		return true
	}

	log.Info().Msg("Waiting for contacts readiness gate before CloudKit sync")
	select {
	case <-readyCh:
		log.Info().Msg("Contacts readiness gate opened")
		return true
	case <-c.stopChan:
		return false
	}
}

func (c *IMClient) startCloudSyncController(log zerolog.Logger) {
	if c.cloudStore == nil || c.client == nil {
		return
	}
	go c.runCloudSyncController(log.With().Str("component", "cloud_sync").Logger())
}

func (c *IMClient) runCloudSyncController(log zerolog.Logger) {
	ctx := context.Background()
	controllerStart := time.Now()
	if !c.waitForContactsReady(log) {
		return
	}
	log.Info().Dur("contacts_wait", time.Since(controllerStart)).Msg("Contacts ready, proceeding with CloudKit sync")

	// Detect a fresh start: if the bridge has no portals for this login,
	// the DB was reset. Clear stale cloud cache tables and sync tokens so
	// the bootstrap does a full sync from scratch. We check bridge portals
	// (not cloud_message) because cloud_* tables survive a bridge DB reset.
	isFresh := false
	if portals, err := c.Main.Bridge.GetAllPortalsWithMXID(ctx); err == nil {
		hasOwnPortal := false
		for _, p := range portals {
			if p.Receiver == c.UserLogin.ID {
				hasOwnPortal = true
				break
			}
		}
		isFresh = !hasOwnPortal
	}

	if isFresh {
		if err := c.cloudStore.clearAllData(ctx); err != nil {
			log.Warn().Err(err).Msg("Failed to clear stale cloud data")
		} else {
			log.Info().Msg("Fresh database detected, cleared cloud cache and sync tokens for full bootstrap")
		}
	}

	log.Info().Bool("incremental", !isFresh).Msg("CloudKit bootstrap sync start (historical backfill)")

	backfillStart := time.Now()
	counts, err := c.runCloudKitBackfill(ctx, log)
	if err != nil {
		log.Error().Err(err).Dur("elapsed", time.Since(backfillStart)).Msg("CloudKit bootstrap sync failed")
	} else {
		log.Info().
			Int("imported", counts.Imported).
			Int("updated", counts.Updated).
			Int("skipped", counts.Skipped).
			Int("deleted", counts.Deleted).
			Dur("elapsed", time.Since(backfillStart)).
			Msg("CloudKit bootstrap sync complete — now creating portals (triggers forward backfill for each)")
	}

	portalStart := time.Now()
	c.createPortalsFromCloudSync(ctx, log)
	log.Info().
		Dur("portal_creation_elapsed", time.Since(portalStart)).
		Dur("total_elapsed", time.Since(controllerStart)).
		Msg("CloudKit backfill pipeline complete — backward backfill will run asynchronously via framework queue; real-time messages via APNs")
}

func (c *IMClient) runCloudKitBackfill(ctx context.Context, log zerolog.Logger) (cloudSyncCounters, error) {
	var total cloudSyncCounters
	backfillStart := time.Now()

	// Phase 1: Sync chats and attachments in parallel — they are independent.
	// Messages depend on both (chats for portal ID resolution, attachments for
	// GUID→record_name mapping), so they must wait.
	phase1Start := time.Now()

	var chatCounts cloudSyncCounters
	var chatToken *string
	var chatErr error
	var attMap map[string]cloudAttachmentRow
	var attToken *string
	var attErr error

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		chatStart := time.Now()
		chatCounts, chatToken, chatErr = c.syncCloudChats(ctx)
		log.Info().
			Dur("elapsed", time.Since(chatStart)).
			Int("imported", chatCounts.Imported).
			Int("updated", chatCounts.Updated).
			Int("skipped", chatCounts.Skipped).
			Err(chatErr).
			Msg("CloudKit chat sync complete")
	}()

	go func() {
		defer wg.Done()
		attStart := time.Now()
		attMap, attToken, attErr = c.syncCloudAttachments(ctx)
		attCount := 0
		if attMap != nil {
			attCount = len(attMap)
		}
		log.Info().
			Dur("elapsed", time.Since(attStart)).
			Int("attachments", attCount).
			Err(attErr).
			Msg("CloudKit attachment sync complete")
	}()

	wg.Wait()
	log.Info().Dur("phase1_elapsed", time.Since(phase1Start)).Msg("CloudKit phase 1 (chats + attachments) complete")

	if chatErr != nil {
		_ = c.cloudStore.setSyncStateError(ctx, cloudZoneChats, chatErr.Error())
		return total, chatErr
	}
	if err := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneChats, chatToken); err != nil {
		log.Warn().Err(err).Msg("Failed to persist chat sync token")
	}
	total.add(chatCounts)

	if attErr != nil {
		log.Warn().Err(attErr).Msg("Failed to sync CloudKit attachments (continuing without)")
	} else {
		if err := c.cloudStore.setSyncStateSuccess(ctx, cloudZoneAttachments, attToken); err != nil {
			log.Warn().Err(err).Msg("Failed to persist attachment sync token")
		}
	}

	// Phase 2: Sync messages (depends on chats + attachments).
	phase2Start := time.Now()
	msgCounts, msgToken, err := c.syncCloudMessages(ctx, attMap)
	if err != nil {
		_ = c.cloudStore.setSyncStateError(ctx, cloudZoneMessages, err.Error())
		return total, err
	}
	if err = c.cloudStore.setSyncStateSuccess(ctx, cloudZoneMessages, msgToken); err != nil {
		log.Warn().Err(err).Msg("Failed to persist message sync token")
	}
	total.add(msgCounts)

	log.Info().
		Dur("phase2_elapsed", time.Since(phase2Start)).
		Int("imported", msgCounts.Imported).
		Int("updated", msgCounts.Updated).
		Int("skipped", msgCounts.Skipped).
		Dur("total_elapsed", time.Since(backfillStart)).
		Msg("CloudKit phase 2 (messages) complete")

	return total, nil
}

// syncCloudAttachments syncs the attachment zone and builds a GUID→attachment info map.
func (c *IMClient) syncCloudAttachments(ctx context.Context) (map[string]cloudAttachmentRow, *string, error) {
	attMap := make(map[string]cloudAttachmentRow)
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneAttachments)
	if err != nil {
		return attMap, nil, err
	}

	for page := 0; page < 256; page++ {
		resp, syncErr := safeCloudSyncAttachments(c.client, token)
		if syncErr != nil {
			return attMap, token, syncErr
		}

		for _, att := range resp.Attachments {
			mime := ""
			if att.MimeType != nil {
				mime = *att.MimeType
			}
			uti := ""
			if att.UtiType != nil {
				uti = *att.UtiType
			}
			filename := ""
			if att.Filename != nil {
				filename = *att.Filename
			}
			attMap[att.Guid] = cloudAttachmentRow{
				GUID:       att.Guid,
				MimeType:   mime,
				UTIType:    uti,
				Filename:   filename,
				FileSize:   att.FileSize,
				RecordName: att.RecordName,
			}
		}

		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken
		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			break
		}
	}

	return attMap, token, nil
}

func (c *IMClient) syncCloudChats(ctx context.Context) (cloudSyncCounters, *string, error) {
	var counts cloudSyncCounters
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneChats)
	if err != nil {
		return counts, nil, err
	}

	for page := 0; page < 256; page++ {
		resp, syncErr := safeCloudSyncChats(c.client, token)
		if syncErr != nil {
			return counts, token, syncErr
		}

		ingestCounts, ingestErr := c.ingestCloudChats(ctx, resp.Chats)
		if ingestErr != nil {
			return counts, token, ingestErr
		}
		counts.add(ingestCounts)

		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken
		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			break
		}
	}

	return counts, token, nil
}

// safeCloudSyncMessages wraps the FFI call with panic recovery.
// UniFFI deserialization panics on malformed buffers; this prevents bridge crashes.
func safeCloudSyncMessages(client *rustpushgo.Client, token *string) (resp rustpushgo.WrappedCloudSyncMessagesPage, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			log.Error().Str("ffi_method", "CloudSyncMessages").Str("stack", stack).Msgf("FFI panic recovered: %v", r)
			err = fmt.Errorf("FFI panic in CloudSyncMessages: %v", r)
		}
	}()
	return client.CloudSyncMessages(token)
}

// safeCloudSyncChats wraps the FFI call with panic recovery.
func safeCloudSyncChats(client *rustpushgo.Client, token *string) (resp rustpushgo.WrappedCloudSyncChatsPage, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			log.Error().Str("ffi_method", "CloudSyncChats").Str("stack", stack).Msgf("FFI panic recovered: %v", r)
			err = fmt.Errorf("FFI panic in CloudSyncChats: %v", r)
		}
	}()
	return client.CloudSyncChats(token)
}

// safeCloudSyncAttachments wraps the FFI call with panic recovery.
func safeCloudSyncAttachments(client *rustpushgo.Client, token *string) (resp rustpushgo.WrappedCloudSyncAttachmentsPage, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			log.Error().Str("ffi_method", "CloudSyncAttachments").Str("stack", stack).Msgf("FFI panic recovered: %v", r)
			err = fmt.Errorf("FFI panic in CloudSyncAttachments: %v", r)
		}
	}()
	return client.CloudSyncAttachments(token)
}

func (c *IMClient) syncCloudMessages(ctx context.Context, attMap map[string]cloudAttachmentRow) (cloudSyncCounters, *string, error) {
	var counts cloudSyncCounters
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneMessages)
	if err != nil {
		return counts, nil, err
	}

	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()
	for page := 0; page < 256; page++ {
		resp, syncErr := safeCloudSyncMessages(c.client, token)
		if syncErr != nil {
			// FFI panic or deserialization error on this page.
			// Log and stop pagination — keep messages from previous pages.
			log.Warn().Err(syncErr).
				Int("page", page).
				Int("imported_so_far", counts.Imported).
				Msg("CloudKit message sync page failed (FFI error), stopping pagination with partial data")
			break
		}

		if len(resp.Messages) > 0 {
			log.Info().
				Int("page", page).
				Int("messages", len(resp.Messages)).
				Int32("status", resp.Status).
				Bool("done", resp.Done).
				Msg("CloudKit message sync page")
		}

		if err = c.ingestCloudMessages(ctx, resp.Messages, "", &counts, attMap); err != nil {
			return counts, token, err
		}

		prev := ptrStringOr(token, "")
		token = resp.ContinuationToken
		if resp.Done || (page > 0 && prev == ptrStringOr(token, "")) {
			break
		}
	}

	return counts, token, nil
}

func (c *IMClient) ingestCloudChats(ctx context.Context, chats []rustpushgo.WrappedCloudSyncChat) (cloudSyncCounters, error) {
	var counts cloudSyncCounters

	// Batch existence check for all non-deleted chat IDs.
	chatIDs := make([]string, 0, len(chats))
	for _, chat := range chats {
		if !chat.Deleted {
			chatIDs = append(chatIDs, chat.CloudChatId)
		}
	}
	existingSet, err := c.cloudStore.hasChatBatch(ctx, chatIDs)
	if err != nil {
		return counts, fmt.Errorf("batch chat existence check failed: %w", err)
	}

	// Collect deleted chat IDs for batch deletion from DB.
	var deletedChatIDs []string

	// Build batch of rows.
	batch := make([]cloudChatUpsertRow, 0, len(chats))
	for _, chat := range chats {
		if chat.Deleted {
			counts.Deleted++
			deletedChatIDs = append(deletedChatIDs, chat.CloudChatId)
			continue
		}

		portalID := c.resolvePortalIDForCloudChat(chat.Participants, chat.DisplayName, chat.GroupId, chat.Style)
		if portalID == "" {
			counts.Skipped++
			continue
		}

		participantsJSON, jsonErr := json.Marshal(chat.Participants)
		if jsonErr != nil {
			return counts, jsonErr
		}

		batch = append(batch, cloudChatUpsertRow{
			CloudChatID:      chat.CloudChatId,
			RecordName:       chat.RecordName,
			GroupID:          strings.ToLower(chat.GroupId),
			PortalID:         portalID,
			Service:          chat.Service,
			DisplayName:      nullableString(chat.DisplayName),
			ParticipantsJSON: string(participantsJSON),
			UpdatedTS:        int64(chat.UpdatedTimestampMs),
		})

		if existingSet[chat.CloudChatId] {
			counts.Updated++
		} else {
			counts.Imported++
		}
	}

	// Batch insert all non-deleted chats.
	if err := c.cloudStore.upsertChatBatch(ctx, batch); err != nil {
		return counts, err
	}

	// Remove deleted chats from DB so they don't produce portals.
	if err := c.cloudStore.deleteChatBatch(ctx, deletedChatIDs); err != nil {
		return counts, fmt.Errorf("failed to delete chats: %w", err)
	}

	return counts, nil
}

// uuidPattern matches a UUID string (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveConversationID determines the canonical portal ID for a cloud message.
//
// Rule 1: If chat_id is a UUID → it's a group conversation → "gid:<lowercase-uuid>"
// Rule 2: Otherwise derive from sender (DM) → "tel:+..." or "mailto:..."
// Rule 3: Messages create conversations. Never discard a message because
//         we haven't seen the chat record yet.
func (c *IMClient) resolveConversationID(ctx context.Context, msg rustpushgo.WrappedCloudSyncMessage) string {
	// Check if chat_id is a UUID (= group conversation)
	if msg.CloudChatId != "" && uuidPattern.MatchString(msg.CloudChatId) {
		return "gid:" + strings.ToLower(msg.CloudChatId)
	}

	// Try to look up the chat record for non-UUID chat_ids
	// (e.g., "iMessage;-;+16692858317" or "chat12345...")
	if msg.CloudChatId != "" {
		if portalID, err := c.cloudStore.getChatPortalID(ctx, msg.CloudChatId); err == nil && portalID != "" {
			return portalID
		}
	}

	// DM: derive from sender
	if msg.Sender != "" && !msg.IsFromMe {
		normalized := normalizeIdentifierForPortalID(msg.Sender)
		if normalized != "" {
			resolved := c.resolveContactPortalID(normalized)
			resolved = c.resolveExistingDMPortalID(string(resolved))
			return string(resolved)
		}
	}

	// is_from_me DMs: derive from destination
	if msg.IsFromMe && msg.CloudChatId != "" {
		// chat_id for DMs is like "iMessage;-;+16692858317"
		parts := strings.Split(msg.CloudChatId, ";")
		if len(parts) == 3 {
			normalized := normalizeIdentifierForPortalID(parts[2])
			if normalized != "" {
				resolved := c.resolveContactPortalID(normalized)
				resolved = c.resolveExistingDMPortalID(string(resolved))
				return string(resolved)
			}
		}
	}

	return ""
}

func (c *IMClient) ingestCloudMessages(
	ctx context.Context,
	messages []rustpushgo.WrappedCloudSyncMessage,
	preferredPortalID string,
	counts *cloudSyncCounters,
	attMap map[string]cloudAttachmentRow,
) error {
	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()

	// Separate deleted messages from live messages up front.
	// Deleted messages are removed from the DB (they may have been stored
	// in a previous sync before the user deleted them in iCloud).
	var deletedGUIDs []string
	var liveMessages []rustpushgo.WrappedCloudSyncMessage
	for _, msg := range messages {
		if msg.Deleted {
			counts.Deleted++
			if msg.Guid != "" {
				deletedGUIDs = append(deletedGUIDs, msg.Guid)
			}
			continue
		}
		liveMessages = append(liveMessages, msg)
	}

	// Remove deleted messages from DB.
	if err := c.cloudStore.deleteMessageBatch(ctx, deletedGUIDs); err != nil {
		return fmt.Errorf("failed to delete messages: %w", err)
	}

	// Phase 1: Resolve portal IDs and build rows for live messages (no DB writes yet).
	guids := make([]string, 0, len(liveMessages))
	for _, msg := range liveMessages {
		if msg.Guid != "" {
			guids = append(guids, msg.Guid)
		}
	}
	existingSet, err := c.cloudStore.hasMessageBatch(ctx, guids)
	if err != nil {
		return fmt.Errorf("batch existence check failed: %w", err)
	}

	batch := make([]cloudMessageRow, 0, len(liveMessages))
	for _, msg := range liveMessages {
		if msg.Guid == "" {
			log.Warn().
				Str("cloud_chat_id", msg.CloudChatId).
				Str("sender", msg.Sender).
				Bool("is_from_me", msg.IsFromMe).
				Int64("timestamp_ms", msg.TimestampMs).
				Msg("Skipping message with empty GUID")
			counts.Skipped++
			continue
		}

		portalID := c.resolveConversationID(ctx, msg)
		if portalID == "" {
			portalID = preferredPortalID
		}
		if portalID == "" {
			log.Warn().
				Str("guid", msg.Guid).
				Str("cloud_chat_id", msg.CloudChatId).
				Str("sender", msg.Sender).
				Bool("is_from_me", msg.IsFromMe).
				Int64("timestamp_ms", msg.TimestampMs).
				Str("service", msg.Service).
				Msg("Skipping message: could not resolve portal ID")
			counts.Skipped++
			continue
		}

		text := ""
		if msg.Text != nil {
			text = *msg.Text
		}
		subject := ""
		if msg.Subject != nil {
			subject = *msg.Subject
		}
		timestampMS := msg.TimestampMs
		if timestampMS <= 0 {
			timestampMS = time.Now().UnixMilli()
		}

		tapbackTargetGUID := ""
		if msg.TapbackTargetGuid != nil {
			tapbackTargetGUID = *msg.TapbackTargetGuid
		}
		tapbackEmoji := ""
		if msg.TapbackEmoji != nil {
			tapbackEmoji = *msg.TapbackEmoji
		}

		// Enrich and serialize attachment metadata.
		attachmentsJSON := ""
		if len(msg.AttachmentGuids) > 0 && attMap != nil {
			var attRows []cloudAttachmentRow
			for _, guid := range msg.AttachmentGuids {
				if guid == "" {
					continue
				}
				if enriched, ok := attMap[guid]; ok {
					attRows = append(attRows, enriched)
				}
			}
			if len(attRows) > 0 {
				if attJSON, jsonErr := json.Marshal(attRows); jsonErr == nil {
					attachmentsJSON = string(attJSON)
				}
			}
		}

		batch = append(batch, cloudMessageRow{
			GUID:              msg.Guid,
			CloudChatID:       msg.CloudChatId,
			PortalID:          portalID,
			TimestampMS:       timestampMS,
			Sender:            msg.Sender,
			IsFromMe:          msg.IsFromMe,
			Text:              text,
			Subject:           subject,
			Service:           msg.Service,
			Deleted:           false,
			TapbackType:       msg.TapbackType,
			TapbackTargetGUID: tapbackTargetGUID,
			TapbackEmoji:      tapbackEmoji,
			AttachmentsJSON:   attachmentsJSON,
		})

		if existingSet[msg.Guid] {
			counts.Updated++
		} else {
			counts.Imported++
		}
	}

	// Phase 2: Batch insert all live rows in a single transaction.
	if err := c.cloudStore.upsertMessageBatch(ctx, batch); err != nil {
		return err
	}

	return nil
}

func (c *IMClient) resolvePortalIDForCloudChat(participants []string, displayName *string, groupID string, style int64) string {
	normalizedParticipants := make([]string, 0, len(participants))
	for _, participant := range participants {
		normalized := normalizeIdentifierForPortalID(participant)
		if normalized == "" {
			continue
		}
		normalizedParticipants = append(normalizedParticipants, normalized)
	}
	if len(normalizedParticipants) == 0 {
		return ""
	}

	// CloudKit chat style: 43 = group, 45 = DM.
	// Use style as the authoritative group/DM signal. The group_id (gid)
	// field is set for ALL CloudKit chats, even DMs, so we can't use its
	// presence alone.
	isGroup := style == 43

	// For groups with a persistent group UUID, use gid:<UUID> as portal ID
	if isGroup && groupID != "" {
		normalizedGID := strings.ToLower(groupID)
		return "gid:" + normalizedGID
	}

	// For DMs: use the single remote participant as the portal ID
	// (e.g., "tel:+15551234567" or "mailto:user@example.com").
	// Filter out our own handle so only the remote side remains.
	remoteParticipants := make([]string, 0, len(normalizedParticipants))
	for _, p := range normalizedParticipants {
		if !c.isMyHandle(p) {
			remoteParticipants = append(remoteParticipants, p)
		}
	}

	if len(remoteParticipants) == 1 {
		// Standard DM — portal ID is the remote participant
		return remoteParticipants[0]
	}

	// Fallback for edge cases (unknown style, multi-participant without group style)
	groupName := displayName
	var senderGuidPtr *string
	if isGroup && groupID != "" {
		senderGuidPtr = &groupID
	}
	portalKey := c.makePortalKey(normalizedParticipants, groupName, nil, senderGuidPtr)
	return string(portalKey.ID)
}

func (c *IMClient) createPortalsFromCloudSync(ctx context.Context, log zerolog.Logger) {
	if c.cloudStore == nil {
		return
	}

	// Get portal IDs sorted by newest message timestamp (most recent first).
	// This lets us prioritize forward backfill for active conversations.
	portalInfos, err := c.cloudStore.listPortalIDsWithNewestTimestamp(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list cloud portal IDs with timestamps")
		return
	}

	// Also get portals that exist in cloud_chat but have zero messages.
	// These still need rooms but won't have any forward backfill to do.
	allPortalIDs, err := c.cloudStore.listAllPortalIDs(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list all cloud chat portal IDs")
		return
	}
	if len(allPortalIDs) == 0 && len(portalInfos) == 0 {
		return
	}

	// Build set of portals that have messages (already in portalInfos).
	hasMessages := make(map[string]bool, len(portalInfos))
	for _, p := range portalInfos {
		hasMessages[p.PortalID] = true
	}

	// Portals with no messages — create room but skip all backfill.
	var noMessagePortals []string
	for _, pid := range allPortalIDs {
		if !hasMessages[pid] {
			noMessagePortals = append(noMessagePortals, pid)
		}
	}

	// Split portals with messages into priority (recent) vs deferred (old).
	// Priority portals get immediate forward backfill (megolm session created
	// inline). Deferred portals skip forward backfill — the backward backfill
	// queue fills them gradually, spreading megolm cost over time.
	const priorityCutoffDays = 14
	cutoffTS := time.Now().Add(-priorityCutoffDays * 24 * time.Hour).UnixMilli()

	var priorityPortals []string   // recent activity → forward backfill
	var deferredPortals []string   // old activity → backward backfill only

	for _, p := range portalInfos {
		if p.NewestTS >= cutoffTS {
			priorityPortals = append(priorityPortals, p.PortalID)
		} else {
			deferredPortals = append(deferredPortals, p.PortalID)
		}
	}

	// Register the skip-set so FetchMessages returns 0 messages for
	// deferred + no-message portals during forward backfill.
	skipSet := make(map[string]bool, len(deferredPortals)+len(noMessagePortals))
	for _, pid := range deferredPortals {
		skipSet[pid] = true
	}
	for _, pid := range noMessagePortals {
		skipSet[pid] = true
	}
	c.initialSyncSkipMu.Lock()
	c.initialSyncSkipForwardBackfill = skipSet
	c.initialSyncSkipMu.Unlock()

	totalPortals := len(priorityPortals) + len(deferredPortals) + len(noMessagePortals)
	portalStart := time.Now()
	log.Info().
		Int("total", totalPortals).
		Int("priority", len(priorityPortals)).
		Int("deferred", len(deferredPortals)).
		Int("no_messages", len(noMessagePortals)).
		Int("cutoff_days", priorityCutoffDays).
		Msg("Creating portals from cloud sync — priority portals get forward backfill first, deferred portals use backward backfill queue")

	// Queue events: priority portals first (most recent activity first within
	// that group), then deferred, then no-message portals.
	// portalInfos is already sorted by newest_ts DESC, so priorityPortals
	// and deferredPortals preserve that order.
	ordered := make([]string, 0, totalPortals)
	ordered = append(ordered, priorityPortals...)
	ordered = append(ordered, deferredPortals...)
	ordered = append(ordered, noMessagePortals...)

	created := 0
	for i, portalID := range ordered {
		portalKey := networkid.PortalKey{
			ID:       networkid.PortalID(portalID),
			Receiver: c.UserLogin.ID,
		}

		isPriority := i < len(priorityPortals)
		res := c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.
						Str("portal_id", portalID).
						Str("source", "cloud_sync").
						Bool("priority", isPriority)
				},
			},
			GetChatInfoFunc: c.GetChatInfo,
		})
		if res.Success {
			created++
		}
		// Progress log at phase transitions and every 100 portals.
		if i+1 == len(priorityPortals) {
			log.Info().
				Int("priority_queued", len(priorityPortals)).
				Dur("elapsed", time.Since(portalStart)).
				Msg("All priority portals queued — now queuing deferred portals")
		} else if (i+1)%100 == 0 {
			log.Info().
				Int("progress", i+1).
				Int("total", totalPortals).
				Int("created_so_far", created).
				Dur("elapsed", time.Since(portalStart)).
				Msg("Portal creation progress")
		}
	}

	log.Info().
		Int("created", created).
		Int("total", totalPortals).
		Int("priority_forward_backfill", len(priorityPortals)).
		Int("deferred_backward_only", len(deferredPortals)).
		Int("no_messages_skipped", len(noMessagePortals)).
		Dur("elapsed", time.Since(portalStart)).
		Msg("Finished queuing portals from cloud sync")
}

func (c *IMClient) ensureCloudSyncStore(ctx context.Context) error {
	if c.cloudStore == nil {
		return fmt.Errorf("cloud store not initialized")
	}
	return c.cloudStore.ensureSchema(ctx)
}
