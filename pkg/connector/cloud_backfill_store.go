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
	Subject     string
	Service     string
	Deleted     bool

	// Tapback/reaction fields
	TapbackType       *uint32
	TapbackTargetGUID string
	TapbackEmoji      string

	// Attachment metadata JSON (serialized []cloudAttachmentRow)
	AttachmentsJSON string
}

// cloudAttachmentRow holds CloudKit attachment metadata for a single attachment.
type cloudAttachmentRow struct {
	GUID       string `json:"guid"`
	MimeType   string `json:"mime_type,omitempty"`
	UTIType    string `json:"uti_type,omitempty"`
	Filename   string `json:"filename,omitempty"`
	FileSize   int64  `json:"file_size"`
	RecordName string `json:"record_name"`
}

const (
	cloudZoneChats       = "chatManateeZone"
	cloudZoneMessages    = "messageManateeZone"
	cloudZoneAttachments = "attachmentManateeZone"
)

func newCloudBackfillStore(db *dbutil.Database, loginID networkid.UserLoginID) *cloudBackfillStore {
	return &cloudBackfillStore{db: db, loginID: loginID}
}

func (s *cloudBackfillStore) ensureSchema(ctx context.Context) error {
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
			subject TEXT,
			service TEXT,
			deleted BOOLEAN NOT NULL DEFAULT FALSE,
			tapback_type INTEGER,
			tapback_target_guid TEXT,
			tapback_emoji TEXT,
			attachments_json TEXT,
			created_ts BIGINT NOT NULL,
			updated_ts BIGINT NOT NULL,
			PRIMARY KEY (login_id, guid)
		)`,
		`CREATE INDEX IF NOT EXISTS cloud_chat_portal_idx
			ON cloud_chat (login_id, portal_id, cloud_chat_id)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_portal_ts_idx
			ON cloud_message (login_id, portal_id, timestamp_ms, guid)`,
		`CREATE INDEX IF NOT EXISTS cloud_message_chat_ts_idx
			ON cloud_message (login_id, chat_id, timestamp_ms, guid)`,
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

	// Migration: add rich content columns to cloud_message if missing
	richCols := []struct {
		name string
		def  string
	}{
		{"subject", "TEXT"},
		{"tapback_type", "INTEGER"},
		{"tapback_target_guid", "TEXT"},
		{"tapback_emoji", "TEXT"},
		{"attachments_json", "TEXT"},
	}
	for _, col := range richCols {
		var exists int
		_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('cloud_message') WHERE name=$1`, col.name).Scan(&exists)
		if exists == 0 {
			if _, err := s.db.Exec(ctx, fmt.Sprintf(`ALTER TABLE cloud_message ADD COLUMN %s %s`, col.name, col.def)); err != nil {
				return fmt.Errorf("failed to add %s column: %w", col.name, err)
			}
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

// clearAllData removes all cloud cache data for this login: sync tokens,
// cached chats, and cached messages. Used on fresh bootstrap when the bridge
// DB was reset but the cloud tables survived.
func (s *cloudBackfillStore) clearAllData(ctx context.Context) error {
	for _, table := range []string{"cloud_sync_state", "cloud_chat", "cloud_message"} {
		if _, err := s.db.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE login_id=$1`, table),
			s.loginID,
		); err != nil {
			return fmt.Errorf("failed to clear %s: %w", table, err)
		}
	}
	return nil
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

// beginTx starts a database transaction for batch operations.
func (s *cloudBackfillStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.RawDB.BeginTx(ctx, nil)
}

// upsertMessageBatch inserts multiple messages in a single transaction.
func (s *cloudBackfillStore) upsertMessageBatch(ctx context.Context, rows []cloudMessageRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO cloud_message (
			login_id, guid, chat_id, portal_id, timestamp_ms,
			sender, is_from_me, text, subject, service, deleted,
			tapback_type, tapback_target_guid, tapback_emoji,
			attachments_json,
			created_ts, updated_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (login_id, guid) DO UPDATE SET
			chat_id=excluded.chat_id,
			portal_id=excluded.portal_id,
			timestamp_ms=excluded.timestamp_ms,
			sender=excluded.sender,
			is_from_me=excluded.is_from_me,
			text=excluded.text,
			subject=excluded.subject,
			service=excluded.service,
			deleted=excluded.deleted,
			tapback_type=excluded.tapback_type,
			tapback_target_guid=excluded.tapback_target_guid,
			tapback_emoji=excluded.tapback_emoji,
			attachments_json=excluded.attachments_json,
			updated_ts=excluded.updated_ts
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare batch statement: %w", err)
	}
	defer stmt.Close()

	nowMS := time.Now().UnixMilli()
	for _, row := range rows {
		_, err = stmt.ExecContext(ctx,
			s.loginID, row.GUID, row.CloudChatID, row.PortalID, row.TimestampMS,
			row.Sender, row.IsFromMe, row.Text, row.Subject, row.Service, row.Deleted,
			row.TapbackType, row.TapbackTargetGUID, row.TapbackEmoji,
			row.AttachmentsJSON,
			nowMS, nowMS,
		)
		if err != nil {
			return fmt.Errorf("failed to insert message %s: %w", row.GUID, err)
		}
	}

	return tx.Commit()
}

// deleteMessageBatch removes messages by GUID in a single transaction.
func (s *cloudBackfillStore) deleteMessageBatch(ctx context.Context, guids []string) error {
	if len(guids) == 0 {
		return nil
	}
	const chunkSize = 500
	for i := 0; i < len(guids); i += chunkSize {
		end := i + chunkSize
		if end > len(guids) {
			end = len(guids)
		}
		chunk := guids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, g := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, g)
		}

		query := fmt.Sprintf(
			`DELETE FROM cloud_message WHERE login_id=$1 AND guid IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to delete message batch: %w", err)
		}
	}
	return nil
}

