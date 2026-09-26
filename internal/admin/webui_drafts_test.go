package admin

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWebUIDraftAuthenticationCSRFAndValidation(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	ciphertext := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	body := fmt.Sprintf(`{"ciphertext":%q,"revision":1}`, ciphertext)
	for _, tc := range []struct {
		name, method, body, token, origin, csrf string
		want                                    int
	}{
		{"no login", "GET", "", "", "", "", 401},
		{"bad login", "GET", "", "bad", "", "", 401},
		{"wrong origin", "PUT", body, token, "https://evil.example", "csrf", 403},
		{"no csrf", "PUT", body, token, passkeyTestOrigin, "", 403},
		{"unknown field", "PUT", `{"ciphertext":"abc","revision":1,"plaintext":"secret"}`, token, passkeyTestOrigin, "csrf", 400},
		{"invalid envelope", "PUT", `{"ciphertext":"secret","revision":1}`, token, passkeyTestOrigin, "csrf", 400},
		{"invalid delete", "DELETE", `{"revision":0}`, token, passkeyTestOrigin, "csrf", 400},
		{"wrong method", "POST", body, token, passkeyTestOrigin, "csrf", 405},
		{"save", "PUT", body, token, passkeyTestOrigin, "csrf", 200},
		{"load", "GET", "", token, "", "", 200},
		{"clear", "DELETE", `{"revision":2}`, token, passkeyTestOrigin, "csrf", 204},
		{"stale save", "PUT", body, token, passkeyTestOrigin, "csrf", 409},
		{"consumed", "GET", "", token, "", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/tgw/api/v1/webui/drafts/"+id, strings.NewReader(tc.body))
			if tc.token != "" {
				r.AddCookie(cookie(adminCookie, tc.token, true))
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CSRF-Token", tc.csrf)
			if tc.csrf != "" {
				r.AddCookie(cookie(csrfCookie, tc.csrf, false))
			}
			w := httptest.NewRecorder()
			server.webuiDraft(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d want%d: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("draft response can be cached")
			}
		})
	}
	if err := store.RevokeAdminSession(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/tgw/api/v1/webui/drafts/"+id, nil)
	r.AddCookie(cookie(adminCookie, token, true))
	w := httptest.NewRecorder()
	server.webuiDraft(w, r)
	if w.Code != 401 {
		t.Fatal("revoked login accepted")
	}
}
