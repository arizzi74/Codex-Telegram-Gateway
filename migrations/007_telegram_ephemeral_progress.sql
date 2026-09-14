-- Temporary Telegram messages are retained until their own conversation has
-- received every chunk of the terminal response. Cleanup has an independent
-- retry lease, so a failed delete never resends the final response.
CREATE TABLE telegram_progress_messages (
    cleanup_id UUID PRIMARY KEY,
    delivery_id UUID NOT NULL REFERENCES telegram_deliveries(delivery_id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    bot_id TEXT NOT NULL,
    chat_id BIGINT NOT NULL,
    message_thread_id BIGINT NOT NULL,
    telegram_message_id BIGINT NOT NULL,
    runtime_id UUID NOT NULL REFERENCES runtimes(runtime_id),
    runtime_generation BIGINT NOT NULL,
    session_id UUID NOT NULL REFERENCES sessions(session_id),
    turn_id TEXT NOT NULL CHECK (turn_id <> ''),
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','deleting','deleted')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT,
    deleted_at TIMESTAMPTZ,
    UNIQUE (delivery_id, chunk_index)
);

CREATE INDEX telegram_progress_cleanup_idx
    ON telegram_progress_messages(next_attempt_at) WHERE status <> 'deleted';
CREATE INDEX telegram_progress_scope_idx
    ON telegram_progress_messages(runtime_id, runtime_generation, session_id, turn_id);
CREATE INDEX events_terminal_turn_idx
    ON events(runtime_id, runtime_generation, session_id, (payload->>'turn_id'))
    WHERE kind IN ('turn_completed','turn_failed','turn_interrupted');

-- Existing unsent acknowledgements should not leak into a newly quiet prompt
-- flow after upgrading. Accepted controls retain their normal feedback.
UPDATE telegram_deliveries AS delivery SET status='cancelled'
FROM commands AS command
WHERE delivery.kind='ui_response' AND delivery.status IN ('pending','failed','sending')
  AND delivery.payload->>'view'='queued'
  AND delivery.payload->>'command_id'=command.command_id::text
  AND command.operation='start_turn';
