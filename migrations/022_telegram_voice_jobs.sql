-- Audio bytes and Telegram token URLs never enter the journal. A retry only
-- needs the opaque Telegram file ID and the immutable execution destination.
CREATE TABLE telegram_voice_jobs (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL UNIQUE,
    bot_id TEXT NOT NULL,
    update_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0,
    reply_to_message_id INTEGER NOT NULL DEFAULT 0,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    thread_id TEXT NOT NULL,
    expected_turn_id TEXT NOT NULL DEFAULT '',
    approval_id TEXT REFERENCES approvals(approval_id),
    question_id TEXT,
    question_secret INTEGER NOT NULL DEFAULT 0 CHECK(question_secret IN (0,1)),
    file_id TEXT,
    file_unique_id TEXT,
    mime_type TEXT,
    duration_seconds INTEGER,
    file_size INTEGER,
    command_ttl_ns INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','processing','completed','failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    lease_id TEXT,
    lease_until TEXT,
    next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    expires_at TEXT NOT NULL,
    completed_at TEXT,
    error_code TEXT,
    command_id TEXT REFERENCES commands(command_id),
    UNIQUE(bot_id,update_id),
    FOREIGN KEY(bot_id,update_id) REFERENCES telegram_updates(bot_id,update_id)
) STRICT;

CREATE INDEX telegram_voice_jobs_ready_idx ON telegram_voice_jobs(next_attempt_at,sequence)
    WHERE status IN ('pending','processing');
CREATE INDEX telegram_voice_jobs_conversation_idx ON telegram_voice_jobs(bot_id,chat_id,message_thread_id,update_id)
    WHERE status IN ('pending','processing');
CREATE INDEX telegram_voice_jobs_owner_idx ON telegram_voice_jobs(bot_id,user_id)
    WHERE status IN ('pending','processing');
CREATE INDEX telegram_voice_jobs_worker_idx ON telegram_voice_jobs(worker_id);
