-- Expired unconsumed challenges use admin_challenges_active_idx. Keep legacy
-- consumed-row cleanup indexed too; new completions delete their challenge in
-- the credential/session transaction instead of retaining a permanent row.
CREATE INDEX admin_challenges_consumed_idx ON admin_challenges (consumed_at)
    WHERE consumed_at IS NOT NULL;
