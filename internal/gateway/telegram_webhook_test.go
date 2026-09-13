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
		r := httptest.NewRequest("POST", "/api/v1/telegram/webhook", strings.NewReader(tc.body))
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
	action, target, text, ignore := parseTelegramText("/connect@mybot session name", "mybot")
	if action != "connect" || target != "session name" || text != "" || ignore {
		t.Fatal(action, target, text, ignore)
	}
	_, _, _, ignore = parseTelegramText("/new@otherbot", "mybot")
	if !ignore {
		t.Fatal("foreign bot mention processed")
	}
	action, _, text, _ = parseTelegramText("/steer keep the files", "bot")
	if action != "steer" || text != "keep the files" {
		t.Fatal("steer parse")
	}
	action, _, text, _ = parseTelegramText("regular prompt", "bot")
	if action != "text" || text != "regular prompt" {
		t.Fatal("ordinary message parse")
	}
}
