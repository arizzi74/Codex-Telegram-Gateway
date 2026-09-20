package registry

import (
	"context"
	"errors"
)

// Reposting progress waits for its old messages to disappear and for any next
// question/answer acknowledgement to be fully sent. This barrier is durable,
// scoped to the chat/topic, and covers both live events and restored progress.
var ErrTelegramProgressRepositionPending = errors.New("registry: Telegram progress reposition is pending")

// After three unsuccessful deletion attempts, new progress may continue while
// cleanup keeps retrying. A message Telegram refuses to delete must not freeze
// the whole chat indefinitely. Active deletion calls still finish first.
const pendingProgressRepositionSQL = `(EXISTS (
    SELECT 1 FROM telegram_deliveries response
    WHERE response.bot_id=delivery.bot_id AND response.chat_id=delivery.chat_id
      AND response.message_thread_id=delivery.message_thread_id
      AND response.kind='ui_response' AND json_extract(response.payload,'$.progress_reposition')=1
      AND response.visibility_revoked=0 AND response.status IN ('pending','failed','sending')
) OR EXISTS (
    SELECT 1 FROM telegram_progress_messages old
    JOIN telegram_deliveries previous ON previous.delivery_id=old.delivery_id
    WHERE old.bot_id=delivery.bot_id AND old.chat_id=delivery.chat_id
      AND old.message_thread_id=delivery.message_thread_id AND old.status<>'deleted'
      AND previous.progress_repositioned=1
      AND (old.status='deleting' OR old.attempt_count<3)
) OR EXISTS (
    SELECT 1 FROM telegram_deliveries inflight
    WHERE inflight.bot_id=delivery.bot_id AND inflight.chat_id=delivery.chat_id
      AND inflight.message_thread_id=delivery.message_thread_id
      AND inflight.progress_repositioned=1 AND inflight.status='sending'
      AND inflight.next_attempt_at>` + sqliteNow + `
))`

// AcceptTelegram invokes this only for an accepted question answer, after
// queueing its UI response and before committing the same deduplicated update.
// No session selection, conversation messages, or Codex events are changed.
func repositionTelegramProgress(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	if _, err := tx.Exec(ctx, `UPDATE telegram_progress_messages SET retire_requested=1,
        next_attempt_at=CASE WHEN status='pending' THEN `+sqliteNow+` ELSE next_attempt_at END
        WHERE bot_id=$1 AND chat_id=$2 AND message_thread_id=$3 AND status<>'deleted'`, in.BotID, in.ChatID, in.TopicID); err != nil {
		return err
	}
	// Fence sent deliveries too: a late successful edit/checkpoint must remain
	// retired. Replacement deliveries have fresh IDs, even for the same event.
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries AS delivery SET visibility_revoked=1,progress_repositioned=1
        WHERE bot_id=$1 AND chat_id=$2 AND message_thread_id=$3
          AND kind IN ('agent_progress_message','tool_progress_message') AND progress_repositioned=0
          AND (status IN ('pending','failed','sending') OR EXISTS (
              SELECT 1 FROM telegram_progress_messages shown
              WHERE shown.delivery_id=delivery.delivery_id AND shown.status<>'deleted'
          ))`, in.BotID, in.ChatID, in.TopicID); err != nil {
		return err
	}
	return reconcileTelegramSelection(ctx, tx, in)
}
