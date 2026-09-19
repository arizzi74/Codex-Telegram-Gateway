-- Dashboard and Telegram status lookups must not scan historical payloads.
CREATE INDEX events_session_activity_idx ON events (session_id, occurred_at DESC);
CREATE INDEX approvals_session_pending_idx ON approvals (session_id)
    WHERE state = 'pending' AND response_command_id IS NULL;
CREATE INDEX commands_session_pending_idx ON commands (session_id)
    WHERE status IN ('pending', 'dispatched', 'acknowledged');
