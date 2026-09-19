package registry

import (
	"context"
	"fmt"
)

// TelegramMenuContext is a previously authenticated conversation. Telegram's
// command-menu scopes are per chat/member, not per topic; the menu synchronizer
// combines topics while command routing retains their separate settings.
type TelegramMenuContext struct {
	UserID, ChatID, TopicID int64
	MultiSession            bool
}

func (s *Store) ListTelegramMenuContexts(ctx context.Context, botID string) ([]TelegramMenuContext, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id,chat_id,message_thread_id,multi_session FROM telegram_chat_modes WHERE bot_id=$1 ORDER BY chat_id,user_id,message_thread_id`, botID)
	if err != nil {
		return nil, fmt.Errorf("registry: list Telegram menu contexts: %w", err)
	}
	defer rows.Close()
	var contexts []TelegramMenuContext
	for rows.Next() {
		var entry TelegramMenuContext
		if err := rows.Scan(&entry.UserID, &entry.ChatID, &entry.TopicID, &entry.MultiSession); err != nil {
			return nil, err
		}
		contexts = append(contexts, entry)
	}
	return contexts, rows.Err()
}
