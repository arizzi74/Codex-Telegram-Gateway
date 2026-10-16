-- Question broadcast is an explicit preference of the session's connected bot.
-- Keep it separate from refreshed names, identity and credentials.
CREATE TABLE telegram_bot_question_preferences (
    bot_id TEXT NOT NULL PRIMARY KEY REFERENCES telegram_bots(bot_id) ON DELETE CASCADE,
    broadcast_questions INTEGER NOT NULL DEFAULT 0 CHECK(broadcast_questions IN (0,1))
) STRICT;
