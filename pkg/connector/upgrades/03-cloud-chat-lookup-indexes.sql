-- v3: Index cloud_chat by record_name and group_id
-- Runs after v2 because cloud_chat tables created by older code only gain
-- these columns there.
CREATE INDEX IF NOT EXISTS cloud_chat_record_name_idx
	ON cloud_chat (login_id, record_name) WHERE record_name <> '';
CREATE INDEX IF NOT EXISTS cloud_chat_group_id_idx
	ON cloud_chat (login_id, group_id) WHERE group_id <> '';
