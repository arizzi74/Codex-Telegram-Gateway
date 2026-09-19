-- Remember authenticated chat destinations even when their selection is cleared.
CREATE TABLE telegram_chat_modes (
    bot_id TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0,
    multi_session INTEGER NOT NULL DEFAULT 0 CHECK (multi_session IN (0,1)),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    PRIMARY KEY(bot_id,user_id,chat_id,message_thread_id)
) STRICT;
INSERT OR IGNORE INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id)
    SELECT bot_id,user_id,chat_id,message_thread_id FROM telegram_bindings;
INSERT OR IGNORE INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id)
    SELECT telegram_bot_id,telegram_user_id,telegram_chat_id,COALESCE(telegram_message_thread_id,0)
    FROM commands WHERE telegram_bot_id IS NOT NULL AND telegram_user_id IS NOT NULL AND telegram_chat_id IS NOT NULL;
CREATE TABLE telegram_session_aliases (
    session_id TEXT PRIMARY KEY REFERENCES sessions(session_id),
    alias TEXT NOT NULL UNIQUE CHECK (length(alias) BETWEEN 2 AND 32),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
) STRICT;
ALTER TABLE telegram_progress_messages ADD COLUMN retire_requested INTEGER NOT NULL DEFAULT 0 CHECK (retire_requested IN (0,1));
ALTER TABLE telegram_deliveries ADD COLUMN visibility_revoked INTEGER NOT NULL DEFAULT 0 CHECK (visibility_revoked IN (0,1));
