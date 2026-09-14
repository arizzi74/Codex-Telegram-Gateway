package registry

// Durable Telegram delivery claims. Telegram itself has no idempotency key, so
// a crash after Send succeeds and before MarkDeliverySent commits can produce a
// duplicate message. The delivery ID and persisted chunk checkpoints minimize
// that unavoidable at-least-once boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Delivery struct {
	ID              string
	BotID           string
	ChatID, TopicID int64
	Kind            string
	Payload         json.RawMessage
	Attempt         int
}
type DeliveryChunk struct {
	Index   int
	Sent    bool
	Payload json.RawMessage
}

// ClaimDeliveries leases due rows. A crashed sender's sending lease is made
// pending again after 30 seconds before new rows are claimed.
func (s *Store) ClaimDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("registry: invalid delivery claim limit")
	}
	rows, err := s.pool.Query(ctx, `WITH claimed AS (
 SELECT delivery_id FROM telegram_deliveries WHERE status IN ('pending','failed','sending') AND next_attempt_at <= now() ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT $1
)
UPDATE telegram_deliveries d SET status='sending', attempt_count=d.attempt_count+1, next_attempt_at=now()+interval '30 seconds'
FROM claimed WHERE d.delivery_id=claimed.delivery_id
RETURNING d.delivery_id,d.bot_id,d.chat_id,d.message_thread_id,d.kind,d.payload,d.attempt_count`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Delivery
	for rows.Next() {
		var d Delivery
		var id uuid.UUID
		if err := rows.Scan(&id, &d.BotID, &d.ChatID, &d.TopicID, &d.Kind, &d.Payload, &d.Attempt); err != nil {
			return nil, err
		}
		d.ID = id.String()
		result = append(result, d)
	}
	return result, rows.Err()
}

// MarkDeliverySent checkpoints one Telegram message/chunk. Repeating it is
// harmless; routes are upserted by their Telegram message identity.
func (s *Store) MarkDeliverySent(ctx context.Context, id string, messageID int64, sessionID, turnID, approvalID string) error {
	deliveryID, err := uuid.Parse(id)
	if err != nil {
		return errors.New("registry: invalid delivery ID")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var bot string
	var chat int64
	if err = tx.QueryRow(ctx, `UPDATE telegram_deliveries SET status='sent', sent_at=now(), telegram_message_id=$2, last_error=NULL WHERE delivery_id=$1 AND status='sending' RETURNING bot_id,chat_id`, deliveryID, messageID).Scan(&bot, &chat); err != nil {
		return err
	}
	if err := checkpointTelegramProgress(ctx, tx, deliveryID, -1, messageID); err != nil {
		return err
	}
	if sessionID != "" {
		sid, err := uuid.Parse(sessionID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO bot_message_routes(bot_id,chat_id,message_id,session_id,turn_id,approval_id) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,'')::uuid) ON CONFLICT(bot_id,chat_id,message_id) DO NOTHING`, bot, chat, messageID, sid, turnID, approvalID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeliveryChunks loads the immutable messages frozen before the first API call.
func (s *Store) DeliveryChunks(ctx context.Context, id string) ([]DeliveryChunk, error) {
	rows, err := s.pool.Query(ctx, `SELECT chunk_index,status='sent',payload FROM telegram_delivery_chunks WHERE delivery_id=$1 ORDER BY chunk_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []DeliveryChunk
	for rows.Next() {
		var chunk DeliveryChunk
		if err := rows.Scan(&chunk.Index, &chunk.Sent, &chunk.Payload); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

// PrepareDeliveryChunks freezes full messages including callback keyboards.
// Repeated preparation returns the original content, even if snapshots changed.
func (s *Store) PrepareDeliveryChunks(ctx context.Context, id string, messages []json.RawMessage) ([]DeliveryChunk, error) {
	if len(messages) < 1 || len(messages) > 1000 {
		return nil, errors.New("registry: invalid delivery chunk count")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT true FROM telegram_deliveries WHERE delivery_id=$1 AND status='sending' FOR UPDATE`, id).Scan(&exists); err != nil {
		return nil, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM telegram_delivery_chunks WHERE delivery_id=$1`, id).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		for index, payload := range messages {
			if _, err = tx.Exec(ctx, `INSERT INTO telegram_delivery_chunks(delivery_id,chunk_index,payload) VALUES($1,$2,$3)`, id, index, payload); err != nil {
				return nil, err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.DeliveryChunks(ctx, id)
}

func (s *Store) ExtendDelivery(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=now()+interval '30 seconds' WHERE delivery_id=$1 AND status='sending'`, id)
	if err == nil && tag.RowsAffected() != 1 {
		return errors.New("registry: delivery is not leased")
	}
	return err
}

// MarkDeliveryChunkSent atomically checkpoints one chunk and creates its route.
// The parent delivery becomes sent only after every prepared chunk is sent.
func (s *Store) MarkDeliveryChunkSent(ctx context.Context, id string, index int, messageID int64, sessionID, turnID, approvalID string, questionIDs ...string) error {
	deliveryID, err := uuid.Parse(id)
	if err != nil {
		return errors.New("registry: invalid delivery ID")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var bot string
	var chat int64
	if err = tx.QueryRow(ctx, `SELECT bot_id,chat_id FROM telegram_deliveries WHERE delivery_id=$1 AND status='sending' FOR UPDATE`, deliveryID).Scan(&bot, &chat); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_delivery_chunks SET status='sent',telegram_message_id=$3,sent_at=now() WHERE delivery_id=$1 AND chunk_index=$2 AND status='pending'`, deliveryID, index, messageID); err != nil {
		return err
	}
	if err := checkpointTelegramProgress(ctx, tx, deliveryID, index, messageID); err != nil {
		return err
	}
	questionID := ""
	if len(questionIDs) > 0 {
		questionID = questionIDs[0]
	}
	if sessionID != "" {
		sid, err := uuid.Parse(sessionID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO bot_message_routes(bot_id,chat_id,message_id,session_id,turn_id,approval_id,question_id) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,'')::uuid,NULLIF($7,'')) ON CONFLICT DO NOTHING`, bot, chat, messageID, sid, turnID, approvalID, questionID); err != nil {
			return err
		}
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_delivery_chunks WHERE delivery_id=$1 AND status='pending')`, deliveryID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		_, err = tx.Exec(ctx, `UPDATE telegram_deliveries SET status='sent',sent_at=now(),telegram_message_id=$2,last_error=NULL WHERE delivery_id=$1`, deliveryID, messageID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) RetryDelivery(ctx context.Context, id string, retryAfter time.Duration, reason string) error {
	deliveryID, err := uuid.Parse(id)
	if err != nil {
		return errors.New("registry: invalid delivery ID")
	}
	if retryAfter <= 0 {
		retryAfter = time.Second
	}
	if retryAfter > time.Hour {
		retryAfter = time.Hour
	}
	_, err = s.pool.Exec(ctx, `UPDATE telegram_deliveries SET status='failed', next_attempt_at=now()+$2::interval, last_error=$3 WHERE delivery_id=$1 AND status='sending'`, deliveryID, retryAfter.String(), reason)
	return err
}
