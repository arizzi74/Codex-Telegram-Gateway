-- Current native settings are separate from historical usage statistics. The
-- activity feed joins this small row without reading session metadata/history.
CREATE TABLE session_settings (
    session_id TEXT NOT NULL PRIMARY KEY,
    worker_id TEXT NOT NULL,
    runtime_id TEXT NOT NULL,
    runtime_generation INTEGER NOT NULL CHECK (runtime_generation > 0),
    revision INTEGER NOT NULL CHECK (revision > 0),
    model TEXT NOT NULL CHECK (length(model) BETWEEN 1 AND 256),
    reasoning_effort TEXT NOT NULL CHECK (length(reasoning_effort) <= 256),
    FOREIGN KEY (session_id,worker_id,runtime_id) REFERENCES sessions(session_id,worker_id,runtime_id)
) STRICT;
