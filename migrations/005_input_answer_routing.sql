-- Input questions can be answered one at a time. Answers remain durable until
-- every question is present, then one immutable InputResponse command is made.
ALTER TABLE approvals
    ADD COLUMN input_answers JSONB NOT NULL DEFAULT '{}'::jsonb;

-- A reply to a Telegram question is more specific than the user's current
-- selected session. The optional question ID selects the right item in a
-- multi-question input request.
ALTER TABLE bot_message_routes
    ADD COLUMN question_id TEXT;

CREATE INDEX bot_message_routes_approval_idx
    ON bot_message_routes (approval_id)
    WHERE approval_id IS NOT NULL;
