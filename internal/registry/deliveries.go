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

// ErrDeliveryLeaseChanged tells a sender to leave a newer claim untouched.
var ErrDeliveryLeaseChanged = errors.New("registry: delivery lease changed")

// Empty polling must not scan completed history or acquire the writer lock.
// Pending/failed rows remain candidates even during backoff: revoked or
// superseded progress must still be cancelled before its next send attempt.
const deliveryQueueReadySQL = `SELECT EXISTS (
    SELECT 1 FROM telegram_deliveries
    WHERE status IN ('pending','failed','sending')
      AND (status IN ('pending','failed') OR next_attempt_at<=` + sqliteNow + `)
)`

const claimDeliveriesSQL = `WITH claimed AS (
 SELECT delivery.delivery_id FROM telegram_deliveries delivery
 WHERE delivery.status IN ('pending','failed','sending') AND delivery.visibility_revoked=0 AND delivery.next_attempt_at <= ` + sqliteNow + `
   AND (delivery.kind NOT IN ('agent_progress_message','tool_progress_message') OR (NOT ` + pendingSelectionConfirmationSQL + ` AND NOT ` + pendingProgressRepositionSQL + ` AND NOT ` + newerProgressDeliverySQL + ` AND NOT EXISTS (
     SELECT 1 FROM events progress
     JOIN events active_event ON active_event.runtime_id=progress.runtime_id
       AND active_event.runtime_generation=progress.runtime_generation AND active_event.session_id=progress.session_id
       AND json_extract(active_event.payload, '$.turn_id')=json_extract(progress.payload, '$.turn_id')
     JOIN telegram_deliveries active ON active.event_id=active_event.event_id
     WHERE progress.event_id=delivery.event_id AND active.kind=delivery.kind
       AND active.delivery_id<>delivery.delivery_id AND active.status='sending' AND active.next_attempt_at>` + sqliteNow + `
       AND active.bot_id=delivery.bot_id AND active.chat_id=delivery.chat_id
       AND active.message_thread_id=delivery.message_thread_id
   )))
 ORDER BY delivery.created_at LIMIT $1
)
UPDATE telegram_deliveries SET status='sending', attempt_count=attempt_count+1, next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','+30 seconds') || '000000Z')
WHERE delivery_id IN (SELECT delivery_id FROM claimed)
RETURNING delivery_id,bot_id,chat_id,message_thread_id,kind,payload,attempt_count`

