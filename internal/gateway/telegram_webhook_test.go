package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

type webhookStore struct{ calls []registry.IncomingUpdate }

func (s *webhookStore) PrepareTelegramImage(_ context.Context, in registry.IncomingUpdate) (registry.IncomingUpdate, error) {
	return in, nil
}

func (s *webhookStore) AcceptTelegram(_ context.Context, in registry.IncomingUpdate) (registry.AcceptResult, error) {
	s.calls = append(s.calls, in)
	return registry.AcceptResult{}, nil
}

func TestWebhookRejectsActorBeforeProcessing(t *testing.T) {
	store := &webhookStore{}
	cfg := config.GatewayConfig{AllowedUserIDs: []int64{7}, Secrets: config.BotSecrets{BotName: "bot"}}
	handler := NewWebhook(store, cfg, "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		header, body string
		want         int
	}{
		{"bad", `{"update_id":1,"message":{"from":{"id":7},"chat":{"id":7},"text":"hi"}}`, 401},
		{"secret", `{"update_id":2,"message":{"from":{"id":8,"username":"allowed-name"},"chat":{"id":7},"text":"hi"}}`, 403},
		{"secret", `{"update_id":3,"message":{"from":{"id":7},"chat":{"id":7},"text":"hi"}}`, 200},
	} {
		r := httptest.NewRequest("POST", "/tgapi/v1/telegram/webhook", strings.NewReader(tc.body))
		r.Header.Set("X-Telegram-Bot-Api-Secret-Token", tc.header)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(w.Code, tc.want)
		}
	}
	if len(store.calls) != 1 || store.calls[0].UserID != 7 || store.calls[0].Text != "hi" {
		t.Fatal("unauthorized update reached registry")
	}
}

func TestParseTelegramCommands(t *testing.T) {
	action, target, text, ignore := parseTelegramText("/tgsessions@mybot runtime name", "mybot")
	if action != "sessions" || target != "runtime name" || text != "" || ignore {
		t.Fatal(action, target, text, ignore)
	}
	_, _, _, ignore = parseTelegramText("/_my_project@otherbot prompt", "mybot")
	if !ignore {
		t.Fatal("foreign bot mention processed")
	}
	action, _, text, _ = parseTelegramText("/tgsteer keep the files", "bot")
	if action != "steer" || text != "keep the files" {
		t.Fatal("steer parse")
	}
	action, _, text, _ = parseTelegramText("regular prompt", "bot")
	if action != "text" || text != "regular prompt" {
		t.Fatal("ordinary message parse")
	}
}

func TestCodexAndGatewayNamespaces(t *testing.T) {
	for _, tc := range []struct{ input, action, target, text string }{
		{"/tgstatus", "status", "", ""},
		{"/tghistory", "history", "", ""},
		{"/tghistory@mybot 25", "history", "", "25"},
		{"/history", "unknown_command", "history", ""},
		{"/status", "codex", "status", ""},
		{"/model@mybot gpt-5.6-sol high", "codex", "model", "gpt-5.6-sol high"},
		{"/debug_config", "codex", "debug-config", ""},
		{"/debug-config", "codex", "debug-config", ""},
		{"/tgnew", "unknown_command", "tgnew", ""},
		{"/tgconnect old session", "unknown_command", "tgconnect", ""},
		{"/tglastmessages", "last_messages", "", ""},
		{"/tglastmessages@mybot 12", "last_messages", "", "12"},
		{"/tgmultisession", "multisession", "", ""},
		{"/tgmultisession off", "multisession", "", "off"},
		{"/_My_Project@mybot please check /status", "session_alias", "_my_project", "please check /status"},
		{"/_my_project", "session_alias", "_my_project", ""},
		{"/tgdeletesession", "delete_session", "", ""},
		{"/tgdeletesession@mybot runtime", "delete_session", "runtime", ""},
		{"/new", "codex", "new", ""},
		{"/connect old gateway name", "unknown_command", "connect", ""},
		{"/unknown never becomes a model prompt", "unknown_command", "unknown", ""},
		{"/start", "help", "", ""},
		{"/help", "codex", "help", ""},
	} {
		action, target, text, ignore := parseTelegramText(tc.input, "mybot")
		if ignore || action != tc.action || target != tc.target || text != tc.text {
			t.Fatalf("%q => %q %q %q ignore=%v", tc.input, action, target, text, ignore)
		}
	}
}
