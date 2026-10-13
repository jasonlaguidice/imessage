-- v1: Create connector-owned tables
-- Every statement is IF NOT EXISTS: databases created before versioned
-- migrations already hold these tables (possibly with older column sets,
-- which v2 brings up to date), and they start at version 0 like a fresh one.
CREATE TABLE IF NOT EXISTS cloud_sync_state (
	login_id           TEXT   NOT NULL,
	zone               TEXT   NOT NULL,
	continuation_token TEXT,
	last_success_ts    BIGINT,
	last_error         TEXT,
	updated_ts         BIGINT NOT NULL,
	PRIMARY KEY (login_id, zone)
);

CREATE TABLE IF NOT EXISTS cloud_chat (
	login_id          TEXT    NOT NULL,
	cloud_chat_id     TEXT    NOT NULL,
	record_name       TEXT    NOT NULL DEFAULT '',
	group_id          TEXT    NOT NULL DEFAULT '',
	portal_id         TEXT    NOT NULL,
	service           TEXT,
	display_name      TEXT,
	group_photo_guid  TEXT,
	participants_json TEXT,
	updated_ts        BIGINT,
	created_ts        BIGINT  NOT NULL,
	deleted           BOOLEAN NOT NULL DEFAULT FALSE,
	is_filtered       INTEGER NOT NULL DEFAULT 0,
	fwd_backfill_done BOOLEAN NOT NULL DEFAULT FALSE,
	PRIMARY KEY (login_id, cloud_chat_id)
);

CREATE TABLE IF NOT EXISTS cloud_message (
	login_id            TEXT    NOT NULL,
	guid                TEXT    NOT NULL,
	chat_id             TEXT,
	portal_id           TEXT,
	timestamp_ms        BIGINT  NOT NULL,
	sender              TEXT,
	is_from_me          BOOLEAN NOT NULL,
	text                TEXT,
	subject             TEXT,
	service             TEXT,
	deleted             BOOLEAN NOT NULL DEFAULT FALSE,
	tapback_type        INTEGER,
	tapback_target_guid TEXT,
	tapback_emoji       TEXT,
	attachments_json    TEXT,
	created_ts          BIGINT  NOT NULL,
	updated_ts          BIGINT  NOT NULL,
	date_read_ms        BIGINT  NOT NULL DEFAULT 0,
	record_name         TEXT    NOT NULL DEFAULT '',
	has_body            BOOLEAN NOT NULL DEFAULT TRUE,
	PRIMARY KEY (login_id, guid)
);

CREATE TABLE IF NOT EXISTS group_photo_cache (
	login_id  TEXT   NOT NULL,
	portal_id TEXT   NOT NULL,
	ts        BIGINT NOT NULL,
	data      BYTEA  NOT NULL,
	PRIMARY KEY (login_id, portal_id)
);

CREATE TABLE IF NOT EXISTS restore_override (
	login_id   TEXT   NOT NULL,
	portal_id  TEXT   NOT NULL,
	updated_ts BIGINT NOT NULL,
	PRIMARY KEY (login_id, portal_id)
);

-- Persists record_name -> MessageEventContent JSON so mxc URIs survive
-- bridge restarts and pre-upload never re-downloads cached attachments.
CREATE TABLE IF NOT EXISTS cloud_attachment_cache (
	login_id     TEXT   NOT NULL,
	record_name  TEXT   NOT NULL,
	content_json BYTEA  NOT NULL,
	created_ts   BIGINT NOT NULL,
	PRIMARY KEY (login_id, record_name)
);

CREATE TABLE IF NOT EXISTS shared_profiles (
	login_id       TEXT    NOT NULL,
	identifier     TEXT    NOT NULL,
	display_name   TEXT    NOT NULL DEFAULT '',
	first_name     TEXT    NOT NULL DEFAULT '',
	last_name      TEXT    NOT NULL DEFAULT '',
	avatar         BYTEA,
	record_key     TEXT    NOT NULL,
	decryption_key BYTEA   NOT NULL,
	has_poster     BOOLEAN NOT NULL DEFAULT FALSE,
	updated_ts     BIGINT  NOT NULL,
	PRIMARY KEY (login_id, identifier)
);

-- Layer-2 MMCS attachment recovery queue (see pending_attachment_store.go).
CREATE TABLE IF NOT EXISTS pending_attachment_retry (
	login_id        TEXT    NOT NULL,
	message_guid    TEXT    NOT NULL,
	att_index       INTEGER NOT NULL,
	att_id          TEXT    NOT NULL,
	portal_id       TEXT    NOT NULL,
	sender          TEXT    NOT NULL DEFAULT '',
	timestamp_ms    BIGINT  NOT NULL DEFAULT 0,
	filename        TEXT    NOT NULL,
	mime_type       TEXT    NOT NULL,
	uti_type        TEXT    NOT NULL DEFAULT '',
	size_bytes      BIGINT  NOT NULL,
	mmcs_descriptor TEXT    NOT NULL,
	created_at      BIGINT  NOT NULL,
	last_attempt_at BIGINT  NOT NULL,
	attempt_count   INTEGER NOT NULL DEFAULT 0,
	next_attempt_at BIGINT  NOT NULL,
	PRIMARY KEY (login_id, message_guid, att_index)
);

-- Only indexes on columns every historical table version has; the indexes
-- on columns that older tables may lack are created in v3.
CREATE INDEX IF NOT EXISTS cloud_chat_portal_idx
	ON cloud_chat (login_id, portal_id, cloud_chat_id);
CREATE INDEX IF NOT EXISTS cloud_message_portal_ts_idx
	ON cloud_message (login_id, portal_id, timestamp_ms, guid);
CREATE INDEX IF NOT EXISTS cloud_message_chat_ts_idx
	ON cloud_message (login_id, chat_id, timestamp_ms, guid);
CREATE INDEX IF NOT EXISTS pending_attachment_retry_due_idx
	ON pending_attachment_retry (login_id, next_attempt_at);
