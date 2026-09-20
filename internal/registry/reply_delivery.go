package registry

import (
	"context"
	"errors"
	"fmt"
)

// ErrTelegramReplyPending leaves the incoming update unaccepted so its caller
// can retry after the sender checkpoints a just-delivered bot message's route.
var ErrTelegramReplyPending = errors.New("registry: Telegram reply route is pending")

// Telegram can expose a sent message before its message ID is committed here.
// An unknown reply during that interval must not fall back to the selected
// session: it might be an answer to a question from a different session.
// The sender cannot know the new ID before Send returns, so an attempted,
// unfinished routable delivery in this chat is conservatively ambiguous.
func awaitTelegramReplyRoute(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	if in.ReplyToMessageID == 0 {
		return nil
	}
	var pending bool
	err := tx.QueryRow(ctx, `SELECT NOT EXISTS (
        SELECT 1 FROM bot_message_routes WHERE bot_id=$1 AND chat_id=$2 AND message_id=$3
    ) AND EXISTS (
        SELECT 1 FROM telegram_deliveries delivery
        WHERE delivery.bot_id=$1 AND delivery.chat_id=$2
          AND delivery.status IN ('pending','failed','sending') AND delivery.attempt_count>0
          AND (COALESCE(json_extract(delivery.payload,'$.session_id'),'')<>''
               OR COALESCE(json_extract(delivery.payload,'$.data.session.session_id'),'')<>'')
    )`, in.BotID, in.ChatID, in.ReplyToMessageID).Scan(&pending)
	if err != nil {
		return fmt.Errorf("registry: inspect pending reply route: %w", err)
	}
	if pending {
		return ErrTelegramReplyPending
	}
	return nil
}
