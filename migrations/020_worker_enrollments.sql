-- Enrollment codes are short-lived capabilities. Only their hashes are durable;
-- the worker and its long-lived token hash are created when the code is redeemed.
CREATE TABLE worker_enrollments (
    enrollment_id TEXT NOT NULL PRIMARY KEY,
    code_hash BLOB NOT NULL UNIQUE CHECK (length(code_hash) = 32),
    service_access TEXT NOT NULL CHECK (service_access IN ('restricted', 'full')),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL CHECK (expires_at > created_at),
    consumed_at TEXT,
    revoked_at TEXT,
    worker_id TEXT REFERENCES workers(worker_id),
    CHECK (length(enrollment_id) = 36 AND substr(enrollment_id, 9, 1) = '-' AND substr(enrollment_id, 14, 1) = '-' AND substr(enrollment_id, 19, 1) = '-' AND substr(enrollment_id, 24, 1) = '-' AND length(replace(enrollment_id, '-', '')) = 32 AND replace(enrollment_id, '-', '') NOT GLOB '*[^0-9a-f]*'),
    CHECK (worker_id IS NULL OR consumed_at IS NOT NULL)
) STRICT;

CREATE INDEX worker_enrollments_unused_expiry_idx ON worker_enrollments(expires_at)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;
