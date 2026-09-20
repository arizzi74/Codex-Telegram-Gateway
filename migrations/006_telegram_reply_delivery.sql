-- Unknown replies only inspect attempted deliveries whose routes may still
-- be awaiting a checkpoint, independently of the size of sent history.
CREATE INDEX telegram_reply_checkpoint_idx ON telegram_deliveries (bot_id,chat_id)
    WHERE status IN ('pending','failed','sending') AND attempt_count>0;
