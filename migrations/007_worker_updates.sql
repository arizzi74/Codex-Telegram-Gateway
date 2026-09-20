-- Worker maintenance is independent of mutable runtimes and Telegram sessions.
-- Requests remain pending across disconnects and gateway/worker restarts.
CREATE TABLE worker_update_requests (
    request_id TEXT NOT NULL PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    worker_name TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','completed','up_to_date','failed')),
    version TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
    completed_at TEXT
) STRICT;

CREATE UNIQUE INDEX worker_update_requests_pending_worker_idx
    ON worker_update_requests(worker_id) WHERE state='pending';

-- Repeated commands join the existing request. A shared chat/topic receives a
-- single final notice, even when several authorized users request the update.
CREATE TABLE worker_update_watchers (
    request_id TEXT NOT NULL REFERENCES worker_update_requests(request_id),
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    user_id INTEGER NOT NULL CHECK (user_id <> 0),
    chat_id INTEGER NOT NULL CHECK (chat_id <> 0),
    message_thread_id INTEGER NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    PRIMARY KEY(request_id, bot_id, chat_id, message_thread_id)
) STRICT;
