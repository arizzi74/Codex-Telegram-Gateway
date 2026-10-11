ALTER TABLE telegram_voice_jobs ADD COLUMN reply_with_audio INTEGER NOT NULL DEFAULT 0 CHECK(reply_with_audio IN (0,1));
ALTER TABLE telegram_voice_jobs ADD COLUMN transcript_language TEXT NOT NULL DEFAULT 'en' CHECK(transcript_language IN ('en','it'));
CREATE INDEX telegram_voice_jobs_reply_turn_idx ON telegram_voice_jobs(session_id,expected_turn_id,bot_id,chat_id,message_thread_id)
    WHERE status='completed' AND reply_with_audio=1 AND question_secret=0;

-- Speech jobs reference the final text delivery. No audio bytes, download URLs,
-- credentials, or duplicate final-answer text are stored in this queue.
CREATE TABLE telegram_speech_jobs (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL UNIQUE,
    delivery_id TEXT NOT NULL UNIQUE REFERENCES telegram_deliveries(delivery_id) ON DELETE CASCADE,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    turn_id TEXT NOT NULL CHECK(turn_id<>''),
    bot_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0,
    user_id INTEGER NOT NULL,
    language TEXT NOT NULL DEFAULT 'en' CHECK(language IN ('en','it')),
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','processing','completed','failed','cancelled')),
    next_chunk INTEGER NOT NULL DEFAULT 0 CHECK(next_chunk>=0 AND next_chunk<=1000),
    attempts INTEGER NOT NULL DEFAULT 0,
    lease_id TEXT,
    lease_until TEXT,
    next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    expires_at TEXT NOT NULL,
    completed_at TEXT,
    error_code TEXT,
    UNIQUE(bot_id,chat_id,message_thread_id,session_id,turn_id)
) STRICT;
CREATE INDEX telegram_speech_jobs_ready_idx ON telegram_speech_jobs(next_attempt_at,sequence)
    WHERE status IN ('pending','processing');
CREATE INDEX telegram_speech_jobs_worker_idx ON telegram_speech_jobs(worker_id);

CREATE TABLE telegram_speech_chunks (
    job_id TEXT NOT NULL REFERENCES telegram_speech_jobs(job_id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL CHECK(chunk_index>=0 AND chunk_index<1000),
    telegram_message_id INTEGER NOT NULL CHECK(telegram_message_id>0),
    sent_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    PRIMARY KEY(job_id,chunk_index)
) STRICT;
