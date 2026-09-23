-- Private Web Push capability state. Endpoints and encryption keys must never
-- be returned by inventory APIs, logged, or included in notification payloads.
CREATE TABLE webpush_keys (
    singleton INTEGER PRIMARY KEY CHECK (singleton=1),
    private_key TEXT NOT NULL,
    public_key TEXT NOT NULL
) STRICT;

CREATE TABLE webpush_subscriptions (
    subscription_id TEXT PRIMARY KEY NOT NULL,
    credential_id BLOB NOT NULL REFERENCES admin_credentials(credential_id),
    admin_session_id TEXT NOT NULL REFERENCES admin_sessions(session_id),
    endpoint TEXT NOT NULL UNIQUE,
    p256dh TEXT NOT NULL,
    auth TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX webpush_subscriptions_credential_idx ON webpush_subscriptions(credential_id);
CREATE INDEX webpush_subscriptions_login_idx ON webpush_subscriptions(admin_session_id);

CREATE TABLE webpush_deliveries (
    delivery_id TEXT PRIMARY KEY NOT NULL,
    subscription_id TEXT NOT NULL REFERENCES webpush_subscriptions(subscription_id) ON DELETE CASCADE,
    event_id TEXT NOT NULL REFERENCES events(event_id),
    session_id TEXT NOT NULL REFERENCES sessions(session_id),
    runtime_generation INTEGER NOT NULL,
    turn_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','sending','delivered','discarded')),
    attempt INTEGER NOT NULL DEFAULT 0,
    lease_id TEXT,
    next_attempt_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(subscription_id,session_id,runtime_generation,turn_id)
) STRICT;
CREATE INDEX webpush_deliveries_ready_idx ON webpush_deliveries(next_attempt_at)
    WHERE status IN ('pending','sending');
CREATE INDEX webpush_deliveries_expiry_idx ON webpush_deliveries(expires_at);
