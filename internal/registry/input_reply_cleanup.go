package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// InputReplyCleanup deletes the temporary composer helper, not the original
// question or the user's answer. Ordinary delivery leases make deletion durable.
type InputReplyCleanup struct {
	MessageID int64 `json:"message_id"`
}

const inputReplyReconcileBatch = 50

// Input answers are authoritative. Historical answer summaries intentionally
// survive a definitely failed submission and must not retire a reopened helper.
// Async questions survive ordinary turn changes, while blocking questions do not.
const inputReplyStillPendingSQL = `EXISTS (
 SELECT 1 FROM approvals approval
 JOIN runtimes runtime ON runtime.runtime_id=approval.runtime_id AND runtime.worker_id=approval.worker_id
 JOIN sessions session ON session.session_id=approval.session_id AND session.runtime_id=approval.runtime_id AND session.worker_id=approval.worker_id AND session.codex_thread_id=approval.codex_thread_id
 WHERE approval.approval_id=helper.approval_id AND approval.state='pending' AND approval.response_command_id IS NULL
   AND approval.runtime_generation=runtime.generation AND session.archived=0
   AND (json_extract(approval.request_payload,'$.async')=1 OR COALESCE(approval.codex_turn_id,'')='' OR COALESCE(approval.codex_turn_id,'')=COALESCE(session.active_turn_id,''))
   AND EXISTS(SELECT 1 FROM json_each(approval.request_payload,'$.questions') question WHERE json_extract(question.value,'$.id')=helper.question_id)
   AND NOT EXISTS(SELECT 1 FROM json_each(approval.input_answers) answer WHERE answer.key=helper.question_id AND json_array_length(answer.value)>0)
)`

func trackInputReplyHelper(ctx context.Context, tx *dbTx, id uuid.UUID, result AcceptResult) error {
	if result.View != "input_prompt" || !result.TextReply {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_input_reply_helpers(delivery_id,approval_id,question_id) VALUES($1,$2,$3)`, id, result.ApprovalID, result.QuestionID); err != nil {
		return err
	}
	return reconcileInputReplyHelper(ctx, tx, id)
}

// Metadata is indexed by approval; triggers enqueue only helpers whose current
// eligibility changed. At most fifty IDs are processed in a polling transaction.
func reconcilePendingInputReplyHelpers(ctx context.Context, tx *dbTx) error {
	rows, err := tx.Query(ctx, `SELECT delivery_id FROM telegram_input_reply_helpers WHERE backfill_pending=1 ORDER BY delivery_id LIMIT $1`, inputReplyReconcileBatch)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := reconcileInputReplyHelper(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func reconcileInputReplyHelper(ctx context.Context, tx *dbTx, id uuid.UUID) error {
	var stale bool
	err := tx.QueryRow(ctx, `SELECT helper.retired=1 OR delivery.visibility_revoked=1 OR NOT `+inputReplyStillPendingSQL+`
 FROM telegram_input_reply_helpers helper JOIN telegram_deliveries delivery USING(delivery_id) WHERE helper.delivery_id=$1`, id).Scan(&stale)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_input_reply_helpers SET backfill_pending=0,retired=CASE WHEN $2 THEN 1 ELSE retired END WHERE delivery_id=$1`, id, stale); err != nil {
		return err
	}
	if !stale {
		return nil
	}
	// Keep in-flight leases checkpointable. Every late message gets its own
	// idempotent deletion even if the sender no longer holds the original lease.
	if _, err = tx.Exec(ctx, `UPDATE telegram_deliveries SET visibility_revoked=1,status=CASE WHEN status IN ('pending','failed') THEN 'cancelled' ELSE status END WHERE delivery_id=$1`, id); err != nil {
		return err
	}
	// Migration knows the actual send time for most historical chunks. Avoid
	// doomed deletions outside Telegram's window; unknown timestamps still get
	// one durable attempt. A late send is dated at its new checkpoint, not the
	// age of the original delivery.
	rows, err := tx.Query(ctx, `SELECT message.bot_id,message.chat_id,delivery.message_thread_id,message.message_id
 FROM telegram_input_reply_messages message JOIN telegram_deliveries delivery USING(delivery_id) WHERE message.delivery_id=$1
 AND (message.sent_at IS NULL OR message.sent_at>(strftime('%Y-%m-%dT%H:%M:%f','now','-48 hours') || '000000Z'))`, id)
	if err != nil {
		return err
	}
	type target struct {
		bot                  string
		chat, topic, message int64
	}
	var messages []target
	for rows.Next() {
		var m target
		if err := rows.Scan(&m.bot, &m.chat, &m.topic, &m.message); err != nil {
			rows.Close()
			return err
		}
		messages = append(messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range messages {
		if err := enqueueInputReplyCleanup(ctx, tx, m.bot, m.chat, m.topic, m.message); err != nil {
			return err
		}
	}
	return nil
}

func enqueueInputReplyCleanup(ctx context.Context, tx *dbTx, bot string, chat, topic, message int64) error {
	key, err := json.Marshal([]any{"input_reply_cleanup", bot, chat, message})
	if err != nil {
		return err
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, key)
	payload, err := json.Marshal(InputReplyCleanup{MessageID: message})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,message_thread_id,kind,payload)
 VALUES($1,$2,$3,$4,'input_reply_cleanup',$5) ON CONFLICT(delivery_id) DO NOTHING`, id, bot, chat, topic, string(payload))
	return err
}

func checkpointInputReplyHelper(ctx context.Context, tx *dbTx, id uuid.UUID, message int64) error {
	if message <= 0 {
		return nil
	}
	// The helper's frozen delivery identifies its destination; no message text
	// heuristic or general question route is allowed to classify it as temporary.
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_input_reply_messages(bot_id,chat_id,message_id,delivery_id,sent_at)
 SELECT delivery.bot_id,delivery.chat_id,$2,helper.delivery_id,`+sqliteNow+` FROM telegram_input_reply_helpers helper JOIN telegram_deliveries delivery USING(delivery_id)
 WHERE helper.delivery_id=$1 ON CONFLICT(bot_id,chat_id,message_id) DO NOTHING`, id, message); err != nil {
		return err
	}
	return reconcileInputReplyHelper(ctx, tx, id)
}

// A helper leased before an answer can still reach the sender's final check.
// Recheck its exact indexed ID, retire it, and return a normal suppression.
func (s *Store) suppressInputReplyHelper(ctx context.Context, id string) (bool, error) {
	var stale bool
	err := s.pool.QueryRow(ctx, `SELECT helper.retired=1 OR delivery.visibility_revoked=1 OR NOT `+inputReplyStillPendingSQL+`
 FROM telegram_input_reply_helpers helper JOIN telegram_deliveries delivery USING(delivery_id) WHERE helper.delivery_id=$1`, id).Scan(&stale)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || !stale {
		return stale, err
	}
	deliveryID, err := uuid.Parse(id)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err = reconcileInputReplyHelper(ctx, tx, deliveryID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
