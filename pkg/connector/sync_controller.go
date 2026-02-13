package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

const (
	cloudIncrementalInterval = 10 * time.Minute
	cloudRepairInterval      = 60 * time.Minute
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

	for page := 0; page < 256; page++ {
		resp, syncErr := c.client.CloudSyncMessages(token)
		if syncErr != nil {
			return counts, token, syncErr
		}

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

func (c *IMClient) ingestCloudMessages(
	ctx context.Context,
	messages []rustpushgo.WrappedCloudSyncMessage,
	preferredPortalID string,
	counts *cloudSyncCounters,
) error {
	for _, msg := range messages {
		if msg.Guid == "" {
			counts.Skipped++
			continue
		}

		portalID := preferredPortalID
		if portalID == "" && msg.CloudChatId != "" {
			resolvedPortalID, err := c.cloudStore.getChatPortalID(ctx, msg.CloudChatId)
			if err != nil {
				return err
			}
			portalID = resolvedPortalID
		}
		if portalID == "" && msg.Sender != "" && !msg.IsFromMe {
			normalizedSender := normalizeIdentifierForPortalID(msg.Sender)
			if normalizedSender != "" {
				resolvedPortal := c.resolveContactPortalID(normalizedSender)
				resolvedPortal = c.resolveExistingDMPortalID(string(resolvedPortal))
				portalID = string(resolvedPortal)
			}
		}
		if portalID == "" {
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

func (c *IMClient) ensureCloudSyncStore(ctx context.Context) error {
	if c.cloudStore == nil {
		return fmt.Errorf("cloud store not initialized")
	}
	return c.cloudStore.ensureSchema(ctx)
}
