package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/gateway"
)

func TestTelegramManagedBotIntentAuthentication(t *testing.T) {
	s, _, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{}
	s.telegramBots = m
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodDelete, "/tgw/api/v1/admin/telegram-bots/managed/request-1"},
		{http.MethodPost, "/tgw/api/v1/admin/telegram-bots/managed/request-1/retry"},
	} {
		for _, boundary := range []struct {
			token, origin, header, cookie string
			status                        int
		}{
			{"", passkeyTestOrigin, "csrf", "csrf", http.StatusUnauthorized},
			{"invalid-session", passkeyTestOrigin, "csrf", "csrf", http.StatusUnauthorized},
			{token, "", "csrf", "csrf", http.StatusForbidden},
			{token, "https://other.example", "csrf", "csrf", http.StatusForbidden},
			{token, passkeyTestOrigin + "/", "csrf", "csrf", http.StatusForbidden},
			{token, passkeyTestOrigin, "", "csrf", http.StatusForbidden},
			{token, passkeyTestOrigin, "csrf", "", http.StatusForbidden},
			{token, passkeyTestOrigin, "wrong", "csrf", http.StatusForbidden},
		} {
			w := webUICommandRequestForTest(s, endpoint.method, endpoint.path, `{"confirmed":true}`, boundary.token, boundary.origin, boundary.header, boundary.cookie)
			if w.Code != boundary.status || len(m.calls) != 0 {
				t.Fatalf("unsafe %s %s: %d %s calls=%v", endpoint.method, endpoint.path, w.Code, w.Body.String(), m.calls)
			}
		}
	}
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodDelete, "/tgw/api/v1/admin/telegram-bots/managed/request-1"},
		{http.MethodPost, "/tgw/api/v1/admin/telegram-bots/managed/request-1/retry"},
	} {
		w := webUICommandRequestForTest(s, endpoint.method, endpoint.path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauthentication_required") || len(m.calls) != 0 {
			t.Fatalf("stale authentication reached intent manager: %d %s calls=%v", w.Code, w.Body.String(), m.calls)
		}
	}
}

