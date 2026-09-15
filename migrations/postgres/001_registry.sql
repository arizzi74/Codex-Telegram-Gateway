CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY,
    checksum BYTEA NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE workers (
    worker_id UUID PRIMARY KEY,
    name TEXT NOT NULL CHECK (name <> ''),
    hostname TEXT,
    os TEXT NOT NULL CHECK (os <> ''),
    arch TEXT NOT NULL CHECK (arch <> ''),
    worker_version TEXT,
    auth_token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(auth_token_hash) = 32),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    connectivity TEXT NOT NULL DEFAULT 'offline'
        CHECK (connectivity IN ('online', 'unreachable', 'offline', 'disabled')),
    connection_id UUID,
    connected_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ,
    heartbeat_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX workers_connectivity_idx ON workers (connectivity, last_seen_at DESC);

CREATE TABLE runtimes (
    runtime_id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES workers(worker_id),
    profile_id TEXT NOT NULL CHECK (profile_id <> ''),
    name TEXT NOT NULL CHECK (name <> ''),
    generation BIGINT NOT NULL DEFAULT 0 CHECK (generation >= 0),
    pid BIGINT,
    state TEXT NOT NULL CHECK (state IN ('stopped', 'starting', 'running', 'degraded', 'failed')),
    codex_version TEXT,
    default_cwd TEXT,
    started_at TIMESTAMPTZ,
    stopped_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE(worker_id, profile_id),
    UNIQUE(runtime_id, worker_id)
);

CREATE INDEX runtimes_worker_state_idx ON runtimes (worker_id, state, last_seen_at DESC);

CREATE TABLE sessions (
    session_id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES workers(worker_id),
    runtime_id UUID NOT NULL REFERENCES runtimes(runtime_id),
    codex_thread_id TEXT NOT NULL CHECK (codex_thread_id <> ''),
    codex_session_id TEXT,
    name TEXT,
    preview TEXT,
    cwd TEXT,
    git_branch TEXT,
    git_root TEXT,
    state TEXT NOT NULL CHECK (state IN ('unknown', 'idle', 'running', 'waiting_approval', 'waiting_input', 'failed', 'not_loaded')),
    active_turn_id TEXT,
    loaded BOOLEAN NOT NULL DEFAULT FALSE,
    archived BOOLEAN NOT NULL DEFAULT FALSE,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_activity_at TIMESTAMPTZ,
    last_reconciled_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE(runtime_id, codex_thread_id),
    UNIQUE(session_id, worker_id, runtime_id),
    CONSTRAINT sessions_runtime_owner_fk FOREIGN KEY (runtime_id, worker_id)
        REFERENCES runtimes(runtime_id, worker_id)
);

CREATE INDEX sessions_worker_activity_idx ON sessions (worker_id, archived, last_activity_at DESC);

CREATE TABLE telegram_bindings (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    user_id BIGINT NOT NULL,
    chat_id BIGINT NOT NULL,
    message_thread_id BIGINT NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    session_id UUID NOT NULL REFERENCES sessions(session_id),
    selected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (bot_id, user_id, chat_id, message_thread_id)
);

CREATE INDEX telegram_bindings_session_idx ON telegram_bindings (session_id);

CREATE TABLE telegram_updates (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    update_id BIGINT NOT NULL,
    user_id BIGINT,
    chat_id BIGINT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    raw JSONB NOT NULL,
    PRIMARY KEY(bot_id, update_id)
);

CREATE INDEX telegram_updates_received_idx ON telegram_updates (received_at DESC);

CREATE TABLE commands (
    command_id UUID PRIMARY KEY,
    source TEXT NOT NULL CHECK (source <> ''),
    worker_id UUID NOT NULL REFERENCES workers(worker_id),
    runtime_id UUID NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation BIGINT NOT NULL CHECK (runtime_generation >= 0),
    session_id UUID REFERENCES sessions(session_id),
    operation TEXT NOT NULL CHECK (operation <> ''),
    expected_turn_id TEXT,
    payload JSONB NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'dispatched', 'acknowledged', 'completed', 'failed', 'expired', 'outcome_unknown')),
    telegram_bot_id TEXT,
    telegram_update_id BIGINT,
    telegram_user_id BIGINT,
    telegram_chat_id BIGINT,
    telegram_message_thread_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at TIMESTAMPTZ,
    acknowledged_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    error_code TEXT,
    error_message TEXT
);

