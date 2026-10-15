package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/gateway"
)

type adminTelegramBotsFake struct {
	calls    []string
	token    string
	takeover bool
	err      error
}

func (m *adminTelegramBotsFake) ListTelegramBots(context.Context) ([]gateway.TelegramBotSummary, error) {
	m.calls = append(m.calls, "list")
	return nil, m.err
}
func (m *adminTelegramBotsFake) ListTelegramManagedBotIntents(context.Context) ([]gateway.TelegramManagedBotIntent, error) {
	m.calls = append(m.calls, "pending")
	return nil, m.err
}
func (m *adminTelegramBotsFake) ConnectTelegramBot(_ context.Context, token string, takeover bool) (gateway.TelegramBotSummary, error) {
	m.calls = append(m.calls, "connect")
	m.token, m.takeover = token, takeover
	return gateway.TelegramBotSummary{}, m.err
}
func (m *adminTelegramBotsFake) ReplaceTelegramBotToken(_ context.Context, id, token string, takeover bool) (gateway.TelegramBotSummary, error) {
	m.calls = append(m.calls, "replace:"+id)
	m.token, m.takeover = token, takeover
	return gateway.TelegramBotSummary{}, m.err
}
func (m *adminTelegramBotsFake) SetTelegramBotEnabled(_ context.Context, id string, enabled bool) error {
	action := "disable:"
	if enabled {
		action = "enable:"
	}
	m.calls = append(m.calls, action+id)
	return m.err
}
func (m *adminTelegramBotsFake) RemoveTelegramBot(_ context.Context, id string) error {
	m.calls = append(m.calls, "remove:"+id)
	return m.err
}
func (m *adminTelegramBotsFake) CreateManagedTelegramBot(_ context.Context, managerID, username, name string) (gateway.TelegramManagedBotIntent, error) {
	m.calls = append(m.calls, "create:"+managerID+":"+username+":"+name)
	return gateway.TelegramManagedBotIntent{}, m.err
}

func TestTelegramBotAdminAuthenticationAndValidation(t *testing.T) {
	s, _, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{}
	s.telegramBots = m
	const base = "/tgw/api/v1/admin/telegram-bots"
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", base, ""},
		{"POST", base, `{"token":"example-token"}`},
		{"POST", base + "/managed", `{"username":"test_bot","name":"Test"}`},
		{"PATCH", base + "/telegram:123", `{"enabled":false}`},
		{"DELETE", base + "/telegram:123", `{"confirmed":true}`},
	} {
		w := webUICommandRequestForTest(s, endpoint.method, endpoint.path, endpoint.body, "", passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s returned %d", endpoint.method, endpoint.path, w.Code)
		}
		if endpoint.method == "GET" {
			continue
		}
		for _, boundary := range []struct{ origin, header, cookie string }{
			{"https://other.example", "csrf", "csrf"}, {"", "csrf", "csrf"},
			{passkeyTestOrigin, "wrong", "csrf"}, {passkeyTestOrigin, "", "csrf"},
		} {
			w := webUICommandRequestForTest(s, endpoint.method, endpoint.path, endpoint.body, token, boundary.origin, boundary.header, boundary.cookie)
			if w.Code != http.StatusForbidden {
				t.Fatalf("unsafe %s %s returned %d", endpoint.method, endpoint.path, w.Code)
			}
		}
	}
	for _, input := range []struct{ method, path, body string }{
		{"POST", base, `{"token":"secret","owner_id":42}`},
		{"POST", base, `{"token":"secret"} {}`},
		{"POST", base, `{"token":"` + strings.Repeat("x", 513) + `"}`},
		{"POST", base, `{"token":"secret","replace_id":"bad/path"}`},
		{"POST", base + "/managed", `{"username":"test_bot","name":"Test","owner_id":42}`},
		{"POST", base + "/managed", `{"username":"","name":"Test"}`},
		{"POST", base + "/managed", `{"username":"test_bot","name":"Test","manager_id":"bad/path"}`},
		{"PATCH", base + "/telegram:123", `{}`},
		{"PATCH", base + "/telegram:123", `{"enabled":null}`},
		{"PATCH", base + "/telegram:123", `{"enabled":true,"owner_id":42}`},
		{"DELETE", base + "/telegram:123", `{}`},
		{"DELETE", base + "/telegram:123", `{"confirmed":false}`},
		{"DELETE", base + "/telegram:123", `null`},
	} {
		w := webUICommandRequestForTest(s, input.method, input.path, input.body, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s %s returned %d: %s", input.method, input.path, w.Code, w.Body.String())
		}
	}
	if len(m.calls) != 0 {
		t.Fatalf("rejected request reached bot manager: %v", m.calls)
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	w := webUICommandRequestForTest(s, "POST", base, `{"token":"secret"}`, token, passkeyTestOrigin, "csrf", "csrf")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "reauthentication_required") || len(m.calls) != 0 {
		t.Fatalf("stale authentication reached manager: %d %s", w.Code, w.Body.String())
	}
}

