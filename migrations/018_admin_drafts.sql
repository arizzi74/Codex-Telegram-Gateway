-- Optional browser-encrypted text drafts. The gateway never receives their key.
-- Tombstones prevent a delayed save from recreating a consumed or cleared draft.
CREATE TABLE admin_drafts (
    recovery_id TEXT PRIMARY KEY NOT NULL,
    user_handle BLOB NOT NULL,
    admin_session_id TEXT NOT NULL REFERENCES admin_sessions(session_id),
    ciphertext TEXT,
    revision INTEGER NOT NULL CHECK (revision > 0 AND revision <= 9007199254740991),
    deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0,1)),
    expires_at TEXT NOT NULL,
    CHECK ((deleted=1 AND ciphertext IS NULL) OR (deleted=0 AND ciphertext IS NOT NULL))
) STRICT;
CREATE INDEX admin_drafts_owner_idx ON admin_drafts(user_handle);
CREATE INDEX admin_drafts_login_idx ON admin_drafts(admin_session_id);
CREATE INDEX admin_drafts_expiry_idx ON admin_drafts(expires_at);
