package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/iaia/telegramgw/internal/registry"
)

var ErrTelegramManagedBotIntentConflict = errors.New("Telegram creation request is already active or connected")

// RemoveTelegramManagedBotIntent removes local creation history only. A
// claimed request must be managed through its connected bot instead.
func (m *TelegramBotManager) RemoveTelegramManagedBotIntent(ctx context.Context, id string) error {
	m.op.Lock()
	defer m.op.Unlock()
	in, err := m.ownedTelegramManagedBotIntent(ctx, id)
	if err != nil {
		return err
	}
	err = m.store.DeleteTelegramManagedBotIntent(ctx, in.ID, m.cfg.Secrets.WLID)
	return telegramManagedBotIntentError(err, "Telegram creation request could not be removed")
}

// RetryTelegramManagedBotIntent rechecks the original manager and generates a
// fresh checked username from the display name. The unsuccessful request stays
// intact until its replacement is committed successfully. Its old username is
// excluded so a delayed callback cannot claim the replacement request.
func (m *TelegramBotManager) RetryTelegramManagedBotIntent(ctx context.Context, id string) (TelegramManagedBotIntent, error) {
	m.op.Lock()
	defer m.op.Unlock()
	in, err := m.ownedTelegramManagedBotIntent(ctx, id)
	if err != nil {
		return TelegramManagedBotIntent{}, err
	}
	if in.BotID != "" || (in.Status != "expired" && in.Status != "failed" && in.Status != "cancelled") {
		return TelegramManagedBotIntent{}, ErrTelegramManagedBotIntentConflict
	}
	if in.ManagerID == "" {
		return TelegramManagedBotIntent{}, ErrTelegramBotInvalid
	}
	return m.createManagedTelegramBot(ctx, in.ManagerID, "", in.DisplayName, in.RequestedUsername, func(ctx context.Context, next registry.TelegramManagedBotIntentRecord) error {
		return m.store.ReplaceTelegramManagedBotIntent(ctx, in, next)
	})
}

func (m *TelegramBotManager) ownedTelegramManagedBotIntent(ctx context.Context, id string) (registry.TelegramManagedBotIntentRecord, error) {
	if strings.TrimSpace(id) == "" {
		return registry.TelegramManagedBotIntentRecord{}, ErrTelegramBotInvalid
	}
	in, err := m.store.TelegramManagedBotIntent(ctx, id)
	if err != nil {
		return in, telegramManagedBotIntentError(err, "Telegram creation request could not be read")
	}
	if in.OwnerID != m.cfg.Secrets.WLID {
		return in, ErrTelegramBotInvalid
	}
	return in, nil
}

func telegramManagedBotIntentError(err error, message string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, registry.ErrTelegramBotRecordNotFound):
		return ErrTelegramBotNotFound
	case errors.Is(err, registry.ErrTelegramManagedBotIntentConflict):
		return ErrTelegramManagedBotIntentConflict
	default:
		return errors.New(message)
	}
}
