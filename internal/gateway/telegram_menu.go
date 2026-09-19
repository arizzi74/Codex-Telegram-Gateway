package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/iaia/telegramgw/internal/telegramcommands"
)

type telegramMenuAPI interface {
	Identity(context.Context) (TelegramUser, error)
	SetMyCommands(context.Context, []BotCommand) error
	SetChatMenuButton(context.Context, int64, MenuButton) error
}

// SyncCommandMenu keeps installed bots' menus current after binary updates.
// Check the bot identity before replacing any command registration.
func SyncCommandMenu(ctx context.Context, api telegramMenuAPI, botName string) error {
	identity, err := api.Identity(ctx)
	if err != nil {
		return err
	}
	if !strings.EqualFold(identity.Username, strings.TrimPrefix(botName, "@")) {
		return errors.New("Telegram token belongs to a different bot than BOTNAME")
	}
	if err := api.SetMyCommands(ctx, telegramcommands.Commands()); err != nil {
		return err
	}
	return api.SetChatMenuButton(ctx, 0, MenuButton{Type: "commands"})
}
