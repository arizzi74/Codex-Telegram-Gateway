-- Complete the durable WebAuthn state reserved in 001.  The handle is a
-- single, opaque administrator identity; it is generated once and never
-- derived from an operator name or an email address.
CREATE TABLE admin_users (
    user_id SMALLINT PRIMARY KEY DEFAULT 1 CHECK (user_id = 1),
    user_handle BYTEA NOT NULL UNIQUE CHECK (octet_length(user_handle) BETWEEN 16 AND 64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE admin_credentials
    ADD COLUMN credential_json JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE admin_challenges
    ADD COLUMN session_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN ceremony_binding_hash BYTEA;

CREATE INDEX admin_challenges_binding_idx
    ON admin_challenges (ceremony_binding_hash, expires_at)
    WHERE consumed_at IS NULL;
