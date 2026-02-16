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
	RecordName  string
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

	// Migration: add record_name column to cloud_message if missing
	var hasMsgRecordName int
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM pragma_table_info('cloud_message') WHERE name='record_name'`).Scan(&hasMsgRecordName)
	if hasMsgRecordName == 0 {
		if _, err := s.db.Exec(ctx, `ALTER TABLE cloud_message ADD COLUMN record_name TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("failed to add record_name column to cloud_message: %w", err)
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

	// Deleted portal tracking: remembers portals that were explicitly deleted
	// (from Apple device or Beeper) so CloudKit re-import doesn't resurrect
	// them if the background CloudKit deletion was interrupted by a restart.
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS deleted_portal (
		login_id TEXT NOT NULL,
		portal_id TEXT NOT NULL,
		deleted_ts BIGINT NOT NULL,
		conv_hash TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (login_id, portal_id)
	)`); err != nil {
		return fmt.Errorf("failed to create deleted_portal table: %w", err)
	}
	// Migration: add conv_hash column if table already exists without it.
	_, _ = s.db.Exec(ctx, `ALTER TABLE deleted_portal ADD COLUMN conv_hash TEXT NOT NULL DEFAULT ''`)

	// Pending CloudKit deletions: persists deletion intent so interrupted
	// background scans (findAndDeleteCloud*) are retried on next startup.
	// Without this, a restart between local DB cleanup and CloudKit deletion
	// leaves remote records alive → fresh sync resurrects the portal.
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS pending_cloud_deletion (
		login_id TEXT NOT NULL,
		portal_id TEXT NOT NULL,
		chat_identifier TEXT NOT NULL DEFAULT '',
		group_id TEXT NOT NULL DEFAULT '',
		created_ts BIGINT NOT NULL,
		PRIMARY KEY (login_id, portal_id)
	)`); err != nil {
		return fmt.Errorf("failed to create pending_cloud_deletion table: %w", err)
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

// clearSyncTokens removes only the sync continuation tokens for this login,
// forcing the next sync to re-download all records from CloudKit.
// Preserves cloud_chat, cloud_message, deleted_portal, and the _version row.
func (s *cloudBackfillStore) clearSyncTokens(ctx context.Context) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM cloud_sync_state WHERE login_id=$1 AND zone != '_version'`,
		s.loginID,
	)
	return err
}

// clearZoneToken removes the continuation token for a specific zone,
// forcing the next sync for that zone to start from scratch.
func (s *cloudBackfillStore) clearZoneToken(ctx context.Context, zone string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM cloud_sync_state WHERE login_id=$1 AND zone=$2`,
		s.loginID, zone,
	)
	return err
}

