package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type dashboardBotAPI struct {
	identity      gateway.TelegramUser
	webhook       gateway.WebhookInfo
	err           error
	identityCalls atomic.Int64
	webhookCalls  atomic.Int64
}

func (a *dashboardBotAPI) Identity(context.Context) (gateway.TelegramUser, error) {
	a.identityCalls.Add(1)
	return a.identity, a.err
}
func (a *dashboardBotAPI) GetWebhook(context.Context) (gateway.WebhookInfo, error) {
	a.webhookCalls.Add(1)
	return a.webhook, a.err
}

func TestBotMonitorCachesConcurrentDashboardReads(t *testing.T) {
	a := &dashboardBotAPI{
		identity: gateway.TelegramUser{ID: 123, IsBot: true, Username: "example_bot", FirstName: "Example", LastName: "Bot"},
		webhook:  gateway.WebhookInfo{URL: passkeyTestOrigin + "/tgapi/v1/telegram/webhook", PendingUpdates: 3},
	}
	m := newBotMonitor(Config{Origin: passkeyTestOrigin, BotAPI: a, BotUsername: "@configured_bot", AllowedUserCount: 2, AllowedChatCount: 1})
	var reads sync.WaitGroup
	for range 12 {
		reads.Go(func() {
			info := m.snapshot(context.Background())
			if info.Status != "connected" || info.WebhookStatus != "active" || info.ID != 123 || info.DisplayName != "Example Bot" || info.Username != "example_bot" || info.ConfiguredUsername != "configured_bot" || info.PendingUpdates == nil || *info.PendingUpdates != 3 || info.AllowedUserCount != 2 || info.AllowedChatCount != 1 || info.CheckedAt.IsZero() {
				t.Errorf("unexpected bot info: %+v", info)
			}
		})
	}
	reads.Wait()
	if a.identityCalls.Load() != 1 || a.webhookCalls.Load() != 1 {
		t.Fatalf("uncached checks: identity=%d webhook=%d", a.identityCalls.Load(), a.webhookCalls.Load())
	}
	m.cached.CheckedAt = time.Now().Add(-time.Minute)
	m.snapshot(context.Background())
	if a.identityCalls.Load() != 2 || a.webhookCalls.Load() != 2 {
		t.Fatal("expired cache did not refresh")
	}
}

func TestBotMonitorSanitizesWebhookAndFailures(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{"private-test-token"}, "[REDACTED]")
	if err != nil {
		t.Fatal(err)
	}
	a := &dashboardBotAPI{
		identity: gateway.TelegramUser{ID: 123, IsBot: true, Username: "example_bot"},
		webhook:  gateway.WebhookInfo{URL: "https://user:password@gateway.example.com/hook?secret=hidden#fragment", LastError: "rejected private-test-token", LastErrorDate: 1700000000},
	}
	info := newBotMonitor(Config{Origin: passkeyTestOrigin, BotAPI: a, Redactor: redactor}).snapshot(context.Background())
	if info.WebhookURL != "https://gateway.example.com/hook" || info.WebhookStatus != "mismatch" || info.LastError != "rejected [REDACTED]" || info.LastErrorAt == nil || info.LastErrorAt.Unix() != 1700000000 {
		t.Fatalf("unexpected sanitized webhook: %+v", info)
	}
	a.err = errors.New("GET https://api.telegram.org/botprivate-test-token/getMe: private response")
	info = newBotMonitor(Config{Origin: passkeyTestOrigin, BotAPI: a}).snapshot(context.Background())
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "unavailable" || info.PendingUpdates != nil || strings.Contains(string(b), "private") || strings.Contains(string(b), "api.telegram.org") {
		t.Fatalf("failure leaks remote error: %s", b)
	}
	info = newBotMonitor(Config{Origin: passkeyTestOrigin}).snapshot(context.Background())
	if info.Status != "not_configured" || info.PendingUpdates != nil {
		t.Fatalf("unconfigured bot: %+v", info)
	}
}

func TestDashboardRequiresAuthenticationBeforeBotCheck(t *testing.T) {
	a := &dashboardBotAPI{}
	s, err := New(&registry.Store{}, Config{Origin: passkeyTestOrigin, BotAPI: a})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, passkeyTestOrigin+"/tgapi/v1/admin/dashboard", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status: %d", w.Code)
	}
	if a.identityCalls.Load() != 0 || a.webhookCalls.Load() != 0 {
		t.Fatal("unauthenticated request queried Telegram")
	}
}

func TestDashboardRedactsSavedConversationSecrets(t *testing.T) {
	r, err := auth.NewRedactor([]string{"private-test-token"}, "[REDACTED]")
	if err != nil {
		t.Fatal(err)
	}
	d := registry.AdminDashboard{Sessions: []registry.AdminSession{{Session: protocol.Session{Name: "private-test-token", Preview: "private-test-token", Stats: &protocol.SessionStats{LastMessage: "sent private-test-token", Model: "private-test-token", ReasoningEffort: "private-test-token"}}}}, Workers: []registry.AdminWorker{{Name: "private-test-token"}}, Runtimes: []protocol.Runtime{{Name: "private-test-token", ProfileID: "private-test-token", DefaultCWD: "private-test-token", LocalSocket: "private-test-token"}}}
	(&Server{redactor: r}).redactDashboard(&d)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "private-test-token") {
		t.Fatalf("dashboard contains unredacted secret: %s", b)
	}
}
