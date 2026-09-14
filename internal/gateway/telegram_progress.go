package gateway

import (
	"context"
	"errors"
	"strings"
	"time"

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

func (s *Sender) skipProgress(ctx context.Context, row registry.Delivery) (bool, error) {
	if row.Kind != "agent_progress_message" {
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
