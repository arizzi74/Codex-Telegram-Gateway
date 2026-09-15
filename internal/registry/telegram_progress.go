package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// TelegramDeletion identifies one already-sent temporary message. Deletion is
// independently leased and retried; it never changes final-message delivery.
type TelegramDeletion struct {
	ID              string
	BotID           string
	ChatID, TopicID int64
	MessageID       int64
	Attempt         int
}

func eventTurnID(event protocol.Event) string {
	var result protocol.Result
	_ = json.Unmarshal(event.Data, &result)
	return result.TurnID
}

// SuppressProgressDelivery checks immediately before sending each temporary
// chunk. Once a terminal event is durable, even a delayed/retried progress
// delivery must not add more messages. A send already in flight may complete;
// its checkpoint remains eligible for the independent deletion queue.
func (s *Store) SuppressProgressDelivery(ctx context.Context, id string) (bool, error) {
	var suppress bool
	err := s.pool.QueryRow(ctx, `SELECT delivery.kind='agent_progress_message' AND (
        delivery.status IN ('sent','cancelled') OR EXISTS (
            SELECT 1 FROM events terminal
            WHERE terminal.runtime_id=progress.runtime_id
              AND ((terminal.runtime_generation=progress.runtime_generation
                    AND ((terminal.session_id=progress.session_id
                          AND json_extract(terminal.payload, '$.turn_id')=json_extract(progress.payload, '$.turn_id')
                          AND terminal.kind IN ('turn_completed','turn_failed','turn_interrupted'))
                         OR terminal.kind IN ('runtime_stopped','runtime_failed')))
                   OR (terminal.kind='runtime_started' AND terminal.runtime_generation>progress.runtime_generation))
        ) OR EXISTS (
            SELECT 1 FROM runtimes runtime WHERE runtime.runtime_id=progress.runtime_id
              AND (runtime.generation>progress.runtime_generation
                   OR (runtime.generation=progress.runtime_generation AND runtime.state IN ('stopped','failed')))
        ))
        FROM telegram_deliveries delivery LEFT JOIN events progress ON progress.event_id=delivery.event_id
        WHERE delivery.delivery_id=$1`, id).Scan(&suppress)
	return suppress, err
}

// SkipDelivery cancels only unsent work. Already checkpointed temporary chunks
// retain their cleanup rows, including when the remaining chunks are skipped.
func (s *Store) SkipDelivery(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("registry: invalid delivery ID")
	}
	_, err := s.pool.Exec(ctx, `UPDATE telegram_deliveries SET status='cancelled',last_error=NULL
        WHERE delivery_id=$1 AND status IN ('pending','failed','sending')`, id)
	return err
}

