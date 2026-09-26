-- Track only the extra "Reply with text" messages, never original questions.
-- Old helpers are discovered once during migration. Subsequent reconciliation
-- drains indexed IDs in bounded batches; it never rescans conversation history.
CREATE TABLE telegram_input_reply_helpers (
    delivery_id TEXT NOT NULL PRIMARY KEY REFERENCES telegram_deliveries(delivery_id),
    approval_id TEXT NOT NULL,
    question_id TEXT NOT NULL,
    retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0,1)),
    backfill_pending INTEGER NOT NULL DEFAULT 0 CHECK(backfill_pending IN (0,1))
) STRICT;
CREATE INDEX telegram_input_reply_approval_idx ON telegram_input_reply_helpers(approval_id,question_id);
CREATE INDEX telegram_input_reply_pending_idx ON telegram_input_reply_helpers(delivery_id) WHERE backfill_pending=1;

CREATE TABLE telegram_input_reply_messages (
    bot_id TEXT NOT NULL,
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL CHECK(message_id>0),
    delivery_id TEXT NOT NULL REFERENCES telegram_input_reply_helpers(delivery_id),
    sent_at TEXT,
    PRIMARY KEY(bot_id,chat_id,message_id)
) STRICT;
CREATE INDEX telegram_input_reply_message_delivery_idx ON telegram_input_reply_messages(delivery_id);

INSERT INTO telegram_input_reply_helpers(delivery_id,approval_id,question_id,backfill_pending)
    SELECT delivery_id,json_extract(payload,'$.approval_id'),json_extract(payload,'$.question_id'),1
    FROM telegram_deliveries
    WHERE kind='ui_response' AND json_extract(payload,'$.view')='input_prompt'
      AND json_extract(payload,'$.text_reply')=1
      AND json_type(payload,'$.approval_id')='text' AND json_type(payload,'$.question_id')='text';
INSERT OR IGNORE INTO telegram_input_reply_messages(bot_id,chat_id,message_id,delivery_id,sent_at)
    SELECT delivery.bot_id,delivery.chat_id,chunk.telegram_message_id,helper.delivery_id,COALESCE(chunk.sent_at,delivery.sent_at)
    FROM telegram_input_reply_helpers helper JOIN telegram_deliveries delivery USING(delivery_id)
    JOIN telegram_delivery_chunks chunk USING(delivery_id)
    WHERE chunk.telegram_message_id>0;
INSERT OR IGNORE INTO telegram_input_reply_messages(bot_id,chat_id,message_id,delivery_id,sent_at)
    SELECT delivery.bot_id,delivery.chat_id,delivery.telegram_message_id,helper.delivery_id,delivery.sent_at
    FROM telegram_input_reply_helpers helper JOIN telegram_deliveries delivery USING(delivery_id)
    WHERE delivery.telegram_message_id>0;

-- Retire old queued answer edits for helpers. Their original question copies
-- retain their normal Question/Answer edits and reply routes.
UPDATE telegram_deliveries AS edit SET visibility_revoked=1,
    status=CASE WHEN status IN ('pending','failed') THEN 'cancelled' ELSE status END
    WHERE edit.kind='question_answered' AND edit.status IN ('pending','failed','sending')
      AND EXISTS(SELECT 1 FROM telegram_input_reply_messages helper
        WHERE helper.bot_id=edit.bot_id AND helper.chat_id=edit.chat_id
          AND helper.message_id=json_extract(edit.payload,'$.message_id'));

-- Change guards keep unchanged heartbeat/discovery upserts out of the queue.
CREATE TRIGGER telegram_input_reply_approval_changed
AFTER UPDATE OF state,response_command_id,input_answers,request_payload ON approvals
WHEN OLD.state IS NOT NEW.state OR OLD.response_command_id IS NOT NEW.response_command_id
  OR OLD.input_answers IS NOT NEW.input_answers OR OLD.request_payload IS NOT NEW.request_payload
BEGIN
    UPDATE telegram_input_reply_helpers SET backfill_pending=1,
      retired=CASE WHEN NEW.state<>'pending' OR NEW.response_command_id IS NOT NULL
        OR NOT EXISTS(SELECT 1 FROM json_each(NEW.request_payload,'$.questions') question
          WHERE json_extract(question.value,'$.id')=telegram_input_reply_helpers.question_id)
        OR EXISTS(SELECT 1 FROM json_each(NEW.input_answers) answer
          WHERE answer.key=telegram_input_reply_helpers.question_id AND json_array_length(answer.value)>0)
        THEN 1 ELSE retired END
      WHERE approval_id=NEW.approval_id AND retired=0;
END;
CREATE TRIGGER telegram_input_reply_approval_deleted
AFTER DELETE ON approvals
BEGIN
    UPDATE telegram_input_reply_helpers SET backfill_pending=1,retired=1
      WHERE approval_id=OLD.approval_id AND retired=0;
END;
CREATE TRIGGER telegram_input_reply_session_changed
AFTER UPDATE OF archived,active_turn_id,codex_thread_id ON sessions
WHEN OLD.archived IS NOT NEW.archived OR OLD.active_turn_id IS NOT NEW.active_turn_id OR OLD.codex_thread_id IS NOT NEW.codex_thread_id
BEGIN
    UPDATE telegram_input_reply_helpers SET backfill_pending=1,
      retired=CASE WHEN NEW.archived=1 OR EXISTS(SELECT 1 FROM approvals approval
        WHERE approval.approval_id=telegram_input_reply_helpers.approval_id
          AND (approval.codex_thread_id<>NEW.codex_thread_id OR
            (COALESCE(json_extract(approval.request_payload,'$.async'),0)<>1
              AND COALESCE(approval.codex_turn_id,'')<>''
              AND COALESCE(approval.codex_turn_id,'')<>COALESCE(NEW.active_turn_id,''))))
        THEN 1 ELSE retired END
      WHERE approval_id IN (SELECT approval_id FROM approvals WHERE session_id=NEW.session_id AND state='pending') AND retired=0;
END;
CREATE TRIGGER telegram_input_reply_runtime_changed
AFTER UPDATE OF generation ON runtimes
WHEN OLD.generation IS NOT NEW.generation
BEGIN
    UPDATE telegram_input_reply_helpers SET backfill_pending=1,
      retired=CASE WHEN EXISTS(SELECT 1 FROM approvals approval
        WHERE approval.approval_id=telegram_input_reply_helpers.approval_id
          AND approval.runtime_generation<>NEW.generation) THEN 1 ELSE retired END
      WHERE approval_id IN (SELECT approval_id FROM approvals WHERE runtime_id=NEW.runtime_id) AND retired=0;
END;

-- Deletion-only work is still safe after permission to post into a chat ends.
DROP TRIGGER telegram_delivery_chat_authorization;
CREATE TRIGGER telegram_delivery_chat_authorization
AFTER INSERT ON telegram_deliveries
WHEN NEW.status IN ('pending','failed','sending')
  AND NEW.kind NOT IN ('picker_cleanup','input_reply_cleanup') AND EXISTS (
    SELECT 1 FROM telegram_chat_policies policy
    WHERE policy.bot_id=NEW.bot_id AND json_array_length(policy.allowed_chat_ids)>0
      AND NOT EXISTS (SELECT 1 FROM json_each(policy.allowed_chat_ids) chat WHERE chat.value=NEW.chat_id)
)
BEGIN
    UPDATE telegram_deliveries SET status='cancelled',visibility_revoked=1,last_error=NULL
    WHERE delivery_id=NEW.delivery_id;
END;
