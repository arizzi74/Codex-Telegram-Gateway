package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

type sessionMenuFixture struct {
	contexts                 []registry.TelegramMenuContext
	aliases                  []registry.TelegramSessionAlias
	identity                 string
	deleteErrorChat          int64
	aliasErrorChat           int64
	setScopes, deletedScopes []BotCommandScope
	commands                 []BotCommand
}

func (f *sessionMenuFixture) ListTelegramMenuContexts(context.Context, string) ([]registry.TelegramMenuContext, error) {
	return f.contexts, nil
}
func (f *sessionMenuFixture) ListTelegramSessionAliases(_ context.Context, _ string, _, chat, _ int64) ([]registry.TelegramSessionAlias, error) {
	if chat == f.aliasErrorChat {
		return nil, fmt.Errorf("alias lookup unavailable")
	}
	return f.aliases, nil
}
func (f *sessionMenuFixture) Identity(context.Context) (TelegramUser, error) {
	return TelegramUser{Username: f.identity}, nil
}
func (f *sessionMenuFixture) SetScopedCommands(_ context.Context, scope BotCommandScope, c []BotCommand) error {
	f.setScopes = append(f.setScopes, scope)
	f.commands = c
	return nil
}
func (f *sessionMenuFixture) DeleteScopedCommands(_ context.Context, scope BotCommandScope) error {
	f.deletedScopes = append(f.deletedScopes, scope)
	if scope.ChatID == f.deleteErrorChat {
		return fmt.Errorf("chat is inaccessible")
	}
	return nil
}

func TestSessionMenusRefreshOnlyChangedAuthorizedScopes(t *testing.T) {
	f := &sessionMenuFixture{identity: "bot", aliases: []registry.TelegramSessionAlias{{Alias: "_project", Name: "Project"}}, contexts: []registry.TelegramMenuContext{
		{UserID: 7, ChatID: 7, MultiSession: true},
		{UserID: 7, ChatID: -10, TopicID: 2, MultiSession: true},
		{UserID: 7, ChatID: -10, TopicID: 3, MultiSession: true},
		{UserID: 8, ChatID: 8, MultiSession: true},
	}}
	m := NewSessionMenus(f, f, nil, SessionMenuOptions{BotID: "bot", AllowedUserIDs: []int64{7}})
	for i := 0; i < 2; i++ {
		if err := m.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.setScopes) != 2 || f.setScopes[0] != (BotCommandScope{Type: "chat_member", ChatID: -10, UserID: 7}) || f.setScopes[1] != (BotCommandScope{Type: "chat", ChatID: 7}) {
		t.Fatalf("wrong/repeated scopes: %#v", f.setScopes)
	}
	if f.commands[len(f.commands)-1].Command != "_project" {
		t.Fatal("session shortcut absent")
	}
	f.aliases[0].Name = "Renamed Project"
	if err := m.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.setScopes) != 4 || f.commands[len(f.commands)-1].Description != "Send to Renamed Project" {
		t.Fatal("renamed inventory did not refresh menu")
	}
	for i := range f.contexts {
		f.contexts[i].MultiSession = false
	}
	if err := m.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.deletedScopes) != 3 {
		t.Fatal("switching off did not remove personal shortcuts")
	}
}

func TestSessionMenusRemoveRevokedPersistedScopesAfterRestart(t *testing.T) {
	f := &sessionMenuFixture{identity: "bot", aliases: []registry.TelegramSessionAlias{{Alias: "_project", Name: "Project"}}, contexts: []registry.TelegramMenuContext{
		{UserID: 7, ChatID: 7, MultiSession: true},
		{UserID: 8, ChatID: 8, MultiSession: true},   // Revoked user, still in Telegram's persisted menu scopes.
		{UserID: 7, ChatID: -10, MultiSession: true}, // Revoked chat for a still-authorized user.
	}}
	m := NewSessionMenus(f, f, nil, SessionMenuOptions{BotID: "bot", AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{7}})
	for range 2 {
		if err := m.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.setScopes) != 1 || f.setScopes[0] != (BotCommandScope{Type: "chat", ChatID: 7}) {
		t.Fatalf("revoked context received session aliases: %#v", f.setScopes)
	}
	if len(f.deletedScopes) != 2 || f.deletedScopes[0] != (BotCommandScope{Type: "chat_member", ChatID: -10, UserID: 7}) || f.deletedScopes[1] != (BotCommandScope{Type: "chat", ChatID: 8}) {
		t.Fatalf("revoked menus not cleared once on startup: %#v", f.deletedScopes)
	}
}

func TestSessionMenuFailureDoesNotBlockOtherScopes(t *testing.T) {
	for _, failure := range []string{"delete", "aliases"} {
		t.Run(failure, func(t *testing.T) {
			f := &sessionMenuFixture{identity: "bot", aliases: []registry.TelegramSessionAlias{{Alias: "_project", Name: "Project"}}, contexts: []registry.TelegramMenuContext{
				{UserID: 7, ChatID: -10, MultiSession: true},
				{UserID: 7, ChatID: 7, MultiSession: true},
			}}
			options := SessionMenuOptions{BotID: "bot", AllowedUserIDs: []int64{7}}
			if failure == "delete" {
				f.deleteErrorChat = -10
				options.AllowedChatIDs = []int64{7}
			} else {
				f.aliasErrorChat = -10
			}
			m := NewSessionMenus(f, f, nil, options)
			for range 2 {
				if err := m.flush(context.Background()); err == nil {
					t.Fatal("scope failure was not reported for retry")
				}
			}
			if len(f.setScopes) != 1 || f.setScopes[0] != (BotCommandScope{Type: "chat", ChatID: 7}) {
				t.Fatalf("one scope failure prevented another menu or retried a success: %#v", f.setScopes)
			}
			if _, applied := m.applied[sessionMenuKey{chat: -10, user: 7}]; applied {
				t.Fatal("failed scope was recorded as applied")
			}
		})
	}
}

func TestSessionMenusVerifyBotBeforeMutation(t *testing.T) {
	f := &sessionMenuFixture{identity: "wrong"}
	m := NewSessionMenus(f, f, nil, SessionMenuOptions{BotID: "bot", AllowedUserIDs: []int64{7}})
	if m.flush(context.Background()) == nil || len(f.deletedScopes) > 0 || len(f.setScopes) > 0 {
		t.Fatal("mismatched bot changed menus")
	}
}

func TestSessionMenusRespectTelegramLimits(t *testing.T) {
	aliases := []registry.TelegramSessionAlias{{Alias: "_duplicate", Name: "Project"}, {Alias: "_duplicate", Name: "Project"}, {Alias: "_invalid-name", Name: "Invalid"}}
	for i := 0; i < 110; i++ {
		aliases = append(aliases, registry.TelegramSessionAlias{Alias: fmt.Sprintf("_session_%03d", i), Name: strings.Repeat("界", 300)})
	}
	commands := sessionMenuCommands(aliases, nil)
	if len(commands) != 100 {
		t.Fatalf("menu count=%d", len(commands))
	}
	seen := map[string]bool{}
	for _, command := range commands {
		if seen[command.Command] || len([]rune(command.Description)) > 256 || command.Command == "_invalid-name" {
			t.Fatalf("bad entry %#v", command)
		}
		seen[command.Command] = true
	}
}