func checkpointTelegramProgress(ctx context.Context, tx *dbTx, deliveryID uuid.UUID, index int, messageID int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO telegram_progress_messages
        (cleanup_id,delivery_id,chunk_index,bot_id,chat_id,message_thread_id,telegram_message_id,
         runtime_id,runtime_generation,session_id,turn_id)
        SELECT $1,delivery.delivery_id,$2,delivery.bot_id,delivery.chat_id,delivery.message_thread_id,$3,
               event.runtime_id,event.runtime_generation,event.session_id,json_extract(event.payload, '$.turn_id')
        FROM telegram_deliveries delivery JOIN events event ON event.event_id=delivery.event_id
        WHERE delivery.delivery_id=$4 AND delivery.kind='agent_progress_message'
        ON CONFLICT(delivery_id,chunk_index) DO NOTHING`, uuid.New(), index, messageID, deliveryID)
	if err != nil {
		return fmt.Errorf("registry: checkpoint temporary message: %w", err)
	}
	return nil
}

// ClaimTelegramDeletions makes a temporary message due only after its final
// response was fully sent to the same bot/chat/topic. A stopped or replaced
// runtime with no queued terminal notification permits cleanup as a fallback.
// The joins are evaluated on every claim, covering restarts and progress sends
// that completed just after the terminal response was delivered.
func (s *Store) ClaimTelegramDeletions(ctx context.Context, limit int) ([]TelegramDeletion, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("registry: invalid deletion claim limit")
	}
	rows, err := s.pool.Query(ctx, `WITH due AS (
        SELECT progress.cleanup_id FROM telegram_progress_messages progress
        WHERE progress.status IN ('pending','deleting') AND progress.next_attempt_at<=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
          AND (EXISTS (
            SELECT 1 FROM telegram_deliveries delivery JOIN events terminal ON terminal.event_id=delivery.event_id
            WHERE delivery.status='sent' AND delivery.bot_id=progress.bot_id AND delivery.chat_id=progress.chat_id
              AND delivery.message_thread_id=progress.message_thread_id AND terminal.runtime_id=progress.runtime_id
              AND terminal.runtime_generation=progress.runtime_generation
              AND ((terminal.session_id=progress.session_id AND json_extract(terminal.payload, '$.turn_id')=progress.turn_id
                    AND terminal.kind IN ('turn_completed','turn_failed','turn_interrupted'))
                   OR terminal.kind='runtime_failed')
          ) OR (NOT EXISTS (
            SELECT 1 FROM telegram_deliveries delivery JOIN events terminal ON terminal.event_id=delivery.event_id
            WHERE delivery.bot_id=progress.bot_id AND delivery.chat_id=progress.chat_id
              AND delivery.message_thread_id=progress.message_thread_id AND terminal.runtime_id=progress.runtime_id
              AND terminal.runtime_generation=progress.runtime_generation
              AND ((terminal.session_id=progress.session_id AND json_extract(terminal.payload, '$.turn_id')=progress.turn_id
                    AND terminal.kind IN ('turn_completed','turn_failed','turn_interrupted'))
                   OR terminal.kind='runtime_failed')
          ) AND (EXISTS (
            SELECT 1 FROM events lifecycle WHERE lifecycle.runtime_id=progress.runtime_id
              AND ((lifecycle.kind='runtime_stopped' AND lifecycle.runtime_generation=progress.runtime_generation)
                   OR (lifecycle.kind='runtime_started' AND lifecycle.runtime_generation>progress.runtime_generation))
          ) OR EXISTS (
            SELECT 1 FROM runtimes runtime WHERE runtime.runtime_id=progress.runtime_id
              AND (runtime.generation>progress.runtime_generation
                   OR (runtime.generation=progress.runtime_generation AND runtime.state='stopped'))
          ))))
        ORDER BY progress.next_attempt_at,progress.cleanup_id LIMIT $1
    ) UPDATE telegram_progress_messages
      SET status='deleting',attempt_count=attempt_count+1,next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','+30 seconds') || '000000Z')
      WHERE cleanup_id IN (SELECT cleanup_id FROM due)
      RETURNING cleanup_id,bot_id,chat_id,message_thread_id,
                telegram_message_id,attempt_count`, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: claim temporary-message deletions: %w", err)
	}
	defer rows.Close()
	var result []TelegramDeletion
	for rows.Next() {
		var deletion TelegramDeletion
		if err := rows.Scan(&deletion.ID, &deletion.BotID, &deletion.ChatID, &deletion.TopicID, &deletion.MessageID, &deletion.Attempt); err != nil {
			return nil, err
		}
		result = append(result, deletion)
	}
	return result, rows.Err()
}

func (s *Store) MarkTelegramDeletionDone(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("registry: invalid deletion ID")
	}
	_, err := s.pool.Exec(ctx, `UPDATE telegram_progress_messages SET status='deleted',deleted_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),last_error=NULL
        WHERE cleanup_id=$1 AND status='deleting'`, id)
	return err
}

func (s *Store) RetryTelegramDeletion(ctx context.Context, id string, retryAfter time.Duration, reason string) error {
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("registry: invalid deletion ID")
	}
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	if retryAfter > time.Hour {
		retryAfter = time.Hour
	}
	_, err := s.pool.Exec(ctx, `UPDATE telegram_progress_messages SET status='pending',next_attempt_at=$2,last_error=$3
        WHERE cleanup_id=$1 AND status='deleting'`, id, time.Now().UTC().Add(retryAfter), reason)
	return err
}
