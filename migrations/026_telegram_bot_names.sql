-- Name preferences are independent of the refreshed Telegram profile cache.
-- NULL distinguishes an uncaptured name from Telegram's valid empty default.
CREATE TABLE telegram_bot_names (
    bot_id TEXT NOT NULL PRIMARY KEY REFERENCES telegram_bots(bot_id) ON DELETE CASCADE,
    inherit_session_name INTEGER NOT NULL DEFAULT 0 CHECK(inherit_session_name IN (0,1)),
    original_display_name TEXT CHECK(original_display_name IS NULL OR length(original_display_name)<=64),
    applied_display_name TEXT CHECK(applied_display_name IS NULL OR length(applied_display_name)<=64)
) STRICT;
