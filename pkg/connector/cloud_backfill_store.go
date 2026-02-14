package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

type cloudBackfillStore struct {
	db      *dbutil.Database
	loginID networkid.UserLoginID
}

type cloudMessageRow struct {
	GUID        string
	CloudChatID string
	PortalID    string
	TimestampMS int64
	Sender      string
	IsFromMe    bool
	Text        string
	Service     string
	Deleted     bool
}

type cloudRepairTask struct {
	ID          int64
	TaskType    string
	CloudChatID string
	PortalID    string
	SinceTSMS   int64
	Attempts    int
}

type cloudActiveChat struct {
	CloudChatID string
	PortalID    string
}

const (
	cloudZoneChats    = "chatManateeZone"
	cloudZoneMessages = "messageManateeZone"

	repairTaskActiveRecent = "active_chat_recent"
	repairTaskGlobalRecent = "global_recent"
)

func newCloudBackfillStore(db *dbutil.Database, loginID networkid.UserLoginID) *cloudBackfillStore {
	return &cloudBackfillStore{db: db, loginID: loginID}
}

func (s *cloudBackfillStore) ensureSchema(ctx context.Context) error {
	repairTaskTable := `CREATE TABLE IF NOT EXISTS cloud_repair_task (
		id BIGSERIAL PRIMARY KEY,
		login_id TEXT NOT NULL,
		task_type TEXT NOT NULL,
		cloud_chat_id TEXT,
		portal_id TEXT,
		since_ts_ms BIGINT NOT NULL,
		not_before_ts BIGINT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		created_ts BIGINT NOT NULL,
		updated_ts BIGINT NOT NULL,
		done_ts BIGINT
	)`
	if s.db.Dialect == dbutil.SQLite {
		repairTaskTable = `CREATE TABLE IF NOT EXISTS cloud_repair_task (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			login_id TEXT NOT NULL,
			task_type TEXT NOT NULL,
			cloud_chat_id TEXT,
			portal_id TEXT,
			since_ts_ms BIGINT NOT NULL,
			not_before_ts BIGINT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_ts BIGINT NOT NULL,
			updated_ts BIGINT NOT NULL,
			done_ts BIGINT
		)`
	}

	queries := []string{
		`CREATE TABLE IF NOT EXISTS cloud_sync_state (
			login_id TEXT NOT NULL,
			zone TEXT NOT NULL,
			continuation_token TEXT,
			last_success_ts BIGINT,
			last_error TEXT,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, zone)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_chat (
			login_id TEXT NOT NULL,
			cloud_chat_id TEXT NOT NULL,
			record_name TEXT NOT NULL DEFAULT '',
			group_id TEXT NOT NULL DEFAULT '',
			portal_id TEXT NOT NULL,
			service TEXT,
			display_name TEXT,
			participants_json TEXT,
			updated_ts BIGINT,
			created_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, cloud_chat_id)
		)`,
		`CREATE TABLE IF NOT EXISTS cloud_message (
			login_id TEXT NOT NULL,
			guid TEXT NOT NULL,
			chat_id TEXT,
			portal_id TEXT,
			timestamp_ms BIGINT NOT NULL,
			sender TEXT,
			is_from_me BOOLEAN NOT NULL,
			text TEXT,
			service TEXT,
			deleted BOOLEAN NOT NULL DEFAULT FALSE,
			created_ts BIGINT NOT NULL,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, guid)
		)`,
		repairTaskTable,
		`CREATE INDEX IF NOT EXISTS cloud_chat_portal_idx
			ON cloud_chat (login_id, portal_id, cloud_chat_id)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_portal_ts_idx
			ON cloud_message (login_id, portal_id, timestamp_ms, guid)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_chat_ts_idx
			ON cloud_message (login_id, chat_id, timestamp_ms, guid)`,
		`CREATE INDEX IF NOT EXISTS cloud_repair_due_idx
			ON cloud_repair_task (login_id, done_ts, not_before_ts, id)`,
	}

	// Run table creation queries first (without indexes that depend on migrations)
	for _, query := range queries {
		if _, err := s.db.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to ensure cloud backfill schema: %w", err)
		}
	}

	// Migration: add record_name column if missing (SQLite doesn't support IF NOT EXISTS on ALTER)
	var hasRecordName int
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('cloud_chat') WHERE name='record_name'`).Scan(&hasRecordName)
	if hasRecordName == 0 {
		if _, err := s.db.Exec(ctx, `ALTER TABLE cloud_chat ADD COLUMN record_name TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("failed to add record_name column: %w", err)
		}
	}

	// Migration: add group_id column if missing
	var hasGroupID int
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('cloud_chat') WHERE name='group_id'`).Scan(&hasGroupID)
	if hasGroupID == 0 {
		if _, err := s.db.Exec(ctx, `ALTER TABLE cloud_chat ADD COLUMN group_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("failed to add group_id column: %w", err)
		}
	}

	// Create index that depends on record_name column (must be after migration)
	if _, err := s.db.Exec(ctx, `CREATE INDEX IF NOT EXISTS cloud_chat_record_name_idx
		ON cloud_chat (login_id, record_name) WHERE record_name <> ''`); err != nil {
		return fmt.Errorf("failed to create record_name index: %w", err)
	}

	// Create index for group_id lookups (messages reference chats by group_id UUID)
	if _, err := s.db.Exec(ctx, `CREATE INDEX IF NOT EXISTS cloud_chat_group_id_idx
		ON cloud_chat (login_id, group_id) WHERE group_id <> ''`); err != nil {
		return fmt.Errorf("failed to create group_id index: %w", err)
	}

	return nil
}

