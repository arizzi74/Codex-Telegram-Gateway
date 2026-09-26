-- Runtime maintenance is reported separately from the worker binary outcome.
-- NULL preserves compatibility with requests completed by older workers.
ALTER TABLE worker_update_requests ADD COLUMN codex_report TEXT
    CHECK (codex_report IS NULL OR (json_valid(codex_report) AND json_type(codex_report)='object'));
