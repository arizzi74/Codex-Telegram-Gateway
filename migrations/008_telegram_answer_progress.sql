-- An answer relocates progress independently of session-selection cleanup.
ALTER TABLE telegram_deliveries ADD COLUMN progress_repositioned INTEGER NOT NULL DEFAULT 0 CHECK (progress_repositioned IN (0,1));

CREATE INDEX telegram_progress_reposition_idx
    ON telegram_deliveries(bot_id,chat_id,message_thread_id)
    WHERE progress_repositioned=1;

CREATE INDEX telegram_answer_confirmation_idx
    ON telegram_deliveries(bot_id,chat_id,message_thread_id)
    WHERE kind='ui_response' AND json_extract(payload,'$.progress_reposition')=1
      AND status IN ('pending','failed','sending') AND visibility_revoked=0;
