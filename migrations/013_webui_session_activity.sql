-- The live sidebar reads only visible sessions and their current indicators.
-- Keep archived helper sessions and larger session metadata off this hot path.
CREATE INDEX sessions_visible_activity_idx ON sessions
    (session_id, worker_id, runtime_id, codex_thread_id, state, active_turn_id)
    WHERE archived = FALSE;
