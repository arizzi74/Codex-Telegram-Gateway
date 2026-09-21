-- Poll only outstanding work; completed history must not increase idle CPU.
-- Keep predicates identical to the queries so SQLite can use partial indexes.
CREATE INDEX telegram_deliveries_queue_idx
    ON telegram_deliveries(status,next_attempt_at,created_at)
    WHERE status IN ('pending','failed','sending');

CREATE INDEX telegram_progress_ready_idx
    ON telegram_progress_messages(next_attempt_at,cleanup_id)
    WHERE status IN ('pending','deleting');

CREATE INDEX events_runtime_lifecycle_idx
    ON events(runtime_id,kind,runtime_generation)
    WHERE kind IN ('runtime_failed','runtime_stopped','runtime_started');

CREATE INDEX commands_expiry_idx
    ON commands(expires_at,created_at)
    WHERE status IN ('pending','dispatched') AND expires_at IS NOT NULL;