func TestTelegramManagedBotIntentValidation(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{}
	s.telegramBots = m
	const base = "/tgw/api/v1/admin/telegram-bots/managed/"
	for _, endpoint := range []struct{ method, suffix string }{
		{http.MethodDelete, ""},
		{http.MethodPost, "/retry"},
	} {
		for _, body := range []string{
			"", `{}`, `null`, `[]`, `{"confirmed":false}`, `{"confirmed":null}`,
			`{"confirmed":"true"}`, `{"confirmed":1}`, `{"confirmed":true,"token":"secret-token"}`,
			`{"confirmed":true} {}`, `{"confirmed":true`,
			`{"confirmed":true}` + strings.Repeat(" ", maxEnrollmentBody),
			"{\"confirmed\":true,\"\xff\":1}",
		} {
			w := webUICommandRequestForTest(s, endpoint.method, base+"request-1"+endpoint.suffix, body, token, passkeyTestOrigin, "csrf", "csrf")
			if w.Code != http.StatusBadRequest || len(m.calls) != 0 || strings.Contains(w.Body.String(), "secret-token") {
				t.Fatalf("invalid confirmation %q: %d %s calls=%v", body, w.Code, w.Body.String(), m.calls)
			}
		}
		for _, id := range []string{"bad%2Fpath", "bad%5Cpath", "bad%20path", "bad%3Fpath", "bad%23path", "bad%00path", strings.Repeat("x", 161)} {
			w := webUICommandRequestForTest(s, endpoint.method, base+id+endpoint.suffix, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
			if w.Code != http.StatusBadRequest || len(m.calls) != 0 {
				t.Fatalf("invalid intent ID %q: %d %s calls=%v", id, w.Code, w.Body.String(), m.calls)
			}
		}
	}
}

func TestTelegramManagedBotIntentExactRoutes(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{}
	s.telegramBots = m
	const base = "/tgw/api/v1/admin/telegram-bots/managed"
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodDelete, base + "/request-1"},
		{http.MethodPost, base + "/request-1/retry"},
	} {
		for _, verb := range []string{http.MethodGet, http.MethodHead, http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodPost} {
			if verb == endpoint.method {
				continue
			}
			w := webUICommandRequestForTest(s, verb, endpoint.path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != endpoint.method || len(m.calls) != 0 {
				t.Fatalf("intent method %s %s: %d %s calls=%v", verb, endpoint.path, w.Code, w.Body.String(), m.calls)
			}
		}
	}
	for _, path := range []string{
		base + "/", base + "/request-1/", base + "/request-1/retry/", base + "/request-1/retry/extra",
		base + "/request-1/other", base + "/bad/path/retry", base + "//retry",
	} {
		for _, verb := range []string{http.MethodDelete, http.MethodPost} {
			w := webUICommandRequestForTest(s, verb, path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
			if (w.Code >= 200 && w.Code < 300) || len(m.calls) != 0 {
				t.Fatalf("out-of-scope intent route %s %q: %d calls=%v", verb, path, w.Code, m.calls)
			}
		}
	}
}

func TestTelegramManagedBotIntentOperationsAndSafeErrors(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{intent: gateway.TelegramManagedBotIntent{
		ID: "replacement-request", ManagerID: "primary", RequestedUsername: "FreshBot", DisplayName: "Fresh Bot",
		LaunchURL: "https://t.me/newbot?start=request", ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second), Status: "pending",
	}}
	s.telegramBots = m
	for _, endpoint := range []struct {
		method, path, action string
		status               int
	}{
		{http.MethodDelete, "/tgw/api/v1/admin/telegram-bots/managed/request-1", "remove-intent:request-1", http.StatusNoContent},
		{http.MethodPost, "/tgw/api/v1/admin/telegram-bots/managed/request-1/retry", "retry-intent:request-1", http.StatusCreated},
	} {
		m.calls, m.err = nil, nil
		w := webUICommandRequestForTest(s, endpoint.method, endpoint.path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != endpoint.status || len(m.calls) != 1 || m.calls[0] != endpoint.action || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("intent operation %s: %d %s calls=%v", endpoint.action, w.Code, w.Body.String(), m.calls)
		}
		if endpoint.status == http.StatusCreated {
			var got gateway.TelegramManagedBotIntent
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got != m.intent {
				t.Fatalf("retry response did not return fresh intent: %s err=%v", w.Body.String(), err)
			}
		} else if w.Body.Len() != 0 {
			t.Fatalf("intent removal body: %q", w.Body.String())
		}
		for _, test := range []struct {
			err    error
			status int
			code   string
		}{
			{gateway.ErrTelegramBotNotFound, http.StatusNotFound, "telegram_bot_not_found"},
			{gateway.ErrTelegramManagedBotIntentConflict, http.StatusConflict, "telegram_managed_bot_intent_conflict"},
			{fmt.Errorf("private-token /private/key: %w", gateway.ErrTelegramManagedBotIntentConflict), http.StatusConflict, "telegram_managed_bot_intent_conflict"},
			{gateway.ErrTelegramBotManagerUnavailable, http.StatusConflict, "telegram_bot_manager_unavailable"},
			{gateway.ErrTelegramBotUsernameUnavailable, http.StatusConflict, "telegram_bot_username_unavailable"},
			{errors.New("https://api.telegram.org/botprivate-token /private/key"), http.StatusBadGateway, "telegram_bot_unavailable"},
		} {
			m.calls, m.err = nil, test.err
			w := webUICommandRequestForTest(s, endpoint.method, endpoint.path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
			if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) || strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), "/private/") || len(m.calls) != 1 || m.calls[0] != endpoint.action {
				t.Fatalf("unsafe intent error response: %d %s calls=%v", w.Code, w.Body.String(), m.calls)
			}
		}
	}
}
