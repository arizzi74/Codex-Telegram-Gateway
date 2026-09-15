-- SQLite baseline, including all state introduced by PostgreSQL migrations 001–007.
-- Timestamps are UTC RFC3339 text with exactly nine fractional digits.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER NOT NULL PRIMARY KEY,
    checksum BLOB NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')
) STRICT;

CREATE TABLE workers (
    worker_id TEXT NOT NULL PRIMARY KEY,
    name TEXT NOT NULL CHECK (name <> ''),
    hostname TEXT,
    os TEXT NOT NULL CHECK (os <> ''),
    arch TEXT NOT NULL CHECK (arch <> ''),
    worker_version TEXT,
    auth_token_hash BLOB NOT NULL UNIQUE CHECK (length(auth_token_hash) = 32),
    enabled INTEGER NOT NULL DEFAULT TRUE CHECK (enabled IN (0, 1)),
    connectivity TEXT NOT NULL DEFAULT 'offline'
        CHECK (connectivity IN ('online', 'unreachable', 'offline', 'disabled')),
    connection_id TEXT,
    connected_at TEXT,
    last_seen_at TEXT,
    heartbeat_metadata TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(heartbeat_metadata)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (connection_id IS NULL OR (length(connection_id) = 36 AND substr(connection_id, 9, 1) = '-' AND substr(connection_id, 14, 1) = '-' AND substr(connection_id, 19, 1) = '-' AND substr(connection_id, 24, 1) = '-' AND length(replace(connection_id, '-', '')) = 32 AND replace(connection_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX workers_connectivity_idx ON workers (connectivity, last_seen_at DESC);

CREATE TABLE runtimes (
    runtime_id TEXT NOT NULL PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    profile_id TEXT NOT NULL CHECK (profile_id <> ''),
    name TEXT NOT NULL CHECK (name <> ''),
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    pid INTEGER,
    state TEXT NOT NULL CHECK (state IN ('stopped', 'starting', 'running', 'degraded', 'failed')),
    codex_version TEXT,
    default_cwd TEXT,
    started_at TEXT,
    stopped_at TEXT,
    last_seen_at TEXT,
    metadata TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
    UNIQUE(worker_id, profile_id),
    UNIQUE(runtime_id, worker_id),
    CHECK (runtime_id IS NULL OR (length(runtime_id) = 36 AND substr(runtime_id, 9, 1) = '-' AND substr(runtime_id, 14, 1) = '-' AND substr(runtime_id, 19, 1) = '-' AND substr(runtime_id, 24, 1) = '-' AND length(replace(runtime_id, '-', '')) = 32 AND replace(runtime_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX runtimes_worker_state_idx ON runtimes (worker_id, state, last_seen_at DESC);

CREATE TABLE sessions (
    session_id TEXT NOT NULL PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    codex_thread_id TEXT NOT NULL CHECK (codex_thread_id <> ''),
    codex_session_id TEXT,
    name TEXT,
    preview TEXT,
    cwd TEXT,
    git_branch TEXT,
    git_root TEXT,
    state TEXT NOT NULL CHECK (state IN ('unknown', 'idle', 'running', 'waiting_approval', 'waiting_input', 'failed', 'not_loaded')),
    active_turn_id TEXT,
    loaded INTEGER NOT NULL DEFAULT FALSE CHECK (loaded IN (0, 1)),
    archived INTEGER NOT NULL DEFAULT FALSE CHECK (archived IN (0, 1)),
    discovered_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    last_activity_at TEXT,
    last_reconciled_at TEXT,
    metadata TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
    UNIQUE(runtime_id, codex_thread_id),
    UNIQUE(session_id, worker_id, runtime_id),
    CONSTRAINT sessions_runtime_owner_fk FOREIGN KEY (runtime_id, worker_id)
        REFERENCES runtimes(runtime_id, worker_id),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (runtime_id IS NULL OR (length(runtime_id) = 36 AND substr(runtime_id, 9, 1) = '-' AND substr(runtime_id, 14, 1) = '-' AND substr(runtime_id, 19, 1) = '-' AND substr(runtime_id, 24, 1) = '-' AND length(replace(runtime_id, '-', '')) = 32 AND replace(runtime_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX sessions_worker_activity_idx ON sessions (worker_id, archived, last_activity_at DESC);

CREATE TABLE telegram_bindings (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    selected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    PRIMARY KEY (bot_id, user_id, chat_id, message_thread_id),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX telegram_bindings_session_idx ON telegram_bindings (session_id);

CREATE TABLE telegram_updates (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    update_id INTEGER NOT NULL,
    user_id INTEGER,
    chat_id INTEGER,
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    raw TEXT NOT NULL CHECK (json_valid(raw)),
    PRIMARY KEY(bot_id, update_id)
) STRICT;

CREATE INDEX telegram_updates_received_idx ON telegram_updates (received_at DESC);

CREATE TABLE commands (
    command_id TEXT NOT NULL PRIMARY KEY,
    source TEXT NOT NULL CHECK (source <> ''),
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL CHECK (runtime_generation >= 0),
    session_id TEXT REFERENCES sessions(session_id),
    operation TEXT NOT NULL CHECK (operation <> ''),
    expected_turn_id TEXT,
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    status TEXT NOT NULL CHECK (status IN ('pending', 'dispatched', 'acknowledged', 'completed', 'failed', 'expired', 'outcome_unknown')),
    telegram_bot_id TEXT,
    telegram_update_id INTEGER,
    telegram_user_id INTEGER,
    telegram_chat_id INTEGER,
    telegram_message_thread_id INTEGER,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    dispatched_at TEXT,
    acknowledged_at TEXT,
    completed_at TEXT,
    expires_at TEXT,
    error_code TEXT,
    error_message TEXT,
    CONSTRAINT commands_runtime_owner_fk
    FOREIGN KEY (runtime_id, worker_id) REFERENCES runtimes(runtime_id, worker_id),
    CONSTRAINT commands_session_owner_fk
    FOREIGN KEY (session_id, worker_id, runtime_id) REFERENCES sessions(session_id, worker_id, runtime_id),
    CHECK (command_id IS NULL OR (length(command_id) = 36 AND substr(command_id, 9, 1) = '-' AND substr(command_id, 14, 1) = '-' AND substr(command_id, 19, 1) = '-' AND substr(command_id, 24, 1) = '-' AND length(replace(command_id, '-', '')) = 32 AND replace(command_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (runtime_id IS NULL OR (length(runtime_id) = 36 AND substr(runtime_id, 9, 1) = '-' AND substr(runtime_id, 14, 1) = '-' AND substr(runtime_id, 19, 1) = '-' AND substr(runtime_id, 24, 1) = '-' AND length(replace(runtime_id, '-', '')) = 32 AND replace(runtime_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX commands_pending_idx ON commands (worker_id, status, created_at) WHERE status IN ('pending', 'dispatched', 'acknowledged');
CREATE INDEX commands_session_idx ON commands (session_id, created_at DESC);


CREATE TRIGGER commands_preserve_routing_trigger
BEFORE UPDATE ON commands
WHEN NEW.command_id IS NOT OLD.command_id
  OR NEW.source IS NOT OLD.source
  OR NEW.worker_id IS NOT OLD.worker_id
  OR NEW.runtime_id IS NOT OLD.runtime_id
  OR NEW.runtime_generation IS NOT OLD.runtime_generation
  OR NEW.session_id IS NOT OLD.session_id
  OR NEW.operation IS NOT OLD.operation
  OR NEW.expected_turn_id IS NOT OLD.expected_turn_id
  OR NEW.payload IS NOT OLD.payload
  OR NEW.telegram_bot_id IS NOT OLD.telegram_bot_id
  OR NEW.telegram_update_id IS NOT OLD.telegram_update_id
  OR NEW.telegram_user_id IS NOT OLD.telegram_user_id
  OR NEW.telegram_chat_id IS NOT OLD.telegram_chat_id
  OR NEW.telegram_message_thread_id IS NOT OLD.telegram_message_thread_id
  OR NEW.created_at IS NOT OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'command routing is immutable');
END;

CREATE TRIGGER commands_preserve_records_trigger
BEFORE DELETE ON commands
BEGIN
    SELECT RAISE(ABORT, 'commands are immutable');
END;


CREATE TABLE events (
    event_id TEXT NOT NULL PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT,
    runtime_generation INTEGER CHECK (runtime_generation IS NULL OR runtime_generation >= 0),
    session_id TEXT,
    event_seq INTEGER NOT NULL CHECK (event_seq >= 0),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    occurred_at TEXT NOT NULL,
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    UNIQUE(worker_id, event_seq),
    CONSTRAINT events_runtime_owner_fk
    FOREIGN KEY (runtime_id, worker_id) REFERENCES runtimes(runtime_id, worker_id),
    CONSTRAINT events_session_owner_fk
    FOREIGN KEY (session_id, worker_id, runtime_id) REFERENCES sessions(session_id, worker_id, runtime_id),
    CHECK (event_id IS NULL OR (length(event_id) = 36 AND substr(event_id, 9, 1) = '-' AND substr(event_id, 14, 1) = '-' AND substr(event_id, 19, 1) = '-' AND substr(event_id, 24, 1) = '-' AND length(replace(event_id, '-', '')) = 32 AND replace(event_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (runtime_id IS NULL OR (length(runtime_id) = 36 AND substr(runtime_id, 9, 1) = '-' AND substr(runtime_id, 14, 1) = '-' AND substr(runtime_id, 19, 1) = '-' AND substr(runtime_id, 24, 1) = '-' AND length(replace(runtime_id, '-', '')) = 32 AND replace(runtime_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX events_worker_received_idx ON events (worker_id, received_at DESC);
CREATE INDEX events_runtime_seq_idx ON events (runtime_id, event_seq DESC);


CREATE TABLE worker_event_watermarks (
    worker_id TEXT NOT NULL PRIMARY KEY REFERENCES workers(worker_id),
    event_seq INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE TABLE approvals (
    response_command_id TEXT REFERENCES commands(command_id),
    input_answers TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(input_answers)),
    approval_id TEXT NOT NULL PRIMARY KEY,
    worker_id TEXT NOT NULL REFERENCES workers(worker_id),
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL CHECK (runtime_generation >= 0),
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    codex_request_id TEXT NOT NULL CHECK (codex_request_id <> ''),
    codex_thread_id TEXT NOT NULL CHECK (codex_thread_id <> ''),
    codex_turn_id TEXT,
    codex_item_id TEXT,
    approval_type TEXT NOT NULL CHECK (approval_type <> ''),
    request_payload TEXT NOT NULL CHECK (json_valid(request_payload)),
    state TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'declined', 'cancelled', 'expired', 'cleared')),
    requested_at TEXT NOT NULL,
    resolved_at TEXT,
    resolution TEXT CHECK (resolution IS NULL OR json_valid(resolution)),
    UNIQUE(runtime_id, runtime_generation, codex_request_id),
    CONSTRAINT approvals_runtime_owner_fk
    FOREIGN KEY (runtime_id, worker_id) REFERENCES runtimes(runtime_id, worker_id),
    CONSTRAINT approvals_session_owner_fk
    FOREIGN KEY (session_id, worker_id, runtime_id) REFERENCES sessions(session_id, worker_id, runtime_id),
    CHECK (approval_id IS NULL OR (length(approval_id) = 36 AND substr(approval_id, 9, 1) = '-' AND substr(approval_id, 14, 1) = '-' AND substr(approval_id, 19, 1) = '-' AND substr(approval_id, 24, 1) = '-' AND length(replace(approval_id, '-', '')) = 32 AND replace(approval_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (worker_id IS NULL OR (length(worker_id) = 36 AND substr(worker_id, 9, 1) = '-' AND substr(worker_id, 14, 1) = '-' AND substr(worker_id, 19, 1) = '-' AND substr(worker_id, 24, 1) = '-' AND length(replace(worker_id, '-', '')) = 32 AND replace(worker_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (runtime_id IS NULL OR (length(runtime_id) = 36 AND substr(runtime_id, 9, 1) = '-' AND substr(runtime_id, 14, 1) = '-' AND substr(runtime_id, 19, 1) = '-' AND substr(runtime_id, 24, 1) = '-' AND length(replace(runtime_id, '-', '')) = 32 AND replace(runtime_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (response_command_id IS NULL OR (length(response_command_id) = 36 AND substr(response_command_id, 9, 1) = '-' AND substr(response_command_id, 14, 1) = '-' AND substr(response_command_id, 19, 1) = '-' AND substr(response_command_id, 24, 1) = '-' AND length(replace(response_command_id, '-', '')) = 32 AND replace(response_command_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX approvals_pending_idx ON approvals (worker_id, state, requested_at) WHERE state = 'pending';


CREATE TABLE telegram_callbacks (
    token TEXT NOT NULL PRIMARY KEY,
    action TEXT NOT NULL CHECK (action <> ''),
    telegram_user_id INTEGER NOT NULL,
    session_id TEXT REFERENCES sessions(session_id),
    command_id TEXT REFERENCES commands(command_id),
    approval_id TEXT REFERENCES approvals(approval_id),
    payload TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),
    expires_at TEXT NOT NULL,
    used_at TEXT,
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (command_id IS NULL OR (length(command_id) = 36 AND substr(command_id, 9, 1) = '-' AND substr(command_id, 14, 1) = '-' AND substr(command_id, 19, 1) = '-' AND substr(command_id, 24, 1) = '-' AND length(replace(command_id, '-', '')) = 32 AND replace(command_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (approval_id IS NULL OR (length(approval_id) = 36 AND substr(approval_id, 9, 1) = '-' AND substr(approval_id, 14, 1) = '-' AND substr(approval_id, 19, 1) = '-' AND substr(approval_id, 24, 1) = '-' AND length(replace(approval_id, '-', '')) = 32 AND replace(approval_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX telegram_callbacks_expiry_idx ON telegram_callbacks (expires_at) WHERE used_at IS NULL;

CREATE TABLE bot_message_routes (
    question_id TEXT,
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    turn_id TEXT,
    approval_id TEXT REFERENCES approvals(approval_id),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    PRIMARY KEY(bot_id, chat_id, message_id),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (approval_id IS NULL OR (length(approval_id) = 36 AND substr(approval_id, 9, 1) = '-' AND substr(approval_id, 14, 1) = '-' AND substr(approval_id, 19, 1) = '-' AND substr(approval_id, 24, 1) = '-' AND length(replace(approval_id, '-', '')) = 32 AND replace(approval_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX bot_message_routes_session_idx ON bot_message_routes (session_id, created_at DESC);

-- Durable outbox rows are created with state changes that need Telegram
-- notification, then delivered by a separate retrying worker.
CREATE TABLE telegram_deliveries (
    delivery_id TEXT NOT NULL PRIMARY KEY,
    event_id TEXT REFERENCES events(event_id),
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    telegram_message_id INTEGER,
    last_error TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    sent_at TEXT,
    CHECK (delivery_id IS NULL OR (length(delivery_id) = 36 AND substr(delivery_id, 9, 1) = '-' AND substr(delivery_id, 14, 1) = '-' AND substr(delivery_id, 19, 1) = '-' AND substr(delivery_id, 24, 1) = '-' AND length(replace(delivery_id, '-', '')) = 32 AND replace(delivery_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (event_id IS NULL OR (length(event_id) = 36 AND substr(event_id, 9, 1) = '-' AND substr(event_id, 14, 1) = '-' AND substr(event_id, 19, 1) = '-' AND substr(event_id, 24, 1) = '-' AND length(replace(event_id, '-', '')) = 32 AND replace(event_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX telegram_deliveries_ready_idx ON telegram_deliveries (next_attempt_at, created_at) WHERE status IN ('pending', 'failed');
CREATE INDEX telegram_deliveries_event_idx ON telegram_deliveries (event_id) WHERE event_id IS NOT NULL;

-- Reserved durable state for the passkey administration flow. No passkey
-- ceremony logic is part of this migration.
CREATE TABLE admin_credentials (
    credential_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(credential_json)),
    credential_id BLOB NOT NULL PRIMARY KEY CHECK (length(credential_id) > 0),
    user_handle BLOB NOT NULL CHECK (length(user_handle) > 0),
    public_key BLOB NOT NULL CHECK (length(public_key) > 0),
    sign_count INTEGER NOT NULL DEFAULT 0 CHECK (sign_count >= 0),
    transports TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(transports)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    last_used_at TEXT,
    revoked_at TEXT
) STRICT;

CREATE INDEX admin_credentials_active_user_idx ON admin_credentials (user_handle) WHERE revoked_at IS NULL;

CREATE TABLE admin_challenges (
    session_data TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(session_data)),
    ceremony_binding_hash BLOB,
    challenge_id TEXT NOT NULL PRIMARY KEY,
    purpose TEXT NOT NULL CHECK (purpose IN ('registration', 'authentication', 'recovery')),
    challenge_hash BLOB NOT NULL CHECK (length(challenge_hash) = 32),
    user_handle BLOB,
    expires_at TEXT NOT NULL,
    consumed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    CHECK (challenge_id IS NULL OR (length(challenge_id) = 36 AND substr(challenge_id, 9, 1) = '-' AND substr(challenge_id, 14, 1) = '-' AND substr(challenge_id, 19, 1) = '-' AND substr(challenge_id, 24, 1) = '-' AND length(replace(challenge_id, '-', '')) = 32 AND replace(challenge_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX admin_challenges_active_idx ON admin_challenges (expires_at) WHERE consumed_at IS NULL;

CREATE TABLE admin_sessions (
    session_id TEXT NOT NULL PRIMARY KEY,
    credential_id BLOB REFERENCES admin_credentials(credential_id),
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    expires_at TEXT NOT NULL,
    revoked_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    last_seen_at TEXT,
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX admin_sessions_active_idx ON admin_sessions (expires_at) WHERE revoked_at IS NULL;

CREATE TABLE admin_bootstrap (
    bootstrap_id INTEGER NOT NULL PRIMARY KEY DEFAULT 1 CHECK (bootstrap_id = 1),
    token_hash BLOB NOT NULL CHECK (length(token_hash) = 32),
    expires_at TEXT NOT NULL,
    consumed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')
) STRICT;


CREATE UNIQUE INDEX approvals_response_command_id_idx
    ON approvals (response_command_id)
    WHERE response_command_id IS NOT NULL;

-- Complete the durable WebAuthn state reserved in 001.  The handle is a
-- single, opaque administrator identity; it is generated once and never
-- derived from an operator name or an email address.
CREATE TABLE admin_users (
    user_id INTEGER NOT NULL PRIMARY KEY DEFAULT 1 CHECK (user_id = 1),
    user_handle BLOB NOT NULL UNIQUE CHECK (length(user_handle) BETWEEN 16 AND 64),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')
) STRICT;



CREATE INDEX admin_challenges_binding_idx
    ON admin_challenges (ceremony_binding_hash, expires_at)
    WHERE consumed_at IS NULL;

-- Chunk checkpoints let a long Telegram response resume after a crash without
-- resending chunks whose Telegram message IDs were already committed.
CREATE TABLE telegram_delivery_chunks (
    delivery_id TEXT NOT NULL REFERENCES telegram_deliveries(delivery_id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL CHECK (chunk_index >= 0),
    payload TEXT NOT NULL CHECK (json_type(payload) = 'object') CHECK (json_valid(payload)),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent')),
    telegram_message_id INTEGER,
    sent_at TEXT,
    PRIMARY KEY (delivery_id, chunk_index),
    CHECK (delivery_id IS NULL OR (length(delivery_id) = 36 AND substr(delivery_id, 9, 1) = '-' AND substr(delivery_id, 14, 1) = '-' AND substr(delivery_id, 19, 1) = '-' AND substr(delivery_id, 24, 1) = '-' AND length(replace(delivery_id, '-', '')) = 32 AND replace(delivery_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX telegram_delivery_chunks_pending_idx
    ON telegram_delivery_chunks (delivery_id, chunk_index) WHERE status = 'pending';

-- Input questions can be answered one at a time. Answers remain durable until
-- every question is present, then one immutable InputResponse command is made.

-- A reply to a Telegram question is more specific than the user's current
-- selected session. The optional question ID selects the right item in a
-- multi-question input request.

CREATE INDEX bot_message_routes_approval_idx
    ON bot_message_routes (approval_id)
    WHERE approval_id IS NOT NULL;

-- Automatic selection changes made by asynchronous new/fork results are
-- conditional on the selection context still having the revision captured by
-- their command. Manual changes and successful automatic binds advance it.
CREATE TABLE telegram_selection_revisions (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    user_id INTEGER NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    PRIMARY KEY (bot_id, user_id, chat_id, message_thread_id)
) STRICT;

-- Temporary Telegram messages are retained until their own conversation has
-- received every chunk of the terminal response. Cleanup has an independent
-- retry lease, so a failed delete never resends the final response.
CREATE TABLE telegram_progress_messages (
    cleanup_id TEXT NOT NULL PRIMARY KEY,
    delivery_id TEXT NOT NULL REFERENCES telegram_deliveries(delivery_id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    bot_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL,
    message_thread_id INTEGER NOT NULL,
    telegram_message_id INTEGER NOT NULL,
    runtime_id TEXT NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation INTEGER NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    turn_id TEXT NOT NULL CHECK (turn_id <> ''),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','deleting','deleted')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z'),
    last_error TEXT,
    deleted_at TEXT,
    UNIQUE (delivery_id, chunk_index),
    CHECK (cleanup_id IS NULL OR (length(cleanup_id) = 36 AND substr(cleanup_id, 9, 1) = '-' AND substr(cleanup_id, 14, 1) = '-' AND substr(cleanup_id, 19, 1) = '-' AND substr(cleanup_id, 24, 1) = '-' AND length(replace(cleanup_id, '-', '')) = 32 AND replace(cleanup_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (delivery_id IS NULL OR (length(delivery_id) = 36 AND substr(delivery_id, 9, 1) = '-' AND substr(delivery_id, 14, 1) = '-' AND substr(delivery_id, 19, 1) = '-' AND substr(delivery_id, 24, 1) = '-' AND length(replace(delivery_id, '-', '')) = 32 AND replace(delivery_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (runtime_id IS NULL OR (length(runtime_id) = 36 AND substr(runtime_id, 9, 1) = '-' AND substr(runtime_id, 14, 1) = '-' AND substr(runtime_id, 19, 1) = '-' AND substr(runtime_id, 24, 1) = '-' AND length(replace(runtime_id, '-', '')) = 32 AND replace(runtime_id, '-', '') NOT GLOB '*[^0-9a-f]*')),
    CHECK (session_id IS NULL OR (length(session_id) = 36 AND substr(session_id, 9, 1) = '-' AND substr(session_id, 14, 1) = '-' AND substr(session_id, 19, 1) = '-' AND substr(session_id, 24, 1) = '-' AND length(replace(session_id, '-', '')) = 32 AND replace(session_id, '-', '') NOT GLOB '*[^0-9a-f]*'))
) STRICT;

CREATE INDEX telegram_progress_cleanup_idx
    ON telegram_progress_messages(next_attempt_at) WHERE status <> 'deleted';
CREATE INDEX telegram_progress_scope_idx
    ON telegram_progress_messages(runtime_id, runtime_generation, session_id, turn_id);
CREATE INDEX events_terminal_turn_idx
    ON events(runtime_id, runtime_generation, session_id, json_extract(payload, '$.turn_id'))
    WHERE kind IN ('turn_completed','turn_failed','turn_interrupted');


-- Event identities and payloads form the durable replay journal.
CREATE TRIGGER events_preserve_updates_trigger
BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are immutable');
END;

CREATE TRIGGER events_preserve_records_trigger
BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are immutable');
END;