func (s *cloudBackfillStore) getSyncState(ctx context.Context, zone string) (*string, error) {
	var token sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT continuation_token FROM cloud_sync_state WHERE login_id=$1 AND zone=$2`,
		s.loginID, zone,
	).Scan(&token)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if !token.Valid {
		return nil, nil
	}
	return &token.String, nil
}

func (s *cloudBackfillStore) setSyncStateSuccess(ctx context.Context, zone string, token *string) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_success_ts, last_error, updated_ts)
		VALUES ($1, $2, $3, $4, NULL, $5)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			continuation_token=excluded.continuation_token,
			last_success_ts=excluded.last_success_ts,
			last_error=NULL,
			updated_ts=excluded.updated_ts
	`, s.loginID, zone, nullableString(token), nowMS, nowMS)
	return err
}

func (s *cloudBackfillStore) clearSyncTokens(ctx context.Context) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM cloud_sync_state WHERE login_id=$1`,
		s.loginID)
	return err
}

func (s *cloudBackfillStore) hasAnyMessages(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 LIMIT 1`,
		s.loginID,
	).Scan(&count)
	return count > 0, err
}

func (s *cloudBackfillStore) setSyncStateError(ctx context.Context, zone, errMsg string) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, last_error, updated_ts)
		VALUES ($1, $2, NULL, $3, $4)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			last_error=excluded.last_error,
			updated_ts=excluded.updated_ts
	`, s.loginID, zone, errMsg, nowMS)
	return err
}

func (s *cloudBackfillStore) upsertChat(
	ctx context.Context,
	cloudChatID, recordName, groupID, portalID, service string,
	displayName *string,
	participants []string,
	updatedTS int64,
) error {
	participantsJSON, err := json.Marshal(participants)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	_, err = s.db.Exec(ctx, `
		INSERT INTO cloud_chat (
			login_id, cloud_chat_id, record_name, group_id, portal_id, service, display_name,
			participants_json, updated_ts, created_ts
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (login_id, cloud_chat_id) DO UPDATE SET
			record_name=excluded.record_name,
			group_id=excluded.group_id,
			portal_id=excluded.portal_id,
			service=excluded.service,
			display_name=excluded.display_name,
			participants_json=excluded.participants_json,
			updated_ts=excluded.updated_ts
	`, s.loginID, cloudChatID, recordName, groupID, portalID, service, nullableString(displayName), string(participantsJSON), updatedTS, nowMS)
	return err
}

func (s *cloudBackfillStore) getChatPortalID(ctx context.Context, cloudChatID string) (string, error) {
	var portalID string
	// Try matching by cloud_chat_id, record_name, or group_id.
	// CloudKit messages reference chats by group_id UUID (the chatID field),
	// while cloud_chat stores chat_identifier as cloud_chat_id and record hash as record_name.
	// Use LOWER() on group_id because CloudKit stores it uppercase but messages reference it lowercase.
	err := s.db.QueryRow(ctx,
		`SELECT portal_id FROM cloud_chat WHERE login_id=$1 AND (cloud_chat_id=$2 OR record_name=$2 OR LOWER(group_id)=LOWER($2))`,
		s.loginID, cloudChatID,
	).Scan(&portalID)
	if err != nil {
		if err == sql.ErrNoRows {
			// Messages use chat_identifier format like "SMS;-;+14158138533" or "iMessage;-;user@example.com"
			// but cloud_chat stores just the identifier part ("+14158138533" or "user@example.com").
			// Try stripping the service prefix.
			if parts := strings.SplitN(cloudChatID, ";-;", 2); len(parts) == 2 {
				return s.getChatPortalID(ctx, parts[1])
			}
			return "", nil
		}
		return "", err
	}
	return portalID, nil
}

func (s *cloudBackfillStore) hasChat(ctx context.Context, cloudChatID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_chat WHERE login_id=$1 AND cloud_chat_id=$2`,
		s.loginID, cloudChatID,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *cloudBackfillStore) getChatParticipantsByPortalID(ctx context.Context, portalID string) ([]string, error) {
	var participantsJSON string
	err := s.db.QueryRow(ctx,
		`SELECT participants_json FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 LIMIT 1`,
		s.loginID, portalID,
	).Scan(&participantsJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var participants []string
	if err = json.Unmarshal([]byte(participantsJSON), &participants); err != nil {
		return nil, err
	}
	// Normalize participants to portal ID format (e.g., tel:+14158138533)
	normalized := make([]string, 0, len(participants))
	for _, p := range participants {
		n := normalizeIdentifierForPortalID(p)
		if n != "" {
			normalized = append(normalized, n)
		}
	}
	return normalized, nil
}

func (s *cloudBackfillStore) hasMessage(ctx context.Context, guid string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND guid=$2`,
		s.loginID, guid,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *cloudBackfillStore) hasPortalMessages(ctx context.Context, portalID string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
	`, s.loginID, portalID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *cloudBackfillStore) upsertMessage(ctx context.Context, row cloudMessageRow) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_message (
			login_id, guid, chat_id, portal_id, timestamp_ms,
			sender, is_from_me, text, service, deleted,
			created_ts, updated_ts
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (login_id, guid) DO UPDATE SET
			chat_id=excluded.chat_id,
			portal_id=excluded.portal_id,
			timestamp_ms=excluded.timestamp_ms,
			sender=excluded.sender,
			is_from_me=excluded.is_from_me,
			text=excluded.text,
			service=excluded.service,
			deleted=excluded.deleted,
			updated_ts=excluded.updated_ts
	`,
		s.loginID, row.GUID, row.CloudChatID, row.PortalID, row.TimestampMS,
		row.Sender, row.IsFromMe, row.Text, row.Service, row.Deleted,
		nowMS, nowMS,
	)
	return err
}

func (s *cloudBackfillStore) listBackwardMessages(
	ctx context.Context,
	portalID string,
	beforeTS int64,
	beforeGUID string,
	count int,
) ([]cloudMessageRow, error) {
	query := `
		SELECT guid, chat_id, portal_id, timestamp_ms, sender, is_from_me, COALESCE(text, ''), COALESCE(service, ''), deleted
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
	`
	args := []any{s.loginID, portalID}
	if beforeTS > 0 || beforeGUID != "" {
		query += ` AND (timestamp_ms < $3 OR (timestamp_ms = $3 AND guid < $4))`
		args = append(args, beforeTS, beforeGUID)
		query += ` ORDER BY timestamp_ms DESC, guid DESC LIMIT $5`
		args = append(args, count)
	} else {
		query += ` ORDER BY timestamp_ms DESC, guid DESC LIMIT $3`
		args = append(args, count)
	}
	return s.queryMessages(ctx, query, args...)
}

func (s *cloudBackfillStore) listForwardMessages(
	ctx context.Context,
	portalID string,
	afterTS int64,
	afterGUID string,
	count int,
) ([]cloudMessageRow, error) {
	query := `
		SELECT guid, chat_id, portal_id, timestamp_ms, sender, is_from_me, COALESCE(text, ''), COALESCE(service, ''), deleted
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
			AND (timestamp_ms > $3 OR (timestamp_ms = $3 AND guid > $4))
		ORDER BY timestamp_ms ASC, guid ASC
		LIMIT $5
	`
	return s.queryMessages(ctx, query, s.loginID, portalID, afterTS, afterGUID, count)
}

func (s *cloudBackfillStore) listLatestMessages(ctx context.Context, portalID string, count int) ([]cloudMessageRow, error) {
	query := `
		SELECT guid, chat_id, portal_id, timestamp_ms, sender, is_from_me, COALESCE(text, ''), COALESCE(service, ''), deleted
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
		ORDER BY timestamp_ms DESC, guid DESC
		LIMIT $3
	`
	return s.queryMessages(ctx, query, s.loginID, portalID, count)
}

func (s *cloudBackfillStore) queryMessages(ctx context.Context, query string, args ...any) ([]cloudMessageRow, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]cloudMessageRow, 0)
	for rows.Next() {
		var row cloudMessageRow
		if err = rows.Scan(
			&row.GUID,
			&row.CloudChatID,
			&row.PortalID,
			&row.TimestampMS,
			&row.Sender,
			&row.IsFromMe,
			&row.Text,
			&row.Service,
			&row.Deleted,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *cloudBackfillStore) listAllPortalIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT portal_id FROM (
			SELECT portal_id FROM cloud_chat
			WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> ''
			UNION
			SELECT portal_id FROM cloud_message
			WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> '' AND deleted=FALSE
		)
		ORDER BY portal_id
	`, s.loginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var portalIDs []string
	for rows.Next() {
		var portalID string
		if err = rows.Scan(&portalID); err != nil {
			return nil, err
		}
		portalIDs = append(portalIDs, portalID)
	}
	return portalIDs, rows.Err()
}

func (s *cloudBackfillStore) listActiveChatsSince(ctx context.Context, sinceTS int64, limit int) ([]cloudActiveChat, error) {
	rows, err := s.db.Query(ctx, `
		SELECT chat_id, portal_id
		FROM cloud_message
		WHERE login_id=$1
			AND deleted=FALSE
			AND chat_id IS NOT NULL
			AND chat_id <> ''
			AND portal_id IS NOT NULL
			AND portal_id <> ''
			AND timestamp_ms >= $2
		GROUP BY chat_id, portal_id
		ORDER BY MAX(timestamp_ms) DESC
		LIMIT $3
	`, s.loginID, sinceTS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []cloudActiveChat
	for rows.Next() {
		var chat cloudActiveChat
		if err = rows.Scan(&chat.CloudChatID, &chat.PortalID); err != nil {
			return nil, err
		}
		chats = append(chats, chat)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return chats, nil
}

func (s *cloudBackfillStore) enqueueRepairTask(
	ctx context.Context,
	taskType, cloudChatID, portalID string,
	sinceTSMS, notBeforeTS int64,
) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_repair_task (
			login_id, task_type, cloud_chat_id, portal_id,
			since_ts_ms, not_before_ts, attempts, created_ts, updated_ts
		) VALUES ($1, $2, $3, $4, $5, $6, 0, $7, $8)
	`, s.loginID, taskType, cloudChatID, portalID, sinceTSMS, notBeforeTS, nowMS, nowMS)
	return err
}

func (s *cloudBackfillStore) getDueRepairTasks(ctx context.Context, limit int) ([]cloudRepairTask, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, task_type, COALESCE(cloud_chat_id, ''), COALESCE(portal_id, ''), since_ts_ms, attempts
		FROM cloud_repair_task
		WHERE login_id=$1
			AND done_ts IS NULL
			AND not_before_ts <= $2
		ORDER BY id ASC
		LIMIT $3
	`, s.loginID, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []cloudRepairTask
	for rows.Next() {
		var task cloudRepairTask
		if err = rows.Scan(&task.ID, &task.TaskType, &task.CloudChatID, &task.PortalID, &task.SinceTSMS, &task.Attempts); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *cloudBackfillStore) markRepairTaskDone(ctx context.Context, id int64) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		UPDATE cloud_repair_task
		SET done_ts=$2, updated_ts=$2
		WHERE login_id=$1 AND id=$3
	`, s.loginID, nowMS, id)
	return err
}

func (s *cloudBackfillStore) markRepairTaskFailed(ctx context.Context, id int64, errMsg string) error {
	nowMS := time.Now().UnixMilli()
	nextRetry := nowMS + int64((5 * time.Minute).Milliseconds())
	_, err := s.db.Exec(ctx, `
		UPDATE cloud_repair_task
		SET attempts=attempts+1,
			last_error=$2,
			not_before_ts=$3,
			updated_ts=$4
		WHERE login_id=$1 AND id=$5
	`, s.loginID, errMsg, nextRetry, nowMS, id)
	return err
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
