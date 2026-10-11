-- Readiness belongs to the active speech lease; encoded audio remains in memory.
-- Gateway startup clears readiness before either text or speech senders run.
ALTER TABLE telegram_speech_jobs ADD COLUMN prepared INTEGER NOT NULL DEFAULT 0 CHECK(prepared IN (0,1));
