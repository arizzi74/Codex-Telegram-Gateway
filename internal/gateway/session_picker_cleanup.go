package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/iaia/telegramgw/internal/registry"
)

func pickerCleanupTarget(row registry.Delivery) (int64, error) {
	var cleanup registry.PickerCleanup
	if row.ChatID == 0 || json.Unmarshal(row.Payload, &cleanup) != nil || cleanup.MessageID <= 0 {
		return 0, errors.New("invalid Telegram session picker cleanup")
	}
	return cleanup.MessageID, nil
}

func renderPickerCleanup(row registry.Delivery) ([]json.RawMessage, error) {
	if _, err := pickerCleanupTarget(row); err != nil {
		return nil, err
	}
	// Use the ordinary durable delivery checkpoint and retry machinery. This
	// record is never posted as a Telegram message or given a session route.
	raw, err := json.Marshal(SendMessage{ChatID: row.ChatID, TopicID: row.TopicID})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{raw}, nil
}

func (s *Sender) sendPickerCleanup(ctx context.Context, row registry.Delivery) (int64, error) {
	id, err := pickerCleanupTarget(row)
	if err != nil {
		return 0, err
	}
	api, ok := s.api.(TelegramDeleteAPI)
	if !ok {
		return 0, errors.New("Telegram session picker cleanup unavailable")
	}
	err = api.DeleteMessage(ctx, row.ChatID, id)
	if telegramMessageAlreadyDeleted(err) {
		return id, nil
	}
	var telegram *TelegramError
	if errors.As(err, &telegram) && telegram.Code == 400 && telegram.RetryAfter == 0 {
		description := strings.ToLower(strings.TrimSpace(telegram.Description))
		if description == "bad request: message can't be deleted" || description == "bad request: message cannot be deleted" {
			// Telegram may refuse deletion of an old or inaccessible message.
			// Do not permanently strand the user's next selection behind it.
			s.log.Warn("Telegram could not remove an old session picker")
			return id, nil
		}
	}
	return id, err
}
