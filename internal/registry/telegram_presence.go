package registry

import (
	"context"
	"errors"
	"fmt"
)

// TelegramTypingTarget identifies a conversation where Telegram should show
// that the gateway is waiting for Codex. It is a transient projection of
// durable command/session state; typing notifications themselves are never
// persisted.
type TelegramTypingTarget struct {
	BotID   string
	ChatID  int64
	TopicID int64
}

// ListTelegramTypingTargets returns distinct Telegram conversations with an
// accepted command still in flight or a visible turn still running, including CLI turns.
// Requiring an online worker and, for session commands, the original binding
// makes disconnects stop presence without mutating command history.
func (s *Store) ListTelegramTypingTargets(ctx context.Context, limit int) ([]TelegramTypingTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("registry: invalid Telegram typing target limit")
	}
	rows, err := s.pool.Query(ctx, `WITH destinations AS (
        SELECT bot_id,chat_id,message_thread_id FROM telegram_chat_modes
        UNION SELECT bot_id,chat_id,message_thread_id FROM telegram_bindings
    ), waiting AS (
        SELECT command.telegram_bot_id AS bot_id,command.telegram_chat_id AS chat_id,
               COALESCE(command.telegram_message_thread_id,0) AS topic_id,command.created_at AS waiting_since
        FROM commands command JOIN workers worker ON worker.worker_id=command.worker_id
        WHERE command.source='telegram' AND command.telegram_bot_id IS NOT NULL AND command.telegram_chat_id IS NOT NULL
          AND command.status IN ('pending','dispatched','acknowledged')
          AND (command.expires_at IS NULL OR command.expires_at>`+sqliteNow+`)
          AND worker.enabled=TRUE AND worker.connectivity='online'
          AND (command.session_id IS NULL OR `+sessionVisibleSQL("command.telegram_bot_id", "command.telegram_chat_id", "COALESCE(command.telegram_message_thread_id,0)", "command.session_id")+`)
          AND NOT EXISTS(SELECT 1 FROM sessions session WHERE session.session_id=command.session_id AND session.state IN ('waiting_input','waiting_approval'))
        UNION ALL
        SELECT destination.bot_id,destination.chat_id,destination.message_thread_id,COALESCE(session.last_activity_at,session.discovered_at)
        FROM destinations destination CROSS JOIN sessions session JOIN workers worker ON worker.worker_id=session.worker_id
        WHERE session.archived=FALSE AND session.state='running' AND session.active_turn_id IS NOT NULL
          AND worker.enabled=TRUE AND worker.connectivity='online'
          AND `+sessionVisibleSQL("destination.bot_id", "destination.chat_id", "destination.message_thread_id", "session.session_id")+`
    ) SELECT bot_id,chat_id,topic_id FROM waiting GROUP BY bot_id,chat_id,topic_id ORDER BY min(waiting_since),bot_id,chat_id,topic_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: list Telegram typing targets: %w", err)
	}
	defer rows.Close()
	targets := make([]TelegramTypingTarget, 0)
	for rows.Next() {
		var target TelegramTypingTarget
		if err := rows.Scan(&target.BotID, &target.ChatID, &target.TopicID); err != nil {
			return nil, fmt.Errorf("registry: scan Telegram typing target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list Telegram typing targets: %w", err)
	}
	return targets, nil
}

// IsTelegramTypingTargetActive is a last-moment check before sending presence.
func (s *Store) IsTelegramTypingTargetActive(ctx context.Context, target TelegramTypingTarget) (bool, error) {
	targets, err := s.ListTelegramTypingTargets(ctx, 1000)
	if err != nil {
		return false, err
	}
	for _, candidate := range targets {
		if candidate == target {
			return true, nil
		}
	}
	return false, nil
}
