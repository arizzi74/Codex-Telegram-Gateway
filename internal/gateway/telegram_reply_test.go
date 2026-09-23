package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

type pendingReplyStore struct {
	webhookStore
	pending int
	err     error
}

func (s *pendingReplyStore) AcceptTelegram(_ context.Context, in registry.IncomingUpdate) (registry.AcceptResult, error) {
	s.calls = append(s.calls, in)
	if s.pending > 0 {
		s.pending--
		return registry.AcceptResult{}, registry.ErrTelegramReplyPending
	}
	return registry.AcceptResult{SessionID: "question-session"}, s.err
}

func TestWebhookWaitsForReplyRouteWithoutChangingUpdate(t *testing.T) {
	store := &pendingReplyStore{pending: 2}
	handler := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}, Secrets: config.BotSecrets{BotName: "bot"}}, "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRequest("POST", "/tgw/api/v1/telegram/webhook", strings.NewReader(`{"update_id":107,"message":{"from":{"id":7},"chat":{"id":9},"text":"Linux","reply_to_message":{"message_id":44}}}`))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || len(store.calls) != 3 {
		t.Fatalf("reply not retried: HTTP %d, calls=%d", w.Code, len(store.calls))
	}
	first := store.calls[0]
	if first.UpdateID != 107 || first.ReplyToMessageID != 44 || first.Text != "Linux" || first.ChatID != 9 || first.UserID != 7 {
		t.Fatalf("lost frozen reply context: %#v", first)
	}
	for _, call := range store.calls[1:] {
		if !reflect.DeepEqual(call, first) {
			t.Fatal("retry changed the original update")
		}
	}
}

func TestWebhookReplyRetryRemainsBoundedAndDoesNotRetryOtherErrors(t *testing.T) {
	for _, scenario := range []string{"pending", "database error"} {
		t.Run(scenario, func(t *testing.T) {
			store := &pendingReplyStore{}
			if scenario == "pending" {
				store.pending = 1000
			} else {
				store.err = errors.New("database unavailable")
			}
			handler := NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}, Secrets: config.BotSecrets{BotName: "bot"}}, "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
			defer cancel()
			r := httptest.NewRequest("POST", "/tgw/api/v1/telegram/webhook", strings.NewReader(`{"update_id":107,"message":{"from":{"id":7},"chat":{"id":9},"text":"Linux","reply_to_message":{"message_id":44}}}`)).WithContext(ctx)
			r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 503 || len(store.calls) == 0 {
				t.Fatalf("unfinished reply was acknowledged: HTTP %d, calls=%d", w.Code, len(store.calls))
			}
			if scenario == "pending" && ctx.Err() == nil {
				t.Fatal("pending reply did not wait for the deadline")
			}
			if scenario == "database error" && len(store.calls) != 1 {
				t.Fatal("unrelated registry failure was retried")
			}
		})
	}
}
