package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWorkerPurgeRequiresFreshAuthenticationOriginCSRFAndConfirmation(t *testing.T) {
	s, store, db, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	worker, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{Name: "Revoked worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("purge-test-worker")})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RevokeWorker(t.Context(), worker.ID); err != nil {
		t.Fatal(err)
	}
	path := "/tgw/api/v1/admin/workers/" + worker.ID.String() + "/purge"
	for _, tc := range []struct {
		name, method, body, token, origin, header, cookie string
		status                                            int
	}{
		{"no login", "DELETE", `{"confirmed":true}`, "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"invalid login", "DELETE", `{"confirmed":true}`, "invalid", passkeyTestOrigin, "csrf", "csrf", 401},
		{"foreign origin", "DELETE", `{"confirmed":true}`, token, "https://evil.example", "csrf", "csrf", 403},
		{"different port", "DELETE", `{"confirmed":true}`, token, passkeyTestOrigin + ":9443", "csrf", "csrf", 403},
		{"missing origin", "DELETE", `{"confirmed":true}`, token, "", "csrf", "csrf", 403},
		{"missing csrf header", "DELETE", `{"confirmed":true}`, token, passkeyTestOrigin, "", "csrf", 403},
		{"missing csrf cookie", "DELETE", `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "", 403},
		{"csrf mismatch", "DELETE", `{"confirmed":true}`, token, passkeyTestOrigin, "wrong", "csrf", 403},
		{"empty body", "DELETE", "", token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"missing confirmation", "DELETE", `{}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"declined confirmation", "DELETE", `{"confirmed":false}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"null body", "DELETE", `null`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"unknown field", "DELETE", `{"confirmed":true,"all_workers":true}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"multiple bodies", "DELETE", `{"confirmed":true} {}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"wrong method", "POST", `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := webUICommandRequestForTest(s, tc.method, path, tc.body, tc.token, tc.origin, tc.header, tc.cookie)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	if _, err = db.Exec(`UPDATE admin_sessions SET created_at=strftime('%Y-%m-%dT%H:%M:%f','now','-6 minutes') || '000000Z'`); err != nil {
		t.Fatal(err)
	}
	w := webUICommandRequestForTest(s, http.MethodDelete, path, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauthentication_required") {
		t.Fatalf("stale authentication accepted: %d %s", w.Code, w.Body.String())
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM workers WHERE worker_id=?`, worker.ID.String()).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejected request deleted worker: count=%d err=%v", count, err)
	}
}

func TestWorkerPurgeHTTPRequiresRevocationAndRemovesOnlyConfirmedWorker(t *testing.T) {
	s, store, _, token := enrollmentHTTPServer(t, passkeyTestOrigin)
	worker, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("purge-target-token")})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{Name: "Unrelated worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("purge-other-token")})
	if err != nil {
		t.Fatal(err)
	}
	path := "/tgw/api/v1/admin/workers/" + worker.ID.String()
	request := func(target string) int {
		return webUICommandRequestForTest(s, http.MethodDelete, target, `{"confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf").Code
	}
	if status := request(path + "/purge"); status != http.StatusConflict {
		t.Fatalf("enabled worker purge status=%d", status)
	}
	if status := request(path); status != http.StatusNoContent {
		t.Fatalf("revocation status=%d", status)
	}
	workers, err := store.ListWorkers(t.Context())
	if err != nil || len(workers) != 2 {
		t.Fatalf("revocation removed records: %+v %v", workers, err)
	}
	if status := request(path + "/purge"); status != http.StatusNoContent {
		t.Fatalf("confirmed purge status=%d", status)
	}
	workers, err = store.ListWorkers(t.Context())
	if err != nil || len(workers) != 1 || workers[0].ID != other.ID || !workers[0].Enabled {
		t.Fatalf("wrong worker inventory after purge: %+v %v", workers, err)
	}
	for _, target := range []string{path + "/purge", "/tgw/api/v1/admin/workers/" + uuid.NewString() + "/purge"} {
		if status := request(target); status != http.StatusNotFound {
			t.Fatalf("missing worker status=%d", status)
		}
	}
	w := webUICommandRequestForTest(s, http.MethodGet, "/tgw/api/v1/admin/dashboard", "", token, "", "", "")
	var dashboard registry.AdminDashboard
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &dashboard) != nil || len(dashboard.Workers) != 1 || dashboard.Workers[0].ID != other.ID {
		t.Fatalf("deleted worker remains in dashboard: %d %s", w.Code, w.Body.String())
	}
}
