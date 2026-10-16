package gateway

import (
	"context"
	"errors"

	"github.com/iaia/telegramgw/internal/registry"
)

// SetTelegramBotBroadcastQuestions changes only the durable question routing
// preference. It never updates Telegram's profile, webhook or credentials.
func (m *TelegramBotManager) SetTelegramBotBroadcastQuestions(ctx context.Context, id string, enabled bool) error {
	m.op.Lock()
	defer m.op.Unlock()
	if m.get(id) == nil {
		return ErrTelegramBotNotFound
	}
	if err := m.store.SetTelegramBotBroadcastQuestions(ctx, id, enabled); err != nil {
		if errors.Is(err, registry.ErrTelegramBotRecordNotFound) {
			return ErrTelegramBotNotFound
		}
		return errors.New("Telegram bot question preference could not be saved")
	}
	return nil
}

// Question preferences are read from the registry for each public summary,
// independently of the Telegram identity probe cache and runtime record writes.
func (m *TelegramBotManager) applyTelegramBotQuestionPreferences(ctx context.Context, summaries []TelegramBotSummary) error {
	preferences, err := m.store.TelegramBotQuestionPreferences(ctx)
	if err != nil {
		return errors.New("Telegram bot question preferences could not be loaded")
	}
	byID := make(map[string]bool, len(preferences))
	for _, preference := range preferences {
		byID[preference.BotID] = preference.BroadcastQuestions
	}
	for i := range summaries {
		summaries[i].BroadcastQuestions = byID[summaries[i].ID]
	}
	return nil
}
