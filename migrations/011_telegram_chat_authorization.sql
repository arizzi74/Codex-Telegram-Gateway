-- Current configured policy disables remembered destinations without deleting
-- sessions, approvals, reply routes, bindings or menu cleanup context.
-- An empty list retains the gateway's unrestricted-chat behavior.
CREATE TABLE telegram_chat_policies (
    bot_id TEXT PRIMARY KEY,
    allowed_chat_ids TEXT NOT NULL CHECK (json_valid(allowed_chat_ids) AND json_type(allowed_chat_ids)='array')
) STRICT;

-- Cover every queue producer, including late question edits and command replies
-- to historical routes. Cancellation is permanent if a chat is later reallowed.
-- Picker cleanup only deletes an existing Telegram message and remains eligible.
CREATE TRIGGER telegram_delivery_chat_authorization
AFTER INSERT ON telegram_deliveries
WHEN NEW.status IN ('pending','failed','sending') AND NEW.kind<>'picker_cleanup' AND EXISTS (
    SELECT 1 FROM telegram_chat_policies policy
    WHERE policy.bot_id=NEW.bot_id AND json_array_length(policy.allowed_chat_ids)>0
      AND NOT EXISTS (SELECT 1 FROM json_each(policy.allowed_chat_ids) chat WHERE chat.value=NEW.chat_id)
)
BEGIN
    UPDATE telegram_deliveries SET status='cancelled',visibility_revoked=1,last_error=NULL
    WHERE delivery_id=NEW.delivery_id;
END;