func TestTelegramBotAdminOperationsAndSafeErrors(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{}
	s.telegramBots = m
	const base = "/tgw/api/v1/admin/telegram-bots"
	for _, tc := range []struct {
		method, path, body, action string
		status                     int
	}{
		{"POST", base, `{"token":" secret-token "}`, "connect", 201},
		{"POST", base, `{"replace_id":"telegram:123","token":"secret-token","takeover":true}`, "replace:telegram:123", 200},
		{"POST", base + "/managed", `{"manager_id":"primary_bot","username":"test_bot","name":"Test Bot"}`, "create:primary_bot:test_bot:Test Bot", 201},
		{"PATCH", base + "/telegram:123", `{"enabled":false}`, "disable:telegram:123", 204},
		{"PATCH", base + "/telegram:123", `{"enabled":true}`, "enable:telegram:123", 204},
		{"DELETE", base + "/telegram:123", `{"confirmed":true}`, "remove:telegram:123", 204},
	} {
		m.calls = nil
		w := webUICommandRequestForTest(s, tc.method, tc.path, tc.body, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != tc.status || len(m.calls) != 1 || m.calls[0] != tc.action || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("operation %s: %d %s calls=%v", tc.action, w.Code, w.Body.String(), m.calls)
		}
		if strings.Contains(w.Body.String(), "secret-token") {
			t.Fatal("token was reflected in a response")
		}
	}
	if m.token != "secret-token" || !m.takeover {
		t.Fatal("token replacement parameters were not forwarded")
	}
	list := webUICommandRequestForTest(s, "GET", base, "", token, "", "", "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"bots":[]`) || !strings.Contains(list.Body.String(), `"pending":[]`) {
		t.Fatalf("empty collection: %d %s", list.Code, list.Body.String())
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{gateway.ErrTelegramBotInvalid, 400, "invalid_telegram_bot"},
		{gateway.ErrTelegramBotNotFound, 404, "telegram_bot_not_found"},
		{gateway.ErrTelegramBotDuplicate, 409, "telegram_bot_duplicate"},
		{gateway.ErrTelegramBotLegacy, 409, "telegram_bot_legacy"},
		{gateway.ErrTelegramBotWebhookConflict, 409, "telegram_bot_webhook_conflict"},
		{gateway.ErrTelegramBotManagerUnavailable, 409, "telegram_bot_manager_unavailable"},
		{gateway.ErrTelegramBotOwnerChanged, 409, "telegram_bot_owner_changed"},
		{errors.New("https://api.telegram.org/botsecret-token/getMe /private/key"), 502, "telegram_bot_unavailable"},
	} {
		m.err = tc.err
		w := webUICommandRequestForTest(s, "POST", base, `{"token":"secret-token"}`, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "secret-token") || strings.Contains(w.Body.String(), "/private/") {
			t.Fatalf("unsafe error response: %d %s", w.Code, w.Body.String())
		}
		if tc.err == gateway.ErrTelegramBotWebhookConflict && !strings.Contains(w.Body.String(), `"takeover_required":true`) {
			t.Fatal("webhook takeover was not explicit")
		}
	}
}
