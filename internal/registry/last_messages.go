package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// The gateway rejects unsupported conversation reads before sending them to an
// older worker. No worker command_failed event will follow that acknowledgement,
// so acknowledge it in the originating chat instead of silently losing the read.
// The caller runs this only on the first transition to failed.
func notifyUnsupportedHistoryAcknowledgement(ctx context.Context, tx *dbTx, commandID uuid.UUID, code string) error {
	if code != protocol.UnsupportedOperation {
		return nil
	}
	var in IncomingUpdate
	var sessionID, runtimeID string
	err := tx.QueryRow(ctx, `SELECT telegram_bot_id,telegram_user_id,telegram_chat_id,COALESCE(telegram_message_thread_id,0),session_id,runtime_id
		FROM commands WHERE command_id=$1 AND operation='read_history'
		AND telegram_bot_id IS NOT NULL AND telegram_user_id IS NOT NULL AND telegram_chat_id IS NOT NULL`, commandID).
		Scan(&in.BotID, &in.UserID, &in.ChatID, &in.TopicID, &sessionID, &runtimeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return queueUIResponse(ctx, tx, in, AcceptResult{View: "error", ErrorCode: code, CommandID: commandID.String(), SessionID: sessionID, RuntimeID: runtimeID})
}

func validLastMessagesPage(page *protocol.HistoryPage, request *protocol.HistoryRequest) bool {
	if len(page.Prompts) != 0 || len(page.Messages) > page.Limit {
		return false
	}
	seen := make(map[protocol.HistoryCursor]bool, len(page.Messages))
	total := 0
	for _, message := range page.Messages {
		cursor := protocol.HistoryCursor{TurnID: message.TurnID, ItemID: message.ItemID}
		if !validHistoryCursor(cursor) || seen[cursor] || (request.Before != nil && cursor == *request.Before) ||
			(message.Role != "user" && message.Role != "assistant") || strings.TrimSpace(message.Text) == "" || !utf8.ValidString(message.Text) {
			return false
		}
		seen[cursor] = true
		length := utf8.RuneCountInString(message.Text)
		total += length
		if length > 16000 || total > 64000 {
			return false
		}
	}
	if page.Next != nil {
		if !validHistoryCursor(*page.Next) || len(page.Messages) == 0 {
			return false
		}
		oldest := page.Messages[0]
		if page.NewestFirst {
			oldest = page.Messages[len(page.Messages)-1]
		}
		if page.Next.TurnID != oldest.TurnID || page.Next.ItemID != oldest.ItemID {
			return false
		}
	}
	return true
}
