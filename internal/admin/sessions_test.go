package admin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestAdminSessionMetadataInventoryAndRevocationHTTP(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, auth string) *httptest.ResponseRecorder {
		return webUICommandRequestForTest(server, method, path, body, auth, passkeyTestOrigin, "csrf", "csrf")
	}
	r := call("GET", "/tgw/api/v1/admin/session", "", token)
	var info struct {
		Authenticated bool      `json:"authenticated"`
		ID            uuid.UUID `json:"session_id"`
		OwnerID       string    `json:"owner_id"`
		ServerTime    time.Time `json:"server_time"`
		Expires       time.Time `json:"expires_at"`
		Verified      time.Time `json:"reauthenticated_at"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &info) != nil {
		t.Fatalf("session: %d %s", r.Code, r.Body.String())
	}
	if !info.Authenticated || info.ID == uuid.Nil || len(info.OwnerID) != 43 || info.ServerTime.IsZero() || info.Expires.Sub(info.Verified) != 8*time.Hour {
		t.Fatalf("incomplete metadata: %+v", info)
	}
	credential, err := store.ValidateAdminSession(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateAdminSession(t.Context(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = call("GET", "/tgw/api/v1/admin/sessions", "", token)
	var list struct {
		Sessions []registry.AdminBrowserSession `json:"sessions"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &list) != nil || len(list.Sessions) != 2 {
		t.Fatalf("inventory: %d %s", r.Code, r.Body.String())
	}
	// Mutations still require exact origin and matching CSRF cookies.
	bad := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/sessions/revoke-all", "{}", token, "https://other.example", "csrf", "csrf")
	if bad.Code != 403 {
		t.Fatal("cross-origin revoke accepted")
	}
	bad = webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/sessions/revoke-all", "{}", token, passkeyTestOrigin, "wrong", "csrf")
	if bad.Code != 403 {
		t.Fatal("missing CSRF protection")
	}
	r = call("DELETE", "/tgw/api/v1/admin/sessions/"+info.ID.String(), "", token)
	if r.Code != 204 {
		t.Fatalf("revoke current: %d %s", r.Code, r.Body.String())
	}
	if len(r.Result().Cookies()) != 2 {
		t.Fatal("current-browser revocation did not clear cookies")
	}
	if call("GET", "/tgw/api/v1/admin/session", "", token).Code != 401 {
		t.Fatal("revoked cookie accepted")
	}
	if call("GET", "/tgw/api/v1/admin/session", "", second).Code != 200 {
		t.Fatal("unselected login revoked")
	}
	r = call("POST", "/tgw/api/v1/admin/sessions/revoke-all", "{}", second)
	if r.Code != 204 || call("GET", "/tgw/api/v1/admin/session", "", second).Code != 401 {
		t.Fatal("revoke-all failed")
	}
}

func TestSensitiveAdminEndpointsRequireFreshPasskeyHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Leave the eight-hour session valid, but age its last verified login.
	if _, err = db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/tgw/api/v1/admin/passkeys/register/begin", "{}"},
		{"POST", "/tgw/api/v1/admin/passkeys/register/finish", `{"ceremony_id":"` + uuid.NewString() + `","credential":{}}`},
		{"DELETE", "/tgw/api/v1/admin/passkeys/a2V5", ""},
		{"POST", "/tgw/api/v1/admin/workers", `{"name":"new-worker"}`},
		{"POST", "/tgw/api/v1/admin/workers/" + uuid.NewString() + "/rotate-token", "{}"},
		{"DELETE", "/tgw/api/v1/admin/workers/" + uuid.NewString(), ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := webUICommandRequestForTest(server, tc.method, tc.path, tc.body, token, passkeyTestOrigin, "csrf", "csrf")
			var body struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if r.Code != 403 || json.Unmarshal(r.Body.Bytes(), &body) != nil || body.Code != "reauthentication_required" || body.Message != "Confirm with your passkey to continue." {
				t.Fatalf("gate: %d %s", r.Code, r.Body.String())
			}
		})
	}
	r := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/admin/dashboard", "", token, "", "", "")
	if r.Code != 200 {
		t.Fatalf("old but valid login cannot read: %d %s", r.Code, r.Body.String())
	}
	// A fresh verified session can perform the same operation.
	credential, err := store.ValidateAdminSession(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.CreateAdminSession(t.Context(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	r = webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/workers", `{"name":"new-worker"}`, fresh, passkeyTestOrigin, "csrf", "csrf")
	if r.Code != 201 {
		t.Fatalf("fresh login refused: %d %s", r.Code, r.Body.String())
	}
}

func TestFreshAuthenticationBoundaryAndCookieSecurity(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{0, true}, {5*time.Minute - time.Nanosecond, true}, {5 * time.Minute, false}, {6 * time.Minute, false}, {-time.Second, false}} {
		if got := freshAuthentication(now.Add(-tc.age), now); got != tc.want {
			t.Fatalf("age %s: %t", tc.age, got)
		}
	}
	server := &Server{}
	response := httptest.NewRecorder()
	server.setSession(response, "test-token")
	for _, c := range response.Result().Cookies() {
		if !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Domain != "" || c.Path != "/" || c.MaxAge != 0 || !c.Expires.IsZero() {
			t.Fatalf("insecure cookie attributes: %+v", c)
		}
		if c.Name == adminCookie && (!c.HttpOnly || c.Value != "test-token") {
			t.Fatal("session cookie not HttpOnly")
		}
	}
	if len(response.Result().Cookies()) != 2 {
		t.Fatal("missing auth/CSRF cookies")
	}
}

// An atomic revocation failure must leave cookies usable for a retry rather
// than claiming sign-out while the server still accepts the bearer session.
func TestAdminLogoutDoesNotClaimSuccessWhenRevocationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	push := webPushTestSubscription(t)
	if _, err = store.UpsertWebPushSubscription(t.Context(), token, registry.WebPushSubscription{Endpoint: push.Endpoint, P256DH: push.Keys.P256DH, Auth: push.Keys.Auth}); err != nil {
		t.Fatal(err)
	}
	draftID := uuid.New()
	if _, err = store.PutAdminDraft(t.Context(), token, draftID, 1, base64.RawURLEncoding.EncodeToString(make([]byte, 40))); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER test_revoke_failure BEFORE UPDATE OF revoked_at ON admin_sessions BEGIN SELECT RAISE(FAIL,'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/logout", "{}", token, passkeyTestOrigin, "csrf", "csrf")
	if response.Code != 500 || len(response.Result().Cookies()) != 0 {
		t.Fatalf("failed revocation claimed signout: %d %+v", response.Code, response.Result().Cookies())
	}
	if _, err = store.ValidateAdminSession(t.Context(), token); err != nil {
		t.Fatal("failed revocation mutated session")
	}
	if _, err = store.GetAdminDraft(t.Context(), token, draftID); err != nil {
		t.Fatal("failed revocation destroyed draft")
	}
	if _, err = db.Exec(`DROP TRIGGER test_revoke_failure`); err != nil {
		t.Fatal(err)
	}
	response = webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/logout", "{}", token, passkeyTestOrigin, "csrf", "csrf")
	if response.Code != 204 || len(response.Result().Cookies()) != 2 {
		t.Fatalf("successful revoke: %d", response.Code)
	}
	var pushes, drafts int
	if err = db.QueryRow(`SELECT count(*) FROM webpush_subscriptions`).Scan(&pushes); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM admin_drafts WHERE ciphertext IS NOT NULL OR deleted=0`).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if pushes != 0 || drafts != 0 {
		t.Fatalf("logout retained capabilities: pushes=%d drafts=%d", pushes, drafts)
	}
}
