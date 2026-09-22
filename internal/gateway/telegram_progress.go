package gateway

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/iaia/telegramgw/internal/registry"
)

// TelegramProgressStore keeps temporary messages tied to the exact turn and
// destination that produced them, independently of the current chat selection.
type TelegramProgressStore interface {
	SuppressProgressDelivery(context.Context, string) (bool, error)
	SkipDelivery(context.Context, string) error
	ClaimTelegramDeletions(context.Context, int) ([]registry.TelegramDeletion, error)
	MarkTelegramDeletionDone(context.Context, string) error
	RetryTelegramDeletion(context.Context, string, time.Duration, string) error
}

type TelegramDeleteAPI interface {
	DeleteMessage(context.Context, int64, int64) error
}

type TelegramReplaceableProgressStore interface {
	TelegramProgressTarget(context.Context, string) (int64, error)
}

type TelegramFormattedEditAPI interface {
	EditFormatted(context.Context, int64, SendMessage) error
}

func isProgressDelivery(kind string) bool {
	return kind == "agent_progress_message" || kind == "tool_progress_message"
}

func telegramTextLength(text string) int {
	length := 0
	for _, r := range text {
		length += utf16.RuneLen(r)
	}
	return length
}

// Each kind of progress occupies its own single replaceable message, including
// when its text exceeds Telegram's limit. Truncate after redaction and preserve
// Unicode boundaries rather than sending additional messages.
func compactProgress(text string) string {
	return compactProgressLimit(text, 4000)
}

func compactProgressLimit(text string, limit int) string {
	const suffix = "\n… (truncated)"
	if telegramTextLength(text) <= limit {
		return text
	}
	remaining := limit - telegramTextLength(suffix)
	for index, r := range text {
		remaining -= utf16.RuneLen(r)
		if remaining < 0 {
			return text[:index] + suffix
		}
	}
	return text
}

func (s *Sender) sendProgress(ctx context.Context, row registry.Delivery, message SendMessage) (int64, error) {
	store, ok := s.store.(TelegramReplaceableProgressStore)
	if !ok {
		return 0, errors.New("Telegram progress tracking unavailable")
	}
	api, ok := s.api.(TelegramFormattedEditAPI)
	if !ok {
		return 0, errors.New("Telegram progress editing unavailable")
	}
	id, err := store.TelegramProgressTarget(ctx, row.ID)
	if err != nil {
		return 0, err
	}
	if skip, err := s.skipInvisibleDelivery(ctx, row); err != nil {
		return 0, err
	} else if skip {
		return 0, errSessionDeliverySuppressed
	}
	if id == 0 {
		return s.sendAuthorizedMessage(ctx, row, message)
	}
	if err := s.authorizeDeliveryDestination(ctx, row, message.ChatID); err != nil {
		return 0, err
	}
	err = api.EditFormatted(ctx, id, message)
	var telegram *TelegramError
	if errors.As(err, &telegram) && telegram.Code == 400 {
		description := strings.ToLower(strings.TrimSpace(telegram.Description))
		// An edit may have succeeded just before a checkpoint failed. Treat
		// Telegram's unchanged-content response as a successful retry.
		if description == "bad request: message is not modified" || strings.HasPrefix(description, "bad request: message is not modified:") {
			return id, nil
		}
		// The user may delete the temporary message while the turn runs.
		// Only a definite missing-message error permits a fresh send.
		if description == "bad request: message to edit not found" {
			if skip, err := s.skipInvisibleDelivery(ctx, row); err != nil {
				return 0, err
			} else if skip {
				return 0, errSessionDeliverySuppressed
			}
			return s.sendAuthorizedMessage(ctx, row, message)
		}
	}
	return id, err
}

func (s *Sender) skipProgress(ctx context.Context, row registry.Delivery) (bool, error) {
	if !isProgressDelivery(row.Kind) {
		return false, nil
	}
	store, ok := s.store.(TelegramProgressStore)
	if !ok {
		return false, errors.New("Telegram progress tracking unavailable")
	}
	if _, ok := s.api.(TelegramDeleteAPI); !ok {
		return false, errors.New("Telegram progress cleanup unavailable")
	}
	skip, err := store.SuppressProgressDelivery(ctx, row.ID)
	if err != nil || !skip {
		return skip, err
	}
	return true, store.SkipDelivery(ctx, row.ID)
}

func (s *Sender) runDeletions(ctx context.Context, store TelegramProgressStore, api TelegramDeleteAPI) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := s.flushDeletions(ctx, store, api); err != nil && ctx.Err() == nil {
			s.log.Warn("telegram progress cleanup deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sender) flushDeletions(ctx context.Context, store TelegramProgressStore, api TelegramDeleteAPI) error {
	rows, err := store.ClaimTelegramDeletions(ctx, 1)
	if err != nil {
		return err
	}
	for _, row := range rows {
		// Like sends, only a short lease is held for one external API call.
		deleteCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := api.DeleteMessage(deleteCtx, row.ChatID, row.MessageID)
		cancel()
		if err == nil || telegramMessageAlreadyDeleted(err) {
			if err := store.MarkTelegramDeletionDone(ctx, row.ID); err != nil {
				return err
			}
			continue
		}
		if retryErr := store.RetryTelegramDeletion(ctx, row.ID, telegramRetryDelay(row.Attempt, err), "Telegram progress cleanup deferred"); retryErr != nil {
			return retryErr
		}
		return err
	}
	return nil
}

func telegramMessageAlreadyDeleted(err error) bool {
	var telegram *TelegramError
	return errors.As(err, &telegram) && telegram.Code == 400 &&
		strings.EqualFold(strings.TrimSpace(telegram.Description), "Bad Request: message to delete not found")
}
