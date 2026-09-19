-- A single durable interactive session workflow per authenticated Telegram context.
CREATE TABLE telegram_session_wizards (
    wizard_id TEXT PRIMARY KEY,
    bot_id TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0,
    revision INTEGER NOT NULL CHECK (revision > 0),
    kind TEXT NOT NULL CHECK (kind IN ('new','delete')),
    phase TEXT NOT NULL,
    runtime_id TEXT,
    runtime_generation INTEGER NOT NULL DEFAULT 0,
    session_id TEXT,
    session_name TEXT NOT NULL DEFAULT '',
    cwd TEXT NOT NULL DEFAULT '',
    workspace TEXT,
    command_id TEXT,
    expires_at TEXT NOT NULL,
    UNIQUE(bot_id, user_id, chat_id, message_thread_id)
);
CREATE INDEX telegram_session_wizards_command_idx ON telegram_session_wizards(command_id);
