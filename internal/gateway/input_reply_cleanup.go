package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/iaia/telegramgw/internal/registry"
)

func inputReplyCleanupTarget(row registry.Delivery) (int64, error) {
	var cleanup struct {
		MessageID int64 `json:"message_id"`
	}
	if row.ChatID == 0 || json.Unmarshal(row.Payload, &cleanup) != nil || cleanup.MessageID <= 0 {
		return 0, errors.New("invalid Telegram answer prompt cleanup")
	}
	return cleanup.MessageID, nil
}

func renderInputReplyCleanup(row registry.Delivery) ([]json.RawMessage, error) {
	if _, err := inputReplyCleanupTarget(row); err != nil {
		return nil, err
	}
	// This is an operation checkpoint, never a new Telegram message. Its
	// destination and identity come from the original cleanup envelope only.
	raw, err := json.Marshal(SendMessage{ChatID: row.ChatID, TopicID: row.TopicID})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{raw}, nil
}

func (s *Sender) sendInputReplyCleanup(ctx context.Context, row registry.Delivery) (int64, error) {
	id, err := inputReplyCleanupTarget(row)
	if err != nil {
		return 0, err
	}
	api, ok := s.api.(TelegramDeleteAPI)
	if !ok {
		return 0, errors.New("Telegram answer prompt cleanup unavailable")
	}
	err = api.DeleteMessage(ctx, row.ChatID, id)
	if err == nil {
		return id, nil
	}
	var telegram *TelegramError
	if errors.As(err, &telegram) && telegram.RetryAfter > 0 {
		return id, err
	}
	if telegramMessageAlreadyDeleted(err) {
		return id, nil
	}
	if errors.As(err, &telegram) && telegram.Code == 400 {
		description := strings.ToLower(strings.TrimSpace(telegram.Description))
		if description == "bad request: message can't be deleted" || description == "bad request: message cannot be deleted" {
			// Telegram cannot delete some old messages (including those past
			// its deletion window). Retire the work without asserting success
			// or posting a replacement explanation into the user's chat.
			store, ok := s.store.(interface {
				SkipDelivery(context.Context, string) error
			})
			if !ok {
				return 0, errors.New("Telegram answer prompt cleanup cancellation unavailable")
			}
			if err := store.SkipDelivery(ctx, row.ID); err != nil {
				return 0, err
			}
			s.log.Warn("Telegram could not delete a temporary answer prompt; cleanup cancelled and message may remain", "delivery_id", row.ID, "chat_id", row.ChatID, "message_id", id)
			return 0, errSessionDeliverySuppressed
		}
	}
	return id, err
}
