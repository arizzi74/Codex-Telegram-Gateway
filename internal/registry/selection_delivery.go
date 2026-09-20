package registry

import (
	"context"
	"errors"
)

// ErrTelegramSelectionPending defers an already-leased progress message until
// the connection confirmation is fully checkpointed. It must not cancel the
// progress: a Telegram retry or a gateway restart can still deliver both.
var ErrTelegramSelectionPending = errors.New("registry: Telegram connection confirmation is pending")

// A selection and its replayed progress commit in one transaction. Depending
// on row creation order is insufficient: the confirmation may fail or be sent
// by another sender. Both live and replayed progress wait for its durable send.
const pendingSelectionConfirmationSQL = `EXISTS (
    SELECT 1 FROM telegram_deliveries confirmation
    WHERE confirmation.kind='ui_response'
      AND confirmation.status IN ('pending','failed','sending')
      AND confirmation.visibility_revoked=0
      AND confirmation.bot_id=delivery.bot_id AND confirmation.chat_id=delivery.chat_id
      AND confirmation.message_thread_id=delivery.message_thread_id
      AND json_extract(confirmation.payload,'$.view')='selected'
      AND json_extract(confirmation.payload,'$.session_id')=(
          SELECT session_id FROM events WHERE event_id=delivery.event_id)
)`

func retireSelectionConfirmations(ctx context.Context, tx *dbTx, in IncomingUpdate, result AcceptResult) error {
	if result.View != "selected" && result.View != "disconnected" {
		return nil
	}
	// A newer choice replaces any unsent connection announcement in this chat.
	// In particular, an old failed A confirmation cannot delay progress after
	// A -> B -> A once the new A confirmation succeeds.
	_, err := tx.Exec(ctx, `UPDATE telegram_deliveries SET visibility_revoked=1
        WHERE bot_id=$1 AND chat_id=$2 AND message_thread_id=$3
          AND kind='ui_response' AND json_extract(payload,'$.view')='selected'
          AND status IN ('pending','failed','sending') AND visibility_revoked=0`, in.BotID, in.ChatID, in.TopicID)
	return err
}
