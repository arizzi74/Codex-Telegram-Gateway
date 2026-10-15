-- Bot credentials are AES-GCM envelopes. The encryption key is stored in a
-- separate permission-restricted state file, never in SQLite or admin output.
CREATE TABLE telegram_bots (
    bot_id TEXT NOT NULL PRIMARY KEY,
    telegram_id INTEGER NOT NULL CHECK(telegram_id >= 0),
    owner_id INTEGER NOT NULL CHECK(owner_id > 0),
    username TEXT NOT NULL,
    display_name TEXT NOT NULL,
    legacy INTEGER NOT NULL DEFAULT 0 CHECK(legacy IN (0,1)),
    enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
    can_manage_bots INTEGER NOT NULL DEFAULT 0 CHECK(can_manage_bots IN (0,1)),
    managed_by TEXT NOT NULL DEFAULT '',
    encrypted_credentials BLOB NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX telegram_bots_identity ON telegram_bots(telegram_id) WHERE telegram_id > 0;
-- Keep revocation after removing credentials; late historical command replies
-- must not become deliverable if the same identity is connected again later.
CREATE TABLE telegram_bot_retirements (bot_id TEXT NOT NULL PRIMARY KEY) STRICT;
CREATE TABLE telegram_bot_retired_commands (
  bot_id TEXT NOT NULL,
  command_id TEXT NOT NULL REFERENCES commands(command_id) ON DELETE CASCADE,
  PRIMARY KEY(bot_id,command_id)
) STRICT;

CREATE TRIGGER telegram_delivery_bot_authorization AFTER INSERT ON telegram_deliveries
WHEN NEW.status IN ('pending','failed','sending') AND (
  EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=NEW.bot_id AND enabled=0)
  OR EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=NEW.bot_id)
  OR EXISTS(SELECT 1 FROM telegram_bot_retired_commands retired JOIN events event ON event.event_id=NEW.event_id
    WHERE retired.bot_id=NEW.bot_id AND retired.command_id=json_extract(event.payload,'$.command_id'))
  OR EXISTS(SELECT 1 FROM telegram_bot_retired_commands retired WHERE retired.bot_id=NEW.bot_id
    AND (retired.command_id=json_extract(NEW.payload,'$.command_id') OR retired.command_id=json_extract(NEW.payload,'$.data.command_id'))))
BEGIN
  UPDATE telegram_deliveries SET status='cancelled',visibility_revoked=1,last_error=NULL WHERE delivery_id=NEW.delivery_id;
END;
CREATE TRIGGER telegram_voice_bot_authorization AFTER INSERT ON telegram_voice_jobs
WHEN NEW.status IN ('pending','processing') AND (
  EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=NEW.bot_id AND enabled=0)
  OR EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=NEW.bot_id))
BEGIN
  UPDATE telegram_voice_jobs SET status='failed',completed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),error_code='voice_unauthorized',
    lease_id=NULL,lease_until=NULL,file_id=NULL,file_unique_id=NULL,mime_type=NULL,duration_seconds=NULL,file_size=NULL,reply_with_audio=0 WHERE job_id=NEW.job_id;
END;
CREATE TRIGGER telegram_speech_bot_authorization AFTER INSERT ON telegram_speech_jobs
WHEN NEW.status IN ('pending','processing') AND (
  EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=NEW.bot_id AND enabled=0)
  OR EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=NEW.bot_id))
BEGIN
  UPDATE telegram_speech_jobs SET status='cancelled',prepared=0,lease_id=NULL,lease_until=NULL,error_code='speech_unauthorized',
    completed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE job_id=NEW.job_id;
END;
CREATE TRIGGER telegram_progress_bot_authorization AFTER INSERT ON telegram_progress_messages
WHEN EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=NEW.bot_id AND enabled=0)
  OR EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=NEW.bot_id)
BEGIN
  UPDATE telegram_progress_messages SET retire_requested=1,next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE cleanup_id=NEW.cleanup_id;
END;
CREATE TABLE telegram_managed_bot_intents (
    intent_id TEXT NOT NULL PRIMARY KEY,
    manager_id TEXT NOT NULL,
    owner_id INTEGER NOT NULL,
    requested_username TEXT NOT NULL COLLATE NOCASE,
    display_name TEXT NOT NULL,
    launch_url TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('pending','activating','connected','expired','failed','cancelled')),
    bot_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX telegram_managed_bot_pending_username ON telegram_managed_bot_intents(manager_id,requested_username) WHERE status='pending';
CREATE TABLE telegram_managed_bot_updates (
    manager_id TEXT NOT NULL,
    update_id INTEGER NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY(manager_id,update_id)
) STRICT;