CREATE INDEX commands_pending_idx ON commands (worker_id, status, created_at) WHERE status IN ('pending', 'dispatched', 'acknowledged');
CREATE INDEX commands_session_idx ON commands (session_id, created_at DESC);

ALTER TABLE commands ADD CONSTRAINT commands_runtime_owner_fk
    FOREIGN KEY (runtime_id, worker_id) REFERENCES runtimes(runtime_id, worker_id);
ALTER TABLE commands ADD CONSTRAINT commands_session_owner_fk
    FOREIGN KEY (session_id, worker_id, runtime_id) REFERENCES sessions(session_id, worker_id, runtime_id);

CREATE OR REPLACE FUNCTION commands_preserve_routing()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'commands are immutable';
    END IF;
    IF NEW.source IS DISTINCT FROM OLD.source
       OR NEW.worker_id IS DISTINCT FROM OLD.worker_id
       OR NEW.runtime_id IS DISTINCT FROM OLD.runtime_id
       OR NEW.runtime_generation IS DISTINCT FROM OLD.runtime_generation
       OR NEW.session_id IS DISTINCT FROM OLD.session_id
       OR NEW.operation IS DISTINCT FROM OLD.operation
       OR NEW.expected_turn_id IS DISTINCT FROM OLD.expected_turn_id
       OR NEW.payload IS DISTINCT FROM OLD.payload
       OR NEW.telegram_bot_id IS DISTINCT FROM OLD.telegram_bot_id
       OR NEW.telegram_update_id IS DISTINCT FROM OLD.telegram_update_id
       OR NEW.telegram_user_id IS DISTINCT FROM OLD.telegram_user_id
       OR NEW.telegram_chat_id IS DISTINCT FROM OLD.telegram_chat_id
       OR NEW.telegram_message_thread_id IS DISTINCT FROM OLD.telegram_message_thread_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'command routing is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER commands_preserve_routing_trigger
BEFORE UPDATE ON commands
FOR EACH ROW EXECUTE FUNCTION commands_preserve_routing();

CREATE TABLE events (
    event_id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES workers(worker_id),
    runtime_id UUID,
    runtime_generation BIGINT CHECK (runtime_generation IS NULL OR runtime_generation >= 0),
    session_id UUID,
    event_seq BIGINT NOT NULL CHECK (event_seq >= 0),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(worker_id, event_seq)
);

CREATE INDEX events_worker_received_idx ON events (worker_id, received_at DESC);
CREATE INDEX events_runtime_seq_idx ON events (runtime_id, event_seq DESC);

ALTER TABLE events ADD CONSTRAINT events_runtime_owner_fk
    FOREIGN KEY (runtime_id, worker_id) REFERENCES runtimes(runtime_id, worker_id);
ALTER TABLE events ADD CONSTRAINT events_session_owner_fk
    FOREIGN KEY (session_id, worker_id, runtime_id) REFERENCES sessions(session_id, worker_id, runtime_id);

CREATE TABLE worker_event_watermarks (
    worker_id UUID PRIMARY KEY REFERENCES workers(worker_id),
    event_seq BIGINT NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE approvals (
    approval_id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES workers(worker_id),
    runtime_id UUID NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation BIGINT NOT NULL CHECK (runtime_generation >= 0),
    session_id UUID NOT NULL REFERENCES sessions(session_id),
    codex_request_id TEXT NOT NULL CHECK (codex_request_id <> ''),
    codex_thread_id TEXT NOT NULL CHECK (codex_thread_id <> ''),
    codex_turn_id TEXT,
    codex_item_id TEXT,
    approval_type TEXT NOT NULL CHECK (approval_type <> ''),
    request_payload JSONB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'declined', 'cancelled', 'expired', 'cleared')),
    requested_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    resolution JSONB,
    UNIQUE(runtime_id, runtime_generation, codex_request_id)
);

