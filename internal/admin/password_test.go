package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/registry"
)

const passwordTestBody = `{"username":"operator","password":"correct horse battery staple"}`

func TestOptionalPasswordLoginSettingsAndSharedAccess(t *testing.T) {
	store := adminIntegrationStore(t)
	passkeyToken := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		return webUICommandRequestForTest(server, method, "/tgw/api/v1/admin/"+path, body, token, passkeyTestOrigin, "csrf", "csrf")
	}
	options := call("GET", "login/options", "", "")
	if options.Code != 200 || !strings.Contains(options.Body.String(), `"password":false`) {
		t.Fatalf("password enabled by default: %d %s", options.Code, options.Body.String())
	}
	if got := call("GET", "password", "", ""); got.Code != 401 {
		t.Fatalf("public password settings: %d", got.Code)
	}
	if got := call("PUT", "password", passwordTestBody, passkeyToken); got.Code != 200 {
		t.Fatalf("enable password: %d %s", got.Code, got.Body.String())
	}
	options = call("GET", "login/options", "", "")
	if options.Code != 200 || !strings.Contains(options.Body.String(), `"password":true`) || strings.Contains(options.Body.String(), "operator") {
		t.Fatalf("unsafe login options: %d %s", options.Code, options.Body.String())
	}
	// Unknown usernames and incorrect passwords produce identical errors.
	unknown := call("POST", "login/password", `{"username":"someone","password":"correct horse battery staple"}`, "")
	wrong := call("POST", "login/password", `{"username":"operator","password":"wrong password"}`, "")
	if unknown.Code != 401 || wrong.Code != 401 || unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("credential enumeration: %d %s / %d %s", unknown.Code, unknown.Body.String(), wrong.Code, wrong.Body.String())
	}
	login := call("POST", "login/password", passwordTestBody, "")
	if login.Code != 200 {
		t.Fatalf("password login: %d %s", login.Code, login.Body.String())
	}
	passwordToken := ""
	for _, c := range login.Result().Cookies() {
		if c.Name == adminCookie {
			passwordToken = c.Value
			if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
				t.Fatalf("insecure cookie: %+v", c)
			}
		}
	}
	if passwordToken == "" {
		t.Fatal("password login did not issue a session")
	}
	metadata := call("GET", "session", "", passwordToken)
	if metadata.Code != 200 || !strings.Contains(metadata.Body.String(), `"authentication_method":"password"`) {
		t.Fatalf("password identity: %d %s", metadata.Code, metadata.Body.String())
	}
	if got := call("GET", "dashboard", "", passwordToken); got.Code != 200 {
		t.Fatalf("password cannot open console: %d", got.Code)
	}
	webui := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions", "", passwordToken, "", "", "")
	if webui.Code != 200 {
		t.Fatalf("password cannot open Web UI: %d %s", webui.Code, webui.Body.String())
	}
	// Sensitive operations use the same recent-authentication checks.
	if got := call("POST", "worker-enrollments", `{"service_access":"restricted"}`, passwordToken); got.Code != 201 {
		t.Fatalf("fresh password rejected for worker enrollment: %d %s", got.Code, got.Body.String())
	}
	keys, err := store.AdminCredentials(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("password leaked into passkeys: %d %v", len(keys), err)
	}
	for _, path := range []string{"password", "passkeys", "sessions", "dashboard"} {
		got := call("GET", path, "", passwordToken)
		if got.Code != 200 || strings.Contains(got.Body.String(), "correct horse") || strings.Contains(got.Body.String(), "$argon2") {
			t.Fatalf("credential data exposed at %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	disabled := call("DELETE", "password", "", passwordToken)
	if disabled.Code != 204 {
		t.Fatalf("disable password: %d %s", disabled.Code, disabled.Body.String())
	}
	newCSRF := ""
	for _, c := range disabled.Result().Cookies() {
		if c.Name == csrfCookie {
			newCSRF = c.Value
		}
	}
	if newCSRF == "" {
		t.Fatal("password self-revocation prevents same-page sign-in by clearing CSRF")
	}
	if call("GET", "session", "", passwordToken).Code != 401 || call("GET", "session", "", passkeyToken).Code != 200 {
		t.Fatal("disable did not isolate password sessions from passkeys")
	}
	if got := call("POST", "login/password", passwordTestBody, ""); got.Code != 401 || got.Body.String() != wrong.Body.String() {
		t.Fatalf("disabled credential is distinguishable: %d %s", got.Code, got.Body.String())
	}
}

func TestPasswordHTTPBoundaryGuards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"login/password", "password"} {
		method := "POST"
		if endpoint == "password" {
			method = "PUT"
		}
		for _, tc := range []struct{ origin, header, cookie string }{
			{"https://evil.example", "csrf", "csrf"}, {"", "csrf", "csrf"}, {passkeyTestOrigin, "wrong", "csrf"}, {passkeyTestOrigin, "", ""},
		} {
			got := webUICommandRequestForTest(server, method, "/tgw/api/v1/admin/"+endpoint, passwordTestBody, token, tc.origin, tc.header, tc.cookie)
			if got.Code != 403 {
				t.Fatalf("%s accepted unsafe origin/CSRF: %d", endpoint, got.Code)
			}
		}
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		return webUICommandRequestForTest(server, method, "/tgw/api/v1/admin/"+path, body, token, passkeyTestOrigin, "csrf", "csrf")
	}
	if got := call("PUT", "password", `{"username":"operator","password":"short"}`); got.Code != 400 {
		t.Fatalf("weak password accepted: %d %s", got.Code, got.Body.String())
	}
	if got := call("PUT", "password", `{"username":"operator","password":"`+strings.Repeat("x", 5000)+`"}`); got.Code != 400 {
		t.Fatalf("oversized body accepted: %d", got.Code)
	}
	if got := call("PUT", "password", passwordTestBody); got.Code != 200 {
		t.Fatalf("enable password: %d %s", got.Code, got.Body.String())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"PUT", "DELETE"} {
		got := call(method, "password", passwordTestBody)
		var problem struct{ Code string }
		if got.Code != 403 || json.Unmarshal(got.Body.Bytes(), &problem) != nil || problem.Code != "reauthentication_required" {
			t.Fatalf("stale session changed password: %d %s", got.Code, got.Body.String())
		}
	}
	// Admission rejects concurrent hashing immediately instead of allocating
	// additional Argon2 memory or queueing unauthenticated requests.
	server.passwordSlots <- struct{}{}
	server.passwordSlots <- struct{}{}
	got := call("POST", "login/password", passwordTestBody)
	<-server.passwordSlots
	<-server.passwordSlots
	if got.Code != 429 || got.Header().Get("Retry-After") != "60" {
		t.Fatalf("hash admission unbounded: %d", got.Code)
	}
}

