-- Browser labels are generated from a bounded user-agent classification, never
-- raw headers. Renewal binds the predecessor at ceremony creation so completion
-- cannot choose a different browser login by changing its cookie.
ALTER TABLE admin_sessions ADD COLUMN browser_label TEXT NOT NULL DEFAULT 'Browser';
ALTER TABLE admin_challenges ADD COLUMN predecessor_token_hash BLOB
    CHECK (predecessor_token_hash IS NULL OR length(predecessor_token_hash)=32);
ALTER TABLE admin_challenges ADD COLUMN browser_label TEXT NOT NULL DEFAULT 'Browser';
