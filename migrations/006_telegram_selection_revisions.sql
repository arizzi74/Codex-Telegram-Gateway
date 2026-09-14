-- Automatic selection changes made by asynchronous new/fork results are
-- conditional on the selection context still having the revision captured by
-- their command. Manual changes and successful automatic binds advance it.
CREATE TABLE telegram_selection_revisions (
    bot_id TEXT NOT NULL CHECK (bot_id <> ''),
    user_id BIGINT NOT NULL,
    chat_id BIGINT NOT NULL,
    message_thread_id BIGINT NOT NULL DEFAULT 0 CHECK (message_thread_id >= 0),
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    PRIMARY KEY (bot_id, user_id, chat_id, message_thread_id)
);
