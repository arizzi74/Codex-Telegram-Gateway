package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/telegramcommands"
)

type SessionMenuStore interface {
	ListTelegramMenuContexts(context.Context, string) ([]registry.TelegramMenuContext, error)
	ListTelegramSessionAliases(context.Context, string, int64, int64, int64) ([]registry.TelegramSessionAlias, error)
}

type SessionMenuAPI interface {
	Identity(context.Context) (TelegramUser, error)
	SetScopedCommands(context.Context, BotCommandScope, []BotCommand) error
	DeleteScopedCommands(context.Context, BotCommandScope) error
}

type SessionMenuOptions struct {
	BotID                          string
	AllowedUserIDs, AllowedChatIDs []int64
	Redactor                       *auth.Redactor
}

type sessionMenuKey struct{ chat, user int64 }
type SessionMenus struct {
	store    SessionMenuStore
	api      SessionMenuAPI
	options  SessionMenuOptions
	log      *slog.Logger
	verified bool
	applied  map[sessionMenuKey]string
}

func NewSessionMenus(store SessionMenuStore, api SessionMenuAPI, logger *slog.Logger, options SessionMenuOptions) *SessionMenus {
	if logger == nil {
		logger = slog.Default()
	}
	return &SessionMenus{store: store, api: api, log: logger, options: options, applied: map[sessionMenuKey]string{}}
}

// Session menus refresh after toggles, discovery, creation, renames and deletion.
// Only changed scopes are written. Native session commands remain available
// for autocomplete in both delivery modes; revoked destinations lose the menu.
func (m *SessionMenus) Run(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		check, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.flush(check)
		cancel()
		if err != nil && ctx.Err() == nil {
			m.log.Warn("Telegram session command menu update deferred")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (m *SessionMenus) flush(ctx context.Context) error {
	if !m.verified {
		identity, err := m.api.Identity(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(identity.Username, strings.TrimPrefix(m.options.BotID, "@")) {
			return errors.New("Telegram menu bot identity mismatch")
		}
		m.verified = true
	}
	contexts, err := m.store.ListTelegramMenuContexts(ctx, m.options.BotID)
	if err != nil {
		return err
	}
	groups := map[sessionMenuKey][]registry.TelegramMenuContext{}
	for _, entry := range contexts {
		key := sessionMenuKey{chat: entry.ChatID, user: entry.UserID}
		// Telegram retains scoped menus across gateway restarts. Keep revoked
		// destinations in the cleanup set even when our in-memory applied map
		// is empty, so their old session aliases do not remain visible.
		if _, exists := groups[key]; !exists {
			groups[key] = nil
		}
		if !auth.AuthorizedTelegramUser(entry.UserID, m.options.AllowedUserIDs) || (len(m.options.AllowedChatIDs) > 0 && !containsChat(m.options.AllowedChatIDs, entry.ChatID)) {
			continue
		}
		groups[key] = append(groups[key], entry)
	}
	// Previously applied but no longer authorized scopes inherit the default.
	for key := range m.applied {
		if _, exists := groups[key]; !exists {
			groups[key] = nil
		}
	}
	keys := make([]sessionMenuKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].chat != keys[j].chat {
			return keys[i].chat < keys[j].chat
		}
		return keys[i].user < keys[j].user
	})
	var scopeErrors []error
scopes:
	for _, key := range keys {
		var aliases []registry.TelegramSessionAlias
		for _, entry := range groups[key] {
			items, err := m.store.ListTelegramSessionAliases(ctx, m.options.BotID, entry.UserID, entry.ChatID, entry.TopicID)
			if err != nil {
				scopeErrors = append(scopeErrors, err)
				continue scopes
			}
			aliases = append(aliases, items...)
		}
		scope := BotCommandScope{Type: "chat_member", ChatID: key.chat, UserID: key.user}
		if key.chat == key.user {
			scope.Type, scope.UserID = "chat", 0
		}
		commands := sessionMenuCommands(aliases, m.options.Redactor)
		raw, _ := json.Marshal(commands)
		hash := sha256.Sum256(raw)
		digest := string(hash[:])
		// Scope rendering can block on registry reads. Recheck authorization
		// before publishing session names, including restored scoped menus.
		allowed := auth.AuthorizedTelegramUser(key.user, m.options.AllowedUserIDs) && telegramChatAllowed(m.options.AllowedChatIDs, key.chat)
		if allowed {
			if store, ok := m.store.(telegramChatAuthorizationStore); ok {
				var err error
				allowed, err = store.TelegramChatAllowed(ctx, m.options.BotID, key.chat)
				if err != nil {
					scopeErrors = append(scopeErrors, err)
					continue
				}
			}
		}
		if len(groups[key]) == 0 || !allowed {
			if m.applied[key] != "off" {
				if err := m.api.DeleteScopedCommands(ctx, scope); err != nil {
					scopeErrors = append(scopeErrors, err)
					continue
				}
				m.applied[key] = "off"
			}
			continue
		}
		if m.applied[key] == digest {
			continue
		}
		if err := m.api.SetScopedCommands(ctx, scope, commands); err != nil {
			scopeErrors = append(scopeErrors, err)
			continue
		}
		m.applied[key] = digest
	}
	return errors.Join(scopeErrors...)
}

func containsChat(chats []int64, id int64) bool {
	for _, chat := range chats {
		if chat == id {
			return true
		}
	}
	return false
}

func sessionMenuCommands(aliases []registry.TelegramSessionAlias, redactor *auth.Redactor) []BotCommand {
	commands := telegramcommands.Commands()
	seen := map[string]bool{}
	for _, command := range commands {
		seen[command.Command] = true
	}
	aliases = append([]registry.TelegramSessionAlias(nil), aliases...)
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Alias < aliases[j].Alias })
	for _, entry := range aliases {
		if len(commands) >= 100 {
			break
		}
		if seen[entry.Alias] || !validSessionMenuAlias(entry.Alias) {
			continue
		}
		name := entry.Name
		if redactor != nil {
			name = redactor.Redact(name)
		}
		name = strings.Join(strings.Fields(name), " ")
		if name == "" {
			name = "Session"
		}
		description := []rune("Send to " + name)
		if len(description) > 256 {
			description = append(description[:255], '…')
		}
		commands = append(commands, BotCommand{Command: entry.Alias, Description: string(description)})
		seen[entry.Alias] = true
	}
	return commands
}

func validSessionMenuAlias(alias string) bool {
	if len(alias) < 2 || len(alias) > 32 || alias[0] != '_' {
		return false
	}
	for _, r := range alias {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
