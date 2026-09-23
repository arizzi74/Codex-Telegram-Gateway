package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
)

func webUICommandRequestForTest(server *Server, method, path, body, token, origin, headerCSRF, cookieCSRF string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.AddCookie(cookie(adminCookie, token, true))
	}
	if headerCSRF != "" {
		r.Header.Set("X-CSRF-Token", headerCSRF)
	}
	if cookieCSRF != "" {
		r.AddCookie(cookie(csrfCookie, cookieCSRF, false))
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	return w
}

func TestWebUICommandsRequirePasskeyExactOriginAndCSRF(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateWorker(context.Background(), registry.CreateWorkerInput{Name: "Unchanged worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, path, body, token, origin, headerCSRF, cookieCSRF string
		code                                                            int
	}{
		{"missing passkey", "POST", "", `{"command":"tgupdateworkers"}`, "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"invalid passkey", "POST", "", `{"command":"tgupdateworkers"}`, "bad", passkeyTestOrigin, "csrf", "csrf", 401},
		{"missing origin", "POST", "", `{"command":"tgupdateworkers"}`, token, "", "csrf", "csrf", 403},
		{"wrong origin", "POST", "", `{"command":"tgupdateworkers"}`, token, "https://evil.example", "csrf", "csrf", 403},
		{"different port", "POST", "", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin + ":8443", "csrf", "csrf", 403},
		{"missing CSRF header", "POST", "", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin, "", "csrf", 403},
		{"missing CSRF cookie", "POST", "", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin, "csrf", "", 403},
		{"CSRF mismatch", "POST", "", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin, "wrong", "csrf", 403},
		{"unknown command", "POST", "", `{"command":"shell"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"extra target", "POST", "", `{"command":"tgupdateworkers","target":"all"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"inline arguments", "POST", "", `{"command":"tgupdateworkers --force"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"invalid context", "POST", "", `{"command":"tgupdateworkers","session_id":"bad"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"get cannot mutate", "GET", "?command=tgupdateworkers", "", token, "", "", "", 405},
		{"discovery requires login", "GET", "", "", "", "", "", "", 401},
		{"status requires login", "GET", "?command=tgstatus", "", "bad", "", "", "", 401},
		{"wrong method", "PUT", "", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin, "csrf", "csrf", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := webUICommandRequestForTest(server, tc.method, "/tgw/api/v1/webui/commands"+tc.path, tc.body, tc.token, tc.origin, tc.headerCSRF, tc.cookieCSRF)
			if w.Code != tc.code {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("command response lost security headers")
			}
		})
	}
	requests, err := store.PendingWorkerUpdates(context.Background(), worker.ID)
	if err != nil || len(requests) != 0 {
		t.Fatalf("rejected commands mutated queue: %+v %v", requests, err)
	}
}

func TestWebUICommandsUpdateOfflineWorkersAndExposeRedactedStatus(t *testing.T) {
	ctx := context.Background()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	redactor, _ := auth.NewRedactor([]string{"private-sentinel"}, "[REDACTED]")
	server, err := New(store, Config{Origin: passkeyTestOrigin, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "private-sentinel worker", OS: "linux", Arch: "arm64", Version: "1.0.0", TokenHash: auth.HashWorkerToken("private-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Disabled", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("disabled-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeWorker(ctx, disabled.ID); err != nil {
		t.Fatal(err)
	}
	var requestID string
	for _, expected := range []string{"queued", "already_queued"} {
		w := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/commands", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin, "csrf", "csrf")
		var response webUICommandResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("response=%d %s", w.Code, w.Body.String())
		}
		if len(response.Workers) != 1 || response.Workers[0].WorkerID != worker.ID.String() || response.Workers[0].State != expected || !strings.Contains(response.Text, "idle") {
			t.Fatalf("queue response: %+v", response)
		}
		if strings.Contains(w.Body.String(), "private-sentinel") || !strings.Contains(response.Workers[0].Name, "[REDACTED]") || strings.Contains(w.Body.String(), "TokenHash") {
			t.Fatalf("unredacted or unsafe projection: %s", w.Body.String())
		}
		requests, err := store.PendingWorkerUpdates(ctx, worker.ID)
		if err != nil || len(requests) != 1 || (requestID != "" && requests[0].RequestID != requestID) {
			t.Fatalf("durable request = %+v %v", requests, err)
		}
		requestID = requests[0].RequestID
	}
	if requests, err := store.PendingWorkerUpdates(ctx, disabled.ID); err != nil || len(requests) != 0 {
		t.Fatalf("disabled worker was queued: %+v %v", requests, err)
	}
	for _, command := range []string{"tgstatus", "tginstances"} {
		w := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/commands?command="+command, "", token, "", "", "")
		var response webUICommandResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Instances) != 2 || len(response.Workers) != 1 || response.Workers[0].State != "pending" {
			t.Fatalf("status response: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-sentinel") || strings.Contains(w.Body.String(), "token_hash") || strings.Contains(w.Body.String(), "private-worker-token") {
			t.Fatalf("status leaked private fields: %s", w.Body.String())
		}
	}
	w := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/commands", "", token, "", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"tgupdateworkers"`) {
		t.Fatalf("discovery: %d %s", w.Code, w.Body.String())
	}
	w = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/commands?command=tgstatus&session_id="+uuid.NewString(), "", token, "", "", "")
	if w.Code != 404 {
		t.Fatalf("unknown selected session=%d", w.Code)
	}
	if err := store.FailWorkerUpdate(ctx, worker.ID, uuid.MustParse(requestID), "unsupported_worker"); err != nil {
		t.Fatal(err)
	}
	w = webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/commands", `{"command":"tgstatus"}`, token, passkeyTestOrigin, "csrf", "csrf")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "codex-telegramgw update worker") {
		t.Fatalf("failed worker remediation not shown: %d %s", w.Code, w.Body.String())
	}
}
