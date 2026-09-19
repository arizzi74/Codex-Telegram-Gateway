package gateway

import (
	"context"
	"testing"
)

type menuRecorder struct {
	username string
	commands []BotCommand
	menu     MenuButton
}

func (m *menuRecorder) Identity(context.Context) (TelegramUser, error) {
	return TelegramUser{Username: m.username}, nil
}
func (m *menuRecorder) SetMyCommands(_ context.Context, commands []BotCommand) error {
	m.commands = commands
	return nil
}
func (m *menuRecorder) SetChatMenuButton(_ context.Context, _ int64, menu MenuButton) error {
	m.menu = menu
	return nil
}

func TestCommandMenuSyncPublishesSessionControlsForMatchingBot(t *testing.T) {
	m := &menuRecorder{username: "mybot"}
	if err := SyncCommandMenu(context.Background(), m, "@MyBot"); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, c := range m.commands {
		found[c.Command] = true
	}
	if found["tgnew"] || found["tgconnect"] || !found["tglastmessages"] || !found["tgmultisession"] || !found["tgdeletesession"] || !found["status"] || m.menu.Type != "commands" {
		t.Fatal("session controls missing from synchronized menu")
	}
	wrong := &menuRecorder{username: "anotherbot"}
	if err := SyncCommandMenu(context.Background(), wrong, "mybot"); err == nil || len(wrong.commands) > 0 || wrong.menu.Type != "" {
		t.Fatal("wrong bot's menu was changed")
	}
}