CREATE INDEX approvals_pending_idx ON approvals (worker_id, state, requested_at) WHERE state = 'pending';

ALTER TABLE approvals ADD CONSTRAINT approvals_runtime_owner_fk
    FOREIGN KEY (runtime_id, worker_id) REFERENCES runtimes(runtime_id, worker_id);
ALTER TABLE approvals ADD CONSTRAINT approvals_session_owner_fk
    FOREIGN KEY (session_id, worker_id, runtime_id) REFERENCES sessions(session_id, worker_id, runtime_id);

CREATE TABLE telegram_callbacks (
    token TEXT PRIMARY KEY,
    action TEXT NOT NULL CHECK (action <> ''),
    telegram_user_id BIGINT NOT NULL,
    session_id UUID REFERENCES sessions(session_id),
    command_id UUID REFERENCES commands(command_id),
    approval_id UUID REFERENCES approvals(approval_id),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ
);

CREATE INDEX telegram_callbacks_expiry_idx ON telegram_callbacks (expires_at) WHERE used_at IS NULL;

CREATE TABLE bot_message_routes (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    chat_id BIGINT NOT NULL,
    message_id BIGINT NOT NULL,
    session_id UUID NOT NULL REFERENCES sessions(session_id),
    turn_id TEXT,
    approval_id UUID REFERENCES approvals(approval_id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(bot_id, chat_id, message_id)
);

CREATE INDEX bot_message_routes_session_idx ON bot_message_routes (session_id, created_at DESC);

-- Durable outbox rows are created with state changes that need Telegram
-- notification, then delivered by a separate retrying worker.
CREATE TABLE telegram_deliveries (
    delivery_id UUID PRIMARY KEY,
    event_id UUID REFERENCES events(event_id),
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    chat_id BIGINT NOT NULL,
    message_thread_id BIGINT NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    kind TEXT NOT NULL CHECK (kind <> ''),
    payload JSONB NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    telegram_message_id BIGINT,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);

CREATE INDEX telegram_deliveries_ready_idx ON telegram_deliveries (next_attempt_at, created_at) WHERE status IN ('pending', 'failed');
CREATE INDEX telegram_deliveries_event_idx ON telegram_deliveries (event_id) WHERE event_id IS NOT NULL;

-- Reserved durable state for the passkey administration flow. No passkey
-- ceremony logic is part of this migration.
CREATE TABLE admin_credentials (
    credential_id BYTEA PRIMARY KEY CHECK (octet_length(credential_id) > 0),
    user_handle BYTEA NOT NULL CHECK (octet_length(user_handle) > 0),
    public_key BYTEA NOT NULL CHECK (octet_length(public_key) > 0),
    sign_count BIGINT NOT NULL DEFAULT 0 CHECK (sign_count >= 0),
    transports JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX admin_credentials_active_user_idx ON admin_credentials (user_handle) WHERE revoked_at IS NULL;

CREATE TABLE admin_challenges (
    challenge_id UUID PRIMARY KEY,
    purpose TEXT NOT NULL CHECK (purpose IN ('registration', 'authentication', 'recovery')),
    challenge_hash BYTEA NOT NULL CHECK (octet_length(challenge_hash) = 32),
    user_handle BYTEA,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX admin_challenges_active_idx ON admin_challenges (expires_at) WHERE consumed_at IS NULL;

CREATE TABLE admin_sessions (
    session_id UUID PRIMARY KEY,
    credential_id BYTEA REFERENCES admin_credentials(credential_id),
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ
);

CREATE INDEX admin_sessions_active_idx ON admin_sessions (expires_at) WHERE revoked_at IS NULL;

CREATE TABLE admin_bootstrap (
    bootstrap_id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (bootstrap_id = 1),
    token_hash BYTEA NOT NULL CHECK (octet_length(token_hash) = 32),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
