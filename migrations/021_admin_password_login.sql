-- Password authentication shares the existing administrator identity and
-- session ownership; WebAuthn reads explicitly select only passkey records.
ALTER TABLE admin_credentials ADD COLUMN kind TEXT NOT NULL DEFAULT 'passkey'
    CHECK (kind IN ('passkey', 'password'));

CREATE TABLE admin_password_login (
    login_id INTEGER NOT NULL PRIMARY KEY DEFAULT 1 CHECK (login_id = 1),
    username TEXT NOT NULL CHECK (length(CAST(username AS BLOB)) BETWEEN 1 AND 64),
    password_hash TEXT NOT NULL CHECK (length(password_hash) BETWEEN 1 AND 128),
    credential_id BLOB NOT NULL UNIQUE REFERENCES admin_credentials(credential_id),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')
) STRICT;
