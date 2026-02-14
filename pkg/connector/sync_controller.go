package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

const (
	// TODO: Subscribe to CloudKit zone change notifications (APNs push)
	// for instant sync like iPhone/Mac. Polling is a stopgap.
	cloudIncrementalInterval = 30 * time.Second
	cloudRepairInterval      = 30 * time.Minute
	cloudRepairLookback      = 6 * time.Hour
)

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
	c.contactsReadyLock.Lock()
	if c.contactsReady {
		c.contactsReadyLock.Unlock()
		return
	}
	c.contactsReady = true
	readyCh := c.contactsReadyCh
	c.contactsReadyLock.Unlock()
	if readyCh != nil {
		close(readyCh)
	}
	log.Info().Msg("Contacts readiness gate satisfied")

	// Re-resolve ghost names now that contacts are available.
	// Ghosts created before contacts loaded have raw phone numbers as names.
	go c.refreshGhostNamesFromContacts(log)
}

func (c *IMClient) refreshGhostNamesFromContacts(log zerolog.Logger) {
	if c.cloudContacts == nil {
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
		contact, _ := c.cloudContacts.GetContactInfo(localID)
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
	if !c.waitForContactsReady(log) {
		return
	}

	// On a fresh DB (no messages), clear any stale continuation tokens
	// so the bootstrap does a full sync from scratch.
	hasMessages, _ := c.cloudStore.hasAnyMessages(ctx)
	if !hasMessages {
		if err := c.cloudStore.clearSyncTokens(ctx); err != nil {
			log.Warn().Err(err).Msg("Failed to clear stale sync tokens")
		} else {
			log.Info().Msg("Fresh database detected, cleared sync tokens for full bootstrap")
		}
	}

	log.Info().Msg("Cloud bootstrap sync start")

	// Dump raw CloudKit chat records to disk for debugging
	c.dumpCloudKitChats(ctx, log)

	counts, err := c.runIncrementalCloudSync(ctx, log)
	if err != nil {
		log.Error().Err(err).Msg("Cloud bootstrap sync failed")
	} else {
		log.Info().
			Int("imported", counts.Imported).
			Int("updated", counts.Updated).
			Int("skipped", counts.Skipped).
			Int("deleted", counts.Deleted).
			Msg("Cloud bootstrap sync end")
	}

	// Diagnostic: do a fresh full sync from scratch to see if CloudKit
	// returns different records than the token-based sync did.
	if diagResult, diagErr := c.client.CloudDiagFullCount(); diagErr != nil {
		log.Warn().Err(diagErr).Msg("CloudKit diagnostic full count failed")
	} else {
		log.Info().Str("diag", diagResult).Msg("CloudKit diagnostic full count result")
	}

	c.createPortalsFromCloudSync(ctx, log)

	if err = c.enqueueRecentRepairTasks(ctx, log); err != nil {
		log.Warn().Err(err).Msg("Failed to enqueue initial repair tasks")
	}
	c.executeRepairTasks(ctx, log, 50)

	incrementalTicker := time.NewTicker(cloudIncrementalInterval)
	repairTicker := time.NewTicker(cloudRepairInterval)
	defer incrementalTicker.Stop()
	defer repairTicker.Stop()

	for {
		select {
		case <-incrementalTicker.C:
			log.Info().Msg("Cloud incremental sync start")
			counts, err = c.runIncrementalCloudSync(ctx, log)
			if err != nil {
				log.Warn().Err(err).Msg("Cloud incremental sync failed")
			} else {
				log.Info().
					Int("imported", counts.Imported).
					Int("updated", counts.Updated).
					Int("skipped", counts.Skipped).
					Int("deleted", counts.Deleted).
					Msg("Cloud incremental sync end")
			}
			c.executeRepairTasks(ctx, log, 20)
		case <-repairTicker.C:
			if err = c.enqueueRecentRepairTasks(ctx, log); err != nil {
				log.Warn().Err(err).Msg("Failed to enqueue repair tasks")
			}
			c.executeRepairTasks(ctx, log, 100)
		case <-c.stopChan:
			return
		}
	}
}

func (c *IMClient) runIncrementalCloudSync(ctx context.Context, log zerolog.Logger) (cloudSyncCounters, error) {
	var total cloudSyncCounters

	chatCounts, chatToken, err := c.syncCloudChats(ctx)
	if err != nil {
		_ = c.cloudStore.setSyncStateError(ctx, cloudZoneChats, err.Error())
		return total, err
	}
	if err = c.cloudStore.setSyncStateSuccess(ctx, cloudZoneChats, chatToken); err != nil {
		log.Warn().Err(err).Msg("Failed to persist chat sync token")
	}
	total.add(chatCounts)

	msgCounts, msgToken, err := c.syncCloudMessages(ctx)
	if err != nil {
		_ = c.cloudStore.setSyncStateError(ctx, cloudZoneMessages, err.Error())
		return total, err
	}
	if err = c.cloudStore.setSyncStateSuccess(ctx, cloudZoneMessages, msgToken); err != nil {
		log.Warn().Err(err).Msg("Failed to persist message sync token")
	}
	total.add(msgCounts)

	return total, nil
}

func (c *IMClient) syncCloudChats(ctx context.Context) (cloudSyncCounters, *string, error) {
	var counts cloudSyncCounters
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneChats)
	if err != nil {
		return counts, nil, err
	}

	for page := 0; page < 256; page++ {
		resp, syncErr := c.client.CloudSyncChats(token)
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

func (c *IMClient) syncCloudMessages(ctx context.Context) (cloudSyncCounters, *string, error) {
	var counts cloudSyncCounters
	token, err := c.cloudStore.getSyncState(ctx, cloudZoneMessages)
	if err != nil {
		return counts, nil, err
	}

	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()
	for page := 0; page < 256; page++ {
		resp, syncErr := c.client.CloudSyncMessages(token)
		if syncErr != nil {
			return counts, token, syncErr
		}

		log.Info().
			Int("page", page).
			Int("messages", len(resp.Messages)).
			Int32("status", resp.Status).
			Bool("done", resp.Done).
			Bool("has_token", token != nil).
			Msg("CloudKit message sync page")

		if err = c.ingestCloudMessages(ctx, resp.Messages, "", &counts); err != nil {
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
	for _, chat := range chats {
		if chat.Deleted {
			counts.Deleted++
			continue
		}

		portalID := c.resolvePortalIDForCloudChat(chat.Participants, chat.DisplayName, chat.GroupId, chat.Style)
		if portalID == "" {
			counts.Skipped++
			continue
		}

		exists, err := c.cloudStore.hasChat(ctx, chat.CloudChatId)
		if err != nil {
			return counts, err
		}

		if err = c.cloudStore.upsertChat(
			ctx,
			chat.CloudChatId,
			chat.RecordName,
			strings.ToLower(chat.GroupId),
			portalID,
			chat.Service,
			chat.DisplayName,
			chat.Participants,
			int64(chat.UpdatedTimestampMs),
		); err != nil {
			return counts, err
		}

		if exists {
			counts.Updated++
		} else {
			counts.Imported++
		}
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
) error {
	log := c.Main.Bridge.Log.With().Str("component", "cloud_sync").Logger()
	for _, msg := range messages {
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

		existing, err := c.cloudStore.hasMessage(ctx, msg.Guid)
		if err != nil {
			return err
		}

		text := ""
		if msg.Text != nil {
			text = *msg.Text
		}
		timestampMS := msg.TimestampMs
		if timestampMS <= 0 {
			timestampMS = time.Now().UnixMilli()
		}

		if err = c.cloudStore.upsertMessage(ctx, cloudMessageRow{
			GUID:        msg.Guid,
			CloudChatID: msg.CloudChatId,
			PortalID:    portalID,
			TimestampMS: timestampMS,
			Sender:      msg.Sender,
			IsFromMe:    msg.IsFromMe,
			Text:        text,
			Service:     msg.Service,
			Deleted:     msg.Deleted,
		}); err != nil {
			return err
		}

		if msg.Deleted {
			counts.Deleted++
		}
		if existing {
			counts.Updated++
		} else {
			counts.Imported++
		}
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

func (c *IMClient) enqueueRecentRepairTasks(ctx context.Context, log zerolog.Logger) error {
	now := time.Now()
	notBefore := now.UnixMilli()
	sinceActive := now.Add(-24 * time.Hour).UnixMilli()
	reconcileSince := now.Add(-cloudRepairLookback).UnixMilli()

	activeChats, err := c.cloudStore.listActiveChatsSince(ctx, sinceActive, 25)
	if err != nil {
		return err
	}

	enqueued := 0
	for _, chat := range activeChats {
		if err = c.cloudStore.enqueueRepairTask(
			ctx,
			repairTaskActiveRecent,
			chat.CloudChatID,
			chat.PortalID,
			reconcileSince,
			notBefore,
		); err != nil {
			return err
		}
		enqueued++
	}

	if err = c.cloudStore.enqueueRepairTask(
		ctx,
		repairTaskGlobalRecent,
		"",
		"",
		reconcileSince,
		notBefore,
	); err != nil {
		return err
	}
	enqueued++

	log.Info().
		Int("enqueued", enqueued).
		Int("active_chat_tasks", len(activeChats)).
		Msg("Repair tasks enqueued")
	return nil
}

func (c *IMClient) executeRepairTasks(ctx context.Context, log zerolog.Logger, limit int) {
	tasks, err := c.cloudStore.getDueRepairTasks(ctx, limit)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to load due repair tasks")
		return
	}
	if len(tasks) == 0 {
		return
	}

	for _, task := range tasks {
		taskLog := log.With().
			Int64("task_id", task.ID).
			Str("task_type", task.TaskType).
			Str("cloud_chat_id", task.CloudChatID).
			Int("attempts", task.Attempts).
			Logger()

		taskLog.Info().Msg("Executing repair task")

		var chatID *string
		if task.TaskType == repairTaskActiveRecent && task.CloudChatID != "" {
			chatID = &task.CloudChatID
		}

		recentMessages, fetchErr := c.client.CloudFetchRecentMessages(
			uint64(maxInt64(task.SinceTSMS, 0)),
			chatID,
			8,
			1000,
		)
		if fetchErr != nil {
			_ = c.cloudStore.markRepairTaskFailed(ctx, task.ID, fetchErr.Error())
			taskLog.Warn().Err(fetchErr).Msg("Repair task failed: cloud fetch error")
			continue
		}

		counts := cloudSyncCounters{}
		ingestErr := c.ingestCloudMessages(ctx, recentMessages, task.PortalID, &counts)
		if ingestErr != nil {
			_ = c.cloudStore.markRepairTaskFailed(ctx, task.ID, ingestErr.Error())
			taskLog.Warn().Err(ingestErr).Msg("Repair task failed: ingest error")
			continue
		}

		if err = c.cloudStore.markRepairTaskDone(ctx, task.ID); err != nil {
			taskLog.Warn().Err(err).Msg("Failed to mark repair task done")
		}
		taskLog.Info().
			Int("imported", counts.Imported).
			Int("updated", counts.Updated).
			Int("skipped", counts.Skipped).
			Int("deleted", counts.Deleted).
			Msg("Repair task completed")
	}
}

func (c *IMClient) createPortalsFromCloudSync(ctx context.Context, log zerolog.Logger) {
	if c.cloudStore == nil {
		return
	}

	portalIDs, err := c.cloudStore.listAllPortalIDs(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list cloud chat portal IDs")
		return
	}
	if len(portalIDs) == 0 {
		return
	}

	log.Info().Int("chat_count", len(portalIDs)).Msg("Creating portals from cloud sync")

	created := 0
	for _, portalID := range portalIDs {
		portalKey := networkid.PortalKey{
			ID:       networkid.PortalID(portalID),
			Receiver: c.UserLogin.ID,
		}

		res := c.UserLogin.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type:         bridgev2.RemoteEventChatResync,
				PortalKey:    portalKey,
				CreatePortal: true,
				LogContext: func(lc zerolog.Context) zerolog.Context {
					return lc.Str("portal_id", portalID).Str("source", "cloud_sync")
				},
			},
			GetChatInfoFunc: c.GetChatInfo,
		})
		if res.Success {
			created++
		}
	}

	log.Info().Int("created", created).Int("total", len(portalIDs)).Msg("Finished creating portals from cloud sync")
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (c *IMClient) dumpCloudKitChats(ctx context.Context, log zerolog.Logger) {
	var allChats []rustpushgo.WrappedCloudSyncChat
	var token *string
	for page := 0; page < 256; page++ {
		resp, err := c.client.CloudSyncChats(token)
		if err != nil {
			log.Warn().Err(err).Int("page", page).Msg("CloudKit dump failed")
			return
		}
		allChats = append(allChats, resp.Chats...)
		log.Info().Int("page", page).Int("count", len(resp.Chats)).Bool("done", resp.Done).Msg("CloudKit dump page")
		if resp.Done {
			break
		}
		token = resp.ContinuationToken
	}

	// Build JSON manually since WrappedCloudSyncChat isn't tagged
	type chatDump struct {
		RecordName   string   `json:"record_name"`
		CloudChatID  string   `json:"cloud_chat_id"`
		GroupID      string   `json:"group_id"`
		Style        int64    `json:"style"`
		Service      string   `json:"service"`
		DisplayName  string   `json:"display_name"`
		Participants []string `json:"participants"`
		Deleted      bool     `json:"deleted"`
	}
	dump := make([]chatDump, len(allChats))
	for i, ch := range allChats {
		dn := ""
		if ch.DisplayName != nil {
			dn = *ch.DisplayName
		}
		dump[i] = chatDump{
			RecordName:   ch.RecordName,
			CloudChatID:  ch.CloudChatId,
			GroupID:      ch.GroupId,
			Style:        ch.Style,
			Service:      ch.Service,
			DisplayName:  dn,
			Participants: ch.Participants,
			Deleted:      ch.Deleted,
		}
	}

	jsonBytes, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		log.Warn().Err(err).Msg("Failed to marshal CloudKit dump")
		return
	}

	dumpPath := "cloudkit_chats_dump.json"
	if err := os.WriteFile(dumpPath, jsonBytes, 0644); err != nil {
		log.Warn().Err(err).Msg("Failed to write CloudKit dump")
		return
	}
	log.Info().Str("path", dumpPath).Int("chats", len(allChats)).Msg("Dumped raw CloudKit chat records")
}

func (c *IMClient) ensureCloudSyncStore(ctx context.Context) error {
	if c.cloudStore == nil {
		return fmt.Errorf("cloud store not initialized")
	}
	return c.cloudStore.ensureSchema(ctx)
}
