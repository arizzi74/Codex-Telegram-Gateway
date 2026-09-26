package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
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
		if response.Workers[0].RequestID != requestID {
			t.Fatalf("queue response omitted durable request identity: %+v", response.Workers[0])
		}
	}
	if requests, err := store.PendingWorkerUpdates(ctx, disabled.ID); err != nil || len(requests) != 0 {
		t.Fatalf("disabled worker was queued: %+v %v", requests, err)
	}
	for _, command := range []string{"tgstatus", "tginstances"} {
		w := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/commands?command="+command, "", token, "", "", "")
		var response webUICommandResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Instances) != 2 || len(response.Workers) != 1 || response.Workers[0].State != "pending" || response.Workers[0].RequestID != requestID {
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

func TestWebUIWorkerCodexResultRemainsSeparateAndRedacted(t *testing.T) {
	ctx := context.Background()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	redactor, _ := auth.NewRedactor([]string{"private-sentinel"}, "[REDACTED]")
	server, err := New(store, Config{Origin: passkeyTestOrigin, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", Version: "1.0.0", TokenHash: auth.HashWorkerToken("private-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	w := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/commands", `{"command":"tgupdateworkers"}`, token, passkeyTestOrigin, "csrf", "csrf")
	var queued webUICommandResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &queued) != nil || len(queued.Workers) != 1 || queued.Workers[0].RequestID == "" {
		t.Fatalf("queue response: %d %s", w.Code, w.Body.String())
	}
	result := protocol.WorkerUpdateResult{
		RequestID: queued.Workers[0].RequestID, State: "completed", Version: "1.1.0",
		Codex: &protocol.CodexUpdateReport{State: "failed", ErrorCode: "check_failed", CheckedAt: time.Now().UTC(), Profiles: []protocol.CodexRuntimeVersion{{ProfileID: "private-sentinel", InstalledVersion: "0.157.1", RunningVersion: "0.157.0", Support: "supported"}}},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	connection := uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: worker.ID.String(), Kind: "worker_update_result", OccurredAt: time.Now().UTC(), Data: raw}
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	w = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/commands?command=tgstatus", "", token, "", "", "")
	var status webUICommandResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || len(status.Workers) != 1 {
		t.Fatalf("status response: %d %s", w.Code, w.Body.String())
	}
	got := status.Workers[0]
	if got.RequestID != result.RequestID || got.State != "completed" || got.Version != "1.1.0" || got.Codex == nil || got.Codex.State != "failed" || got.Codex.ErrorCode != "check_failed" {
		t.Fatalf("Codex failure overwrote worker outcome: %+v", got)
	}
	for _, want := range []string{"updated and restarted", "version check failed", "installed 0.157.1", "running 0.157.0", "[REDACTED]"} {
		if !strings.Contains(status.Text, want) {
			t.Fatalf("missing %q in response: %s", want, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), "private-sentinel") || strings.Contains(w.Body.String(), "private-worker-token") || strings.Contains(w.Body.String(), "token_hash") {
		t.Fatalf("runtime report leaked private metadata: %s", w.Body.String())
	}
}

func TestWebUIRuntimeUpdateDoesNotClaimWorkerWasNotRestarted(t *testing.T) {
	text := webUIWorkerUpdateText(registry.WorkerUpdateStatus{State: "up_to_date", Version: "1.1.0", Codex: &protocol.CodexUpdateReport{State: "completed"}})
	if strings.Contains(text, "no worker restart") || !strings.Contains(text, "worker binary up to date") || !strings.Contains(text, "Codex runtime: updated") {
		t.Fatalf("runtime installation falsely claims no restart: %s", text)
	}
}