func TestPasswordLoginRateLimitIsSeparateFromPasskeys(t *testing.T) {
	server, err := New(adminIntegrationStore(t), Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	// Invalid JSON consumes the password budget before any hashing work.
	for range 5 {
		got := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/login/password", `{`, "", passkeyTestOrigin, "csrf", "csrf")
		if got.Code != 400 {
			t.Fatalf("password attempt: %d", got.Code)
		}
	}
	got := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/login/password", `{`, "", passkeyTestOrigin, "csrf", "csrf")
	if got.Code != 429 || got.Header().Get("Retry-After") != "60" {
		t.Fatalf("password budget not enforced: %d", got.Code)
	}
	if got := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/login/begin", `{}`, "", passkeyTestOrigin, "csrf", "csrf"); got.Code != 200 {
		t.Fatalf("password attempts blocked passkey: %d", got.Code)
	}
	global, err := New(adminIntegrationStore(t), Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if got := loginLimitRequest(global, "password", fmt.Sprintf("2001:db8::%x", i+1)); got.Code != 401 {
			t.Fatalf("global attempt %d: %d", i, got.Code)
		}
	}
	if got := loginLimitRequest(global, "password", "203.0.113.3"); got.Code != 429 {
		t.Fatalf("rotating clients bypassed password budget: %d", got.Code)
	}
}
