package registry

import (
	"context"
	"encoding/json"
	"errors"
)

// telegramChatAllowedSQL uses only fixed internal SQL identifiers. A missing
// policy or an empty list allows every chat, matching configuration semantics.
func telegramChatAllowedSQL(bot, chat string) string {
	return `NOT EXISTS(SELECT 1 FROM telegram_chat_policies chat_policy WHERE chat_policy.bot_id=` + bot + ` AND json_array_length(chat_policy.allowed_chat_ids)>0 AND NOT EXISTS(SELECT 1 FROM json_each(chat_policy.allowed_chat_ids) allowed_chat WHERE allowed_chat.value=` + chat + `))`
}

// ApplyTelegramChatAllowlist runs before starting gateway services. Remembered
// destinations are disabled by the persisted policy, while queued content is
// irrevocably retired so later reauthorization cannot replay it. Active leases
// retain their checkpoint opportunity for an API call already in flight.
func (s *Store) ApplyTelegramChatAllowlist(ctx context.Context, bot string, chats []int64) error {
	if bot == "" {
		return errors.New("registry: Telegram bot ID is required")
	}
	if chats == nil {
		chats = []int64{}
	}
	raw, err := json.Marshal(chats)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_chat_policies(bot_id,allowed_chat_ids) VALUES($1,$2)
        ON CONFLICT(bot_id) DO UPDATE SET allowed_chat_ids=excluded.allowed_chat_ids`, bot, string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries AS delivery
        SET status=CASE WHEN status='sending' THEN 'sending' ELSE 'cancelled' END,visibility_revoked=1,last_error=NULL
        WHERE bot_id=$1 AND kind NOT IN ('picker_cleanup','input_reply_cleanup') AND status IN ('pending','failed','sending')
          AND NOT `+telegramChatAllowedSQL("delivery.bot_id", "delivery.chat_id"), bot); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_progress_messages AS progress SET retire_requested=1,next_attempt_at=`+sqliteNow+`
        WHERE bot_id=$1 AND status<>'deleted' AND NOT `+telegramChatAllowedSQL("progress.bot_id", "progress.chat_id"), bot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TelegramChatAllowed checks current persisted authorization at an outbound
// request boundary, including when a prepared chunk differs from its queue row.
func (s *Store) TelegramChatAllowed(ctx context.Context, bot string, chat int64) (bool, error) {
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT `+telegramChatAllowedSQL("$1", "$2"), bot, chat).Scan(&allowed)
	return allowed, err
}