// getSyncVersion returns the stored sync schema version (0 if never set).
func (s *cloudBackfillStore) getSyncVersion(ctx context.Context) (int, error) {
	var token sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT continuation_token FROM cloud_sync_state WHERE login_id=$1 AND zone='_version'`,
		s.loginID,
	).Scan(&token)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	if !token.Valid {
		return 0, nil
	}
	v := 0
	fmt.Sscanf(token.String, "%d", &v)
	return v, nil
}

// setSyncVersion stores the sync schema version.
func (s *cloudBackfillStore) setSyncVersion(ctx context.Context, version int) error {
	nowMS := time.Now().UnixMilli()
	vStr := fmt.Sprintf("%d", version)
	_, err := s.db.Exec(ctx, `
		INSERT INTO cloud_sync_state (login_id, zone, continuation_token, updated_ts)
		VALUES ($1, '_version', $2, $3)
		ON CONFLICT (login_id, zone) DO UPDATE SET
			continuation_token=excluded.continuation_token,
			updated_ts=excluded.updated_ts
	`, s.loginID, vStr, nowMS)
	return err
}

// clearAllData removes cloud cache data for this login: sync tokens,
// cached chats, and cached messages. Used on fresh bootstrap when the bridge
// DB was reset but the cloud tables survived.
//
// IMPORTANT: deleted_portal and pending_cloud_deletion are intentionally
// PRESERVED across resets. These are the safety nets that prevent resurrection
// if CloudKit records weren't fully deleted. Without them, a fresh sync
// re-downloads surviving CloudKit records and recreates deleted portals.
// The only way to clear deleted_portal is a genuinely new message arriving.
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

// hasAnySyncState checks whether any sync state rows exist for this login.
// Used to detect an interrupted sync — if tokens exist but no portals were
// created yet, the sync was interrupted mid-flight and should resume, NOT restart.
func (s *cloudBackfillStore) hasAnySyncState(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_sync_state WHERE login_id=$1`,
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
			login_id, guid, record_name, chat_id, portal_id, timestamp_ms,
			sender, is_from_me, text, subject, service, deleted,
			tapback_type, tapback_target_guid, tapback_emoji,
			attachments_json,
			created_ts, updated_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (login_id, guid) DO UPDATE SET
			record_name=excluded.record_name,
			chat_id=excluded.chat_id,
			portal_id=excluded.portal_id,
			timestamp_ms=excluded.timestamp_ms,
			sender=excluded.sender,
			is_from_me=excluded.is_from_me,
			text=excluded.text,
			subject=excluded.subject,
			service=excluded.service,
			deleted=CASE WHEN cloud_message.deleted THEN cloud_message.deleted ELSE excluded.deleted END,
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
			s.loginID, row.GUID, row.RecordName, row.CloudChatID, row.PortalID, row.TimestampMS,
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

// deleteMessagesByChatIDs removes messages whose chat_id matches any of the
// given cloud_chat_ids. This prevents orphaned messages from keeping portals
// alive after their parent chat is deleted from CloudKit.
func (s *cloudBackfillStore) deleteMessagesByChatIDs(ctx context.Context, chatIDs []string) error {
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
			`DELETE FROM cloud_message WHERE login_id=$1 AND chat_id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := s.db.Exec(ctx, query, args...); err != nil {
			return fmt.Errorf("failed to delete messages by chat ID: %w", err)
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

// getDisplayNameByPortalID returns the CloudKit display_name for a given portal_id.
// This is the user-set group name (cv_name from the iMessage protocol), NOT an
// auto-generated label. Returns empty string if none found or if the group is unnamed.
func (s *cloudBackfillStore) getDisplayNameByPortalID(ctx context.Context, portalID string) (string, error) {
	var displayName sql.NullString
	err := s.db.QueryRow(ctx,
		`SELECT display_name FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND display_name IS NOT NULL AND display_name <> '' LIMIT 1`,
		s.loginID, portalID,
	).Scan(&displayName)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	if displayName.Valid {
		return displayName.String, nil
	}
	return "", nil
}

// getCloudChatIDByPortalID returns the cloud_chat_id (Apple's chat GUID, e.g.
// "iMessage;-;user@example.com") for a given portal_id. Returns empty string if none found.
func (s *cloudBackfillStore) getCloudChatIDByPortalID(ctx context.Context, portalID string) (string, error) {
	var chatID string
	err := s.db.QueryRow(ctx,
		`SELECT cloud_chat_id FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 LIMIT 1`,
		s.loginID, portalID,
	).Scan(&chatID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return chatID, nil
}

// getCloudChatIDByGroupID returns the cloud_chat_id for any chat matching a group_id.
func (s *cloudBackfillStore) getCloudChatIDByGroupID(ctx context.Context, groupID string) (string, error) {
	var chatID string
	err := s.db.QueryRow(ctx,
		`SELECT cloud_chat_id FROM cloud_chat WHERE login_id=$1 AND LOWER(group_id)=LOWER($2) LIMIT 1`,
		s.loginID, groupID,
	).Scan(&chatID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return chatID, nil
}

// getCloudRecordNamesByPortalID returns ALL CloudKit record_names for chat records
// belonging to a portal. Multiple cloud_chat rows can map to the same portal_id
// (e.g., one from APNs with empty record_name, one from CloudKit with a real one).
func (s *cloudBackfillStore) getCloudRecordNamesByPortalID(ctx context.Context, portalID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name FROM cloud_chat WHERE login_id=$1 AND portal_id=$2 AND record_name <> ''`,
		s.loginID, portalID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// deleteLocalChatByPortalID removes cloud_chat records and marks cloud_message
// records as deleted for a given portal_id. cloud_message records are KEPT
// (not physically deleted) so their GUIDs serve as a persisted echo-detection
// set — if Apple re-delivers these messages via APNs, we can check the UUID
// against cloud_message to identify them as echoes. Setting deleted=TRUE
// prevents backfill from pulling old messages into a recreated portal (backfill
// queries filter deleted=FALSE). The records are fully purged when the
// deleted_portal entry is cleared (genuinely new conversation detected).
func (s *cloudBackfillStore) deleteLocalChatByPortalID(ctx context.Context, portalID string) error {
	if _, err := s.db.Exec(ctx,
		`DELETE FROM cloud_chat WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	); err != nil {
		return fmt.Errorf("failed to delete cloud_chat records for portal %s: %w", portalID, err)
	}
	// Mark cloud_message records as deleted so backfill won't return them.
	// Records are kept (not physically deleted) for UUID-based echo detection
	// (hasMessageUUID doesn't check the deleted flag).
	if _, err := s.db.Exec(ctx,
		`UPDATE cloud_message SET deleted=TRUE, updated_ts=$3 WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE`,
		s.loginID, portalID, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("failed to mark cloud_message records as deleted for portal %s: %w", portalID, err)
	}
	return nil
}

// persistMessageUUID inserts a minimal cloud_message record for a realtime
// APNs message so the UUID survives restarts. CloudKit-synced messages are
// already stored via upsertMessageBatch; this covers the realtime path.
// Uses INSERT OR IGNORE so it's safe to call even if the message already exists.
func (s *cloudBackfillStore) persistMessageUUID(ctx context.Context, uuid, portalID string, timestampMS int64, isFromMe bool) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT OR IGNORE INTO cloud_message (login_id, guid, portal_id, timestamp_ms, is_from_me, created_ts, updated_ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, s.loginID, uuid, portalID, timestampMS, isFromMe, nowMS, nowMS)
	return err
}

// hasMessageUUID checks if a message UUID exists in cloud_message for this login.
// Used for echo detection: if the UUID is known, the message is an echo of a
// previously-seen message and should not create a new portal.
func (s *cloudBackfillStore) hasMessageUUID(ctx context.Context, uuid string) (bool, error) {
	var count int
	err := s.db.QueryRow(ctx,
		`SELECT COUNT(*) FROM cloud_message WHERE login_id=$1 AND guid=$2 LIMIT 1`,
		s.loginID, uuid,
	).Scan(&count)
	return count > 0, err
}

// purgeCloudMessagesByPortalID deletes all cloud_message records for a portal.
// Called when a deleted_portal entry is cleared (genuinely new conversation),
// so old message UUIDs don't block future messages.
func (s *cloudBackfillStore) purgeCloudMessagesByPortalID(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM cloud_message WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	return err
}

// getPortalIDByGroupID looks up the portal_id for a given group UUID in the
// cloud_chat table. Used as a fallback when the delete target's group UUID
// doesn't directly match a portal key.
func (s *cloudBackfillStore) getPortalIDByGroupID(ctx context.Context, groupID string) (string, error) {
	var portalID string
	err := s.db.QueryRow(ctx,
		`SELECT portal_id FROM cloud_chat WHERE login_id=$1 AND LOWER(group_id)=LOWER($2) AND portal_id <> '' LIMIT 1`,
		s.loginID, groupID,
	).Scan(&portalID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return portalID, nil
}

// findPortalIDsByParticipants returns all distinct portal_ids from cloud_chat
// whose participants overlap with the given normalized participant list.
// Used to find duplicate group portals that have the same members but different
// group UUIDs. Participants are compared after normalization (tel:/mailto: prefix).
func (s *cloudBackfillStore) findPortalIDsByParticipants(ctx context.Context, normalizedTarget []string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT portal_id, participants_json FROM cloud_chat WHERE login_id=$1 AND portal_id <> ''`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Build a set of target participants for fast lookup.
	targetSet := make(map[string]bool, len(normalizedTarget))
	for _, p := range normalizedTarget {
		targetSet[p] = true
	}

	var matches []string
	seen := make(map[string]bool)
	for rows.Next() {
		var portalID, participantsJSON string
		if err = rows.Scan(&portalID, &participantsJSON); err != nil {
			return nil, err
		}
		if seen[portalID] {
			continue
		}
		var participants []string
		if err = json.Unmarshal([]byte(participantsJSON), &participants); err != nil {
			continue
		}
		// Normalize and check overlap: match if all non-self participants overlap.
		normalized := make([]string, 0, len(participants))
		for _, p := range participants {
			n := normalizeIdentifierForPortalID(p)
			if n != "" {
				normalized = append(normalized, n)
			}
		}
		if participantSetsMatch(normalized, normalizedTarget) {
			matches = append(matches, portalID)
			seen[portalID] = true
		}
	}
	return matches, rows.Err()
}

// participantSetsMatch checks if two normalized participant sets are equivalent
// (same members, ignoring order). Allows ±1 member difference to handle cases
// where self is included in one set but not the other.
func participantSetsMatch(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	setA := make(map[string]bool, len(a))
	for _, p := range a {
		setA[p] = true
	}
	setB := make(map[string]bool, len(b))
	for _, p := range b {
		setB[p] = true
	}
	// Count members in A not in B, and vice versa.
	diff := 0
	for p := range setA {
		if !setB[p] {
			diff++
		}
	}
	for p := range setB {
		if !setA[p] {
			diff++
		}
	}
	// Allow ±1 difference (self may be in one set but not the other).
	return diff <= 1
}

// getMessageRecordNamesByPortalID returns all CloudKit record_names for messages
// belonging to a portal. Used when deleting a chat to also delete its messages
// from CloudKit so they don't reappear during future syncs.
func (s *cloudBackfillStore) getMessageRecordNamesByPortalID(ctx context.Context, portalID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name FROM cloud_message WHERE login_id=$1 AND portal_id=$2 AND record_name <> ''`,
		s.loginID, portalID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// getCloudRecordNamesByGroupID returns all non-empty chat record_names for ANY
// portal_id that shares the given group_id. Used as a fallback when the portal
// being deleted was created from APNs (empty record_name) but CloudKit has
// records under a different portal_id for the same logical group.
func (s *cloudBackfillStore) getCloudRecordNamesByGroupID(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT record_name FROM cloud_chat WHERE login_id=$1 AND LOWER(group_id)=LOWER($2) AND record_name <> ''`,
		s.loginID, groupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// getMessageRecordNamesByGroupID returns all non-empty message record_names
// for portals that share the given group_id.
func (s *cloudBackfillStore) getMessageRecordNamesByGroupID(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT cm.record_name FROM cloud_message cm
		INNER JOIN cloud_chat cc ON cc.login_id=cm.login_id AND cc.portal_id=cm.portal_id
		WHERE cm.login_id=$1 AND LOWER(cc.group_id)=LOWER($2) AND cm.record_name <> ''
	`, s.loginID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// deleteLocalChatByGroupID removes all local cloud_chat and cloud_message records
// for any portal_id that shares the given group_id.
func (s *cloudBackfillStore) deleteLocalChatByGroupID(ctx context.Context, groupID string) error {
	// Find all portal_ids for this group
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT portal_id FROM cloud_chat WHERE login_id=$1 AND LOWER(group_id)=LOWER($2) AND portal_id <> ''`,
		s.loginID, groupID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var portalIDs []string
	for rows.Next() {
		var pid string
		if err = rows.Scan(&pid); err != nil {
			return err
		}
		portalIDs = append(portalIDs, pid)
	}
	if err = rows.Err(); err != nil {
		return err
	}

	for _, pid := range portalIDs {
		if err := s.deleteLocalChatByPortalID(ctx, pid); err != nil {
			return err
		}
	}
	return nil
}

// getOldestMessageTimestamp returns the oldest non-deleted message timestamp
// for a portal, or 0 if no messages exist.
func (s *cloudBackfillStore) getOldestMessageTimestamp(ctx context.Context, portalID string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(ctx, `
		SELECT MIN(timestamp_ms)
		FROM cloud_message
		WHERE login_id=$1 AND portal_id=$2 AND deleted=FALSE
	`, s.loginID, portalID).Scan(&ts)
	if err != nil || !ts.Valid {
		return 0, err
	}
	return ts.Int64, nil
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

const cloudMessageSelectCols = `guid, COALESCE(chat_id, ''), portal_id, timestamp_ms, COALESCE(sender, ''), is_from_me,
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

// portalWithNewestMessage pairs a portal ID with its newest message timestamp
// and message count. Used to prioritize portal creation during initial sync.
type portalWithNewestMessage struct {
	PortalID     string
	NewestTS     int64
	MessageCount int
}

// listPortalIDsWithNewestTimestamp returns all portal IDs from both messages
// and chat records, ordered by newest message timestamp descending (most
// recent activity first). Chat-only portals (no messages) are included with
// their updated_ts from the cloud_chat table so they still get portals created.
func (s *cloudBackfillStore) listPortalIDsWithNewestTimestamp(ctx context.Context) ([]portalWithNewestMessage, error) {
	rows, err := s.db.Query(ctx, `
		SELECT portal_id, MAX(newest_ts) AS newest_ts, SUM(msg_count) AS msg_count FROM (
			SELECT portal_id, MAX(timestamp_ms) AS newest_ts, COUNT(*) AS msg_count
			FROM cloud_message
			WHERE login_id=$1 AND portal_id IS NOT NULL AND portal_id <> '' AND deleted=FALSE
			GROUP BY portal_id

			UNION ALL

			SELECT cc.portal_id, COALESCE(cc.updated_ts, 0) AS newest_ts, 0 AS msg_count
			FROM cloud_chat cc
			WHERE cc.login_id=$1 AND cc.portal_id IS NOT NULL AND cc.portal_id <> ''
			AND cc.portal_id NOT IN (
				SELECT DISTINCT cm.portal_id FROM cloud_message cm
				WHERE cm.login_id=$1 AND cm.portal_id IS NOT NULL AND cm.portal_id <> '' AND cm.deleted=FALSE
			)
		)
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

// recordDeletedPortal marks a portal as deleted so CloudKit re-import won't
// resurrect it. The conv_hash uniquely fingerprints this deletion event
// (includes timestamp). The deleted_ts is used for timestamp comparison to
// block messages from before the deletion while allowing genuinely new ones.
func (s *cloudBackfillStore) recordDeletedPortal(ctx context.Context, portalID string, convHash string, deletedTS int64) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO deleted_portal (login_id, portal_id, deleted_ts, conv_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (login_id, portal_id) DO UPDATE SET deleted_ts=excluded.deleted_ts, conv_hash=excluded.conv_hash
	`, s.loginID, portalID, deletedTS, convHash)
	return err
}

// deletedPortalEntry holds the deletion timestamp and conversation hash
// for a deleted portal.
type deletedPortalEntry struct {
	DeletedTS int64
	ConvHash  string
}

// getDeletedPortals returns a map of portal_id → deletedPortalEntry for all
// portals that were explicitly deleted.
func (s *cloudBackfillStore) getDeletedPortals(ctx context.Context) (map[string]deletedPortalEntry, error) {
	rows, err := s.db.Query(ctx,
		`SELECT portal_id, deleted_ts, conv_hash FROM deleted_portal WHERE login_id=$1`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]deletedPortalEntry)
	for rows.Next() {
		var portalID string
		var entry deletedPortalEntry
		if err = rows.Scan(&portalID, &entry.DeletedTS, &entry.ConvHash); err != nil {
			return nil, err
		}
		out[portalID] = entry
	}
	return out, rows.Err()
}

// clearDeletedPortal removes a portal from the deleted list, e.g. when
// a genuinely new conversation is detected with newer messages.
func (s *cloudBackfillStore) clearDeletedPortal(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM deleted_portal WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	return err
}

// deleteMessagesBeforeTimestamp removes cloud_message rows for a portal that
// are older than the given timestamp (milliseconds). Used when a deleted portal
// is re-initiated — only messages AFTER the deletion should backfill.
func (s *cloudBackfillStore) deleteMessagesBeforeTimestamp(ctx context.Context, portalID string, beforeTS int64) (int64, error) {
	result, err := s.db.Exec(ctx,
		`DELETE FROM cloud_message WHERE login_id=$1 AND portal_id=$2 AND timestamp_ms <= $3`,
		s.loginID, portalID, beforeTS,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// pendingCloudDeletion holds the persisted intent to delete CloudKit records
// for a portal. Used to retry interrupted background deletions on startup.
type pendingCloudDeletion struct {
	PortalID       string
	ChatIdentifier string
	GroupID        string
	CreatedTS      int64
}

// recordPendingCloudDeletion persists the intent to delete CloudKit records
// for a portal. Must be called BEFORE starting the actual CloudKit deletion
// so that restarts can retry. Idempotent (upserts).
func (s *cloudBackfillStore) recordPendingCloudDeletion(ctx context.Context, portalID, chatIdentifier, groupID string) error {
	nowMS := time.Now().UnixMilli()
	_, err := s.db.Exec(ctx, `
		INSERT INTO pending_cloud_deletion (login_id, portal_id, chat_identifier, group_id, created_ts)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (login_id, portal_id) DO UPDATE SET
			chat_identifier=excluded.chat_identifier,
			group_id=excluded.group_id,
			created_ts=excluded.created_ts
	`, s.loginID, portalID, chatIdentifier, groupID, nowMS)
	return err
}

// getPendingCloudDeletions returns all pending CloudKit deletions for this login.
func (s *cloudBackfillStore) getPendingCloudDeletions(ctx context.Context) ([]pendingCloudDeletion, error) {
	rows, err := s.db.Query(ctx,
		`SELECT portal_id, chat_identifier, group_id, created_ts FROM pending_cloud_deletion WHERE login_id=$1`,
		s.loginID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []pendingCloudDeletion
	for rows.Next() {
		var d pendingCloudDeletion
		if err = rows.Scan(&d.PortalID, &d.ChatIdentifier, &d.GroupID, &d.CreatedTS); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// clearPendingCloudDeletion removes a completed deletion from the pending list.
func (s *cloudBackfillStore) clearPendingCloudDeletion(ctx context.Context, portalID string) error {
	_, err := s.db.Exec(ctx,
		`DELETE FROM pending_cloud_deletion WHERE login_id=$1 AND portal_id=$2`,
		s.loginID, portalID,
	)
	return err
}