// ClaimDeliveries atomically leases due rows in one UPDATE statement. SQLite
// serializes concurrent writers, so senders cannot claim the same live lease.
// A crashed sender's lease becomes eligible again after 30 seconds.
// Only the latest event of each progress kind at each destination is eligible.
// An existing send of that kind must finish or expire before its replacement.
// Newly selected progress also waits for the connection confirmation to send.
func (s *Store) ClaimDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("registry: invalid delivery claim limit")
	}
	var ready bool
	if err := s.pool.QueryRow(ctx, deliveryQueueReadySQL).Scan(&ready); err != nil {
		return nil, err
	}
	if !ready {
		return nil, nil
	}
	// New work arriving after the read above is picked up on the next poll.
	// The transaction and UPDATE below remain the authority for live leases.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries SET status='cancelled' WHERE status IN ('pending','failed','sending') AND visibility_revoked=1 AND (status IN ('pending','failed') OR (status='sending' AND next_attempt_at<=`+sqliteNow+`))`); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries AS delivery SET status='cancelled',last_error=NULL
        WHERE status IN ('pending','failed','sending') AND kind IN ('agent_progress_message','tool_progress_message')
          AND (status IN ('pending','failed') OR (status='sending' AND next_attempt_at<=`+sqliteNow+`))
          AND `+newerProgressDeliverySQL); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, claimDeliveriesSQL, limit)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
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
	if err = tx.QueryRow(ctx, `UPDATE telegram_deliveries SET status='sent', sent_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), telegram_message_id=$2, last_error=NULL WHERE delivery_id=$1 AND status='sending' RETURNING bot_id,chat_id`, deliveryID, messageID).Scan(&bot, &chat); err != nil {
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
		_, err = tx.Exec(ctx, `INSERT INTO bot_message_routes(bot_id,chat_id,message_id,session_id,turn_id,approval_id) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,'')) ON CONFLICT(bot_id,chat_id,message_id) DO NOTHING`, bot, chat, messageID, sid, turnID, approvalID)
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
	if err = tx.QueryRow(ctx, `SELECT true FROM telegram_deliveries WHERE delivery_id=$1 AND status='sending'`, id).Scan(&exists); err != nil {
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

// ReplaceUnsentSessionDeliveryChunks repairs a legacy session picker that is
// too large for Telegram. Only its unsent tail may change; every accepted
// message and its checkpoint survive. The lease attempt fences stale senders,
// including a sender whose render overlapped a newer delivery claim.
func (s *Store) ReplaceUnsentSessionDeliveryChunks(ctx context.Context, id string, attempt int, messages []json.RawMessage) ([]DeliveryChunk, error) {
	return s.replaceUnsentDeliveryChunks(ctx, id, attempt, messages,
		`kind='ui_response' AND json_extract(payload,'$.view')='sessions' AND COALESCE(json_extract(payload,'$.error_code'),'')=''`)
}

// ReplaceUnsentProgressDeliveryChunks compacts legacy frozen multipart progress
// to one replaceable message, preserving already sent chunks and their cleanup
// checkpoints. An expired or superseded sender cannot rewrite the newer lease.
func (s *Store) ReplaceUnsentProgressDeliveryChunks(ctx context.Context, id string, attempt int, messages []json.RawMessage) ([]DeliveryChunk, error) {
	if len(messages) != 1 {
		return nil, errors.New("registry: progress requires one replacement message")
	}
	return s.replaceUnsentDeliveryChunks(ctx, id, attempt, messages,
		`kind IN ('agent_progress_message','tool_progress_message')`)
}

// eligible is a fixed SQL predicate supplied only by the typed wrappers above.
func (s *Store) replaceUnsentDeliveryChunks(ctx context.Context, id string, attempt int, messages []json.RawMessage, eligible string) ([]DeliveryChunk, error) {
	if len(messages) < 1 || len(messages) > 1000 {
		return nil, errors.New("registry: invalid delivery chunk count")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var validLease bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM telegram_deliveries
		WHERE delivery_id=$1 AND (`+eligible+`)
		  AND status='sending' AND attempt_count=$2 AND next_attempt_at>`+sqliteNow+`
		  AND EXISTS (SELECT 1 FROM telegram_delivery_chunks WHERE delivery_id=$1 AND status='pending')
	)`, id, attempt).Scan(&validLease); err != nil {
		return nil, err
	}
	if !validLease {
		return nil, ErrDeliveryLeaseChanged
	}
	if _, err = tx.Exec(ctx, `DELETE FROM telegram_delivery_chunks WHERE delivery_id=$1 AND status='pending'`, id); err != nil {
		return nil, err
	}
	var nextIndex int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(chunk_index)+1,0) FROM telegram_delivery_chunks WHERE delivery_id=$1`, id).Scan(&nextIndex); err != nil {
		return nil, err
	}
	for index, payload := range messages {
		if _, err = tx.Exec(ctx, `INSERT INTO telegram_delivery_chunks(delivery_id,chunk_index,payload) VALUES($1,$2,$3)`, id, nextIndex+index, payload); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.DeliveryChunks(ctx, id)
}

func (s *Store) ExtendDelivery(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','+30 seconds') || '000000Z') WHERE delivery_id=$1 AND status='sending'`, id)
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
	if err = tx.QueryRow(ctx, `SELECT bot_id,chat_id FROM telegram_deliveries WHERE delivery_id=$1 AND status='sending'`, deliveryID).Scan(&bot, &chat); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_delivery_chunks SET status='sent',telegram_message_id=$3,sent_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE delivery_id=$1 AND chunk_index=$2 AND status='pending'`, deliveryID, index, messageID); err != nil {
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
		if _, err = tx.Exec(ctx, `INSERT INTO bot_message_routes(bot_id,chat_id,message_id,session_id,turn_id,approval_id,question_id) VALUES($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,'')) ON CONFLICT DO NOTHING`, bot, chat, messageID, sid, turnID, approvalID, questionID); err != nil {
			return err
		}
		if approvalID != "" && questionID != "" {
			id, err := uuid.Parse(approvalID)
			if err != nil {
				return err
			}
			if err := enqueueQuestionAnswerEdits(ctx, tx, id, bot, chat, messageID); err != nil {
				return err
			}
		}
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_delivery_chunks WHERE delivery_id=$1 AND status='pending')`, deliveryID).Scan(&pending); err != nil {
		return err
	}
	if !pending {
		_, err = tx.Exec(ctx, `UPDATE telegram_deliveries SET status='sent',sent_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),telegram_message_id=$2,last_error=NULL WHERE delivery_id=$1`, deliveryID, messageID)
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
	_, err = s.pool.Exec(ctx, `UPDATE telegram_deliveries SET status='failed', next_attempt_at=$2, last_error=$3 WHERE delivery_id=$1 AND status='sending'`, deliveryID, time.Now().UTC().Add(retryAfter), reason)
	return err
}
