package admin

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/gateway"
)

func TestTelegramBotProfilePhotoResetExactRouteAndErrors(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{}
	s.telegramBots = m
	const base = "/tgw/api/v1/admin/telegram-bots"
	const endpoint = base + "/telegram:123/profile-photo/reset"
	for _, verb := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete, http.MethodPut} {
		w := webUICommandRequestForTest(s, verb, endpoint, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" || len(m.calls) != 0 {
			t.Fatalf("profile reset verb %s: %d %s calls=%v", verb, w.Code, w.Body.String(), m.calls)
		}
	}
	for _, path := range []string{
		base + "/profile-photo/reset",
		base + "//profile-photo/reset",
		base + "/bad/path/profile-photo/reset",
		base + "/telegram:123/profile-photo/reset/extra",
		base + "/telegram:123/profile-photo/reset/",
		base + "/telegram:123/photo/reset",
	} {
		w := webUICommandRequestForTest(s, http.MethodPost, path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
		if (w.Code >= 200 && w.Code < 300) || len(m.calls) != 0 {
			t.Fatalf("out-of-scope reset route %q: %d calls=%v", path, w.Code, m.calls)
		}
	}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{gateway.ErrTelegramBotDisabled, 409, "telegram_bot_disabled"},
		{gateway.ErrTelegramBotOwnerChanged, 409, "telegram_bot_owner_changed"},
		{gateway.ErrTelegramBotNotFound, 404, "telegram_bot_not_found"},
		{gateway.ErrTelegramBotProfileWebhookConflict, 409, "telegram_bot_profile_webhook_conflict"},
		{errors.New("https://api.telegram.org/botsecret-token /private/key"), 502, "telegram_bot_unavailable"},
	} {
		m.err, m.calls = test.err, nil
		w := webUICommandRequestForTest(s, http.MethodPost, endpoint, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != test.status || !strings.Contains(w.Body.String(), test.code) || strings.Contains(w.Body.String(), "secret-token") || strings.Contains(w.Body.String(), "/private/") || strings.Contains(w.Body.String(), "takeover_required") || len(m.calls) != 1 || m.calls[0] != "reset-photo:telegram:123" {
			t.Fatalf("reset error = %d %s calls=%v", w.Code, w.Body.String(), m.calls)
		}
	}
}
