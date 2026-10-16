package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/gateway"
)

func TestTelegramBotAdminQuestionPreferencesAndSafeErrors(t *testing.T) {
	s, _, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	m := &adminTelegramBotsFake{bots: []gateway.TelegramBotSummary{
		{ID: "primary", BroadcastQuestions: true},
		{ID: "telegram:123"},
	}}
	s.telegramBots = m
	const base = "/tgw/api/v1/admin/telegram-bots"
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodPost, base + "/refresh"},
	} {
		w := webUICommandRequestForTest(s, request.method, request.path, "", token, passkeyTestOrigin, "csrf", "csrf")
		var result struct {
			Bots []map[string]json.RawMessage `json:"bots"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Bots) != 2 {
			t.Fatalf("collection response = %d %s", w.Code, w.Body.String())
		}
		for i, expected := range []string{"true", "false"} {
			if got := string(result.Bots[i]["broadcast_questions"]); got != expected {
				t.Fatalf("bot %d question preference = %q, want %s", i, got, expected)
			}
		}
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{gateway.ErrTelegramBotNotFound, http.StatusNotFound, "telegram_bot_not_found"},
		{errors.New("https://api.telegram.org/botsecret-token/getMe /private/key"), http.StatusBadGateway, "telegram_bot_unavailable"},
	} {
		m.calls, m.err = nil, tc.err
		w := webUICommandRequestForTest(s, http.MethodPatch, base+"/telegram:123", `{"broadcast_questions":true}`, token, passkeyTestOrigin, "csrf", "csrf")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "secret-token") || strings.Contains(w.Body.String(), "/private/") || len(m.calls) != 1 || m.calls[0] != "broadcast-questions:telegram:123" {
			t.Fatalf("question preference error = %d %s calls=%v", w.Code, w.Body.String(), m.calls)
		}
	}
}
