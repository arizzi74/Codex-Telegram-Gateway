package gateway

import "github.com/iaia/telegramgw/internal/config"

// TelegramBotResolver resolves durable routing IDs to enabled bot instances.
// A missing ID must never be routed through another bot's token.
type TelegramBotResolver interface {
	ResolveTelegramBot(string) (config.GatewayConfig, *TelegramClient, bool)
	TelegramBotIDs() []string
}

func telegramBotUsername(cfg config.GatewayConfig) string {
	if cfg.Secrets.BotUsername != "" {
		return cfg.Secrets.BotUsername
	}
	return cfg.Secrets.BotName
}