// deleteChatBatch removes chats by cloud_chat_id in a single transaction.
func (s *cloudBackfillStore) deleteChatBatch(ctx context.Context, chatIDs []string) error {
	if len(chatIDs) == 0 {
		return nil
	}
	const chunkSize = 500
	for i := 0; i < len(chatIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(chatIDs) {
			end = len(chatIDs)
		}
		chunk := chatIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, id := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`DELETE FROM cloud_chat WHERE login_id=$1 AND cloud_chat_id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to delete chat batch: %w", err)
		}
	}
	return nil
}

// upsertChatBatch inserts multiple chats in a single transaction.
func (s *cloudBackfillStore) upsertChatBatch(ctx context.Context, chats []cloudChatUpsertRow) error {
	if len(chats) == 0 {
		return nil
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO cloud_chat (
			login_id, cloud_chat_id, record_name, group_id, portal_id, service, display_name,
			participants_json, updated_ts, created_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (login_id, cloud_chat_id) DO UPDATE SET
			record_name=excluded.record_name,
			group_id=excluded.group_id,
			portal_id=excluded.portal_id,
			service=excluded.service,
			display_name=excluded.display_name,
			participants_json=excluded.participants_json,
			updated_ts=excluded.updated_ts
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare batch statement: %w", err)
	}
	defer stmt.Close()

	nowMS := time.Now().UnixMilli()
	for _, chat := range chats {
		_, err = stmt.ExecContext(ctx,
			s.loginID, chat.CloudChatID, chat.RecordName, chat.GroupID,
			chat.PortalID, chat.Service, chat.DisplayName,
			chat.ParticipantsJSON, chat.UpdatedTS, nowMS,
		)
		if err != nil {
			return fmt.Errorf("failed to insert chat %s: %w", chat.CloudChatID, err)
		}
	}

	return tx.Commit()
}

// hasMessageBatch checks existence of multiple GUIDs in a single query and
// returns the set of GUIDs that already exist.
func (s *cloudBackfillStore) hasMessageBatch(ctx context.Context, guids []string) (map[string]bool, error) {
	if len(guids) == 0 {
		return nil, nil
	}
	existing := make(map[string]bool, len(guids))
	// SQLite has a limit on the number of variables. Process in chunks.
	const chunkSize = 500
	for i := 0; i < len(guids); i += chunkSize {
		end := i + chunkSize
		if end > len(guids) {
			end = len(guids)
		}
		chunk := guids[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, g := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, g)
		}

		query := fmt.Sprintf(
			`SELECT guid FROM cloud_message WHERE login_id=$1 AND guid IN (%s)`,
			strings.Join(placeholders, ","),
		)
		rows, err := s.db.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var guid string
			if err := rows.Scan(&guid); err != nil {
				rows.Close()
				return nil, err
			}
			existing[guid] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// cloudChatUpsertRow holds the pre-serialized data for a batch chat upsert.
type cloudChatUpsertRow struct {
	CloudChatID      string
	RecordName       string
	GroupID          string
	PortalID         string
	Service          string
	DisplayName      any // nil or string
	ParticipantsJSON string
	UpdatedTS        int64
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

// hasChatBatch checks existence of multiple cloud chat IDs in a single query
// and returns the set of IDs that already exist.
func (s *cloudBackfillStore) hasChatBatch(ctx context.Context, chatIDs []string) (map[string]bool, error) {
	if len(chatIDs) == 0 {
		return nil, nil
	}
	existing := make(map[string]bool, len(chatIDs))
	const chunkSize = 500
	for i := 0; i < len(chatIDs); i += chunkSize {
		end := i + chunkSize
		if end > len(chatIDs) {
			end = len(chatIDs)
		}
		chunk := chatIDs[i:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, s.loginID)
		for j, id := range chunk {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`SELECT cloud_chat_id FROM cloud_chat WHERE login_id=$1 AND cloud_chat_id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		rows, err := s.db.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			existing[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return existing, nil
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
			sender, is_from_me, text, subject, service, deleted,
			tapback_type, tapback_target_guid, tapback_emoji,
			attachments_json,
			created_ts, updated_ts
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (login_id, guid) DO UPDATE SET
			chat_id=excluded.chat_id,
			portal_id=excluded.portal_id,
			timestamp_ms=excluded.timestamp_ms,
			sender=excluded.sender,
			is_from_me=excluded.is_from_me,
			text=excluded.text,
			subject=excluded.subject,
			service=excluded.service,
			deleted=excluded.deleted,
			tapback_type=excluded.tapback_type,
			tapback_target_guid=excluded.tapback_target_guid,
			tapback_emoji=excluded.tapback_emoji,
			attachments_json=excluded.attachments_json,
			updated_ts=excluded.updated_ts
	`,
		s.loginID, row.GUID, row.CloudChatID, row.PortalID, row.TimestampMS,
		row.Sender, row.IsFromMe, row.Text, row.Subject, row.Service, row.Deleted,
		row.TapbackType, row.TapbackTargetGUID, row.TapbackEmoji,
		row.AttachmentsJSON,
		nowMS, nowMS,
	)
	return err
}

const cloudMessageSelectCols = `guid, chat_id, portal_id, timestamp_ms, sender, is_from_me,
	COALESCE(text, ''), COALESCE(subject, ''), COALESCE(service, ''), deleted,
	tapback_type, COALESCE(tapback_target_guid, ''), COALESCE(tapback_emoji, ''),
	COALESCE(attachments_json, '')`

func (s *cloudBackfillStore) listBackwardMessages(
	ctx context.Context,
	portalID string,
	beforeTS int64,
	beforeGUID string,
	count int,
) ([]cloudMessageRow, error) {
	query := `SELECT ` + cloudMessageSelectCols + `
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
	query := `SELECT ` + cloudMessageSelectCols + `
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
			AND (timestamp_ms > $3 OR (timestamp_ms = $3 AND guid > $4))
		ORDER BY timestamp_ms ASC, guid ASC
		LIMIT $5
	`
	return s.queryMessages(ctx, query, s.loginID, portalID, afterTS, afterGUID, count)
}

func (s *cloudBackfillStore) listLatestMessages(ctx context.Context, portalID string, count int) ([]cloudMessageRow, error) {
	query := `SELECT ` + cloudMessageSelectCols + `
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
			&row.Subject,
			&row.Service,
			&row.Deleted,
			&row.TapbackType,
			&row.TapbackTargetGUID,
			&row.TapbackEmoji,
			&row.AttachmentsJSON,
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

// portalWithNewestMessage pairs a portal ID with its newest message timestamp
// and message count. Used to prioritize portal creation during initial sync.
type portalWithNewestMessage struct {
	PortalID     string
	NewestTS     int64
	MessageCount int
}

// listPortalIDsWithNewestTimestamp returns all portal IDs that have at least
// one non-deleted message, ordered by newest message timestamp descending
// (most recent activity first).
func (s *cloudBackfillStore) listPortalIDsWithNewestTimestamp(ctx context.Context) ([]portalWithNewestMessage, error) {
	rows, err := s.db.Query(ctx, `
		SELECT portal_id, MAX(timestamp_ms) AS newest_ts, COUNT(*) AS msg_count
		FROM cloud_message
		WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> '' AND deleted=FALSE
		GROUP BY portal_id
		ORDER BY newest_ts DESC
	`, s.loginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []portalWithNewestMessage
	for rows.Next() {
		var p portalWithNewestMessage
		if err = rows.Scan(&p.PortalID, &p.NewestTS, &p.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
