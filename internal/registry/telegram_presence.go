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
// accepted command still in flight or a Telegram-started turn still running.
// Requiring an online worker and, for session commands, the original binding
// makes disconnects stop presence without mutating command history.
func (s *Store) ListTelegramTypingTargets(ctx context.Context, limit int) ([]TelegramTypingTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("registry: invalid Telegram typing target limit")
	}
	rows, err := s.pool.Query(ctx, `WITH waiting AS (
        SELECT command.telegram_bot_id AS bot_id,
               command.telegram_chat_id AS chat_id,
               COALESCE(command.telegram_message_thread_id, 0) AS topic_id,
               command.created_at AS waiting_since
        FROM commands AS command
        JOIN workers AS worker ON worker.worker_id = command.worker_id
        WHERE command.source = 'telegram'
          AND command.telegram_bot_id IS NOT NULL
          AND command.telegram_chat_id IS NOT NULL
          AND command.status IN ('pending', 'dispatched', 'acknowledged')
          AND (command.expires_at IS NULL OR command.expires_at > now())
          AND worker.enabled = TRUE AND worker.connectivity = 'online'
          AND (command.session_id IS NULL OR EXISTS (
              SELECT 1 FROM telegram_bindings AS binding
              WHERE binding.bot_id = command.telegram_bot_id
                AND binding.user_id = command.telegram_user_id
                AND binding.chat_id = command.telegram_chat_id
                AND binding.message_thread_id = COALESCE(command.telegram_message_thread_id, 0)
          ))
          AND (command.session_id IS NULL OR NOT EXISTS (
              SELECT 1 FROM sessions AS command_session
              WHERE command_session.session_id = command.session_id
                AND command_session.state IN ('waiting_approval', 'waiting_input')
          ))
        UNION ALL
        SELECT command.telegram_bot_id AS bot_id,
               command.telegram_chat_id AS chat_id,
               COALESCE(command.telegram_message_thread_id, 0) AS topic_id,
               event.received_at AS waiting_since
        FROM sessions AS session
        JOIN workers AS worker ON worker.worker_id = session.worker_id
        JOIN events AS event
          ON event.worker_id = session.worker_id
         AND event.runtime_id = session.runtime_id
         AND event.session_id = session.session_id
         AND event.kind = 'turn_started'
         AND event.payload->>'turn_id' = session.active_turn_id
        JOIN commands AS command
          ON command.command_id::text = event.payload->>'command_id'
         AND command.source = 'telegram'
        JOIN telegram_bindings AS binding
          ON binding.bot_id = command.telegram_bot_id
         AND binding.user_id = command.telegram_user_id
         AND binding.chat_id = command.telegram_chat_id
         AND binding.message_thread_id = COALESCE(command.telegram_message_thread_id, 0)
        WHERE session.archived = FALSE
          AND session.state = 'running'
          AND session.active_turn_id IS NOT NULL
          AND worker.enabled = TRUE AND worker.connectivity = 'online'
    )
    SELECT bot_id, chat_id, topic_id
    FROM waiting
    GROUP BY bot_id, chat_id, topic_id
    ORDER BY min(waiting_since), bot_id, chat_id, topic_id
    LIMIT $1`, limit)
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
