package admin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUICreationRequiresAuthenticationOriginCSRFAndFrozenTarget(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	target := `"worker_id":"` + uuid.NewString() + `","runtime_id":"` + uuid.NewString() + `","runtime_generation":1,"request_id":"` + uuid.NewString() + `"`
	for _, endpoint := range []string{"sessions/new", "workspaces"} {
		body := `{` + target + `}`
		if endpoint == "sessions/new" {
			body = `{` + target + `,"name":"Project","cwd":"~/CODEX/Project"}`
		}
		for _, tc := range []struct {
			name, method, body, token, origin, header, cookie string
			status                                            int
		}{
			{"no auth", "POST", body, "", passkeyTestOrigin, "csrf", "csrf", 401},
			{"bad auth", "POST", body, "bad", passkeyTestOrigin, "csrf", "csrf", 401},
			{"no origin", "POST", body, token, "", "csrf", "csrf", 403},
			{"wrong origin", "POST", body, token, "https://evil.example", "csrf", "csrf", 403},
			{"no CSRF", "POST", body, token, passkeyTestOrigin, "", "csrf", 403},
			{"wrong CSRF", "POST", body, token, passkeyTestOrigin, "wrong", "csrf", 403},
			{"unknown target", "POST", body, token, passkeyTestOrigin, "csrf", "csrf", 404},
			{"unsupported option", "POST", body[:len(body)-1] + `,"sandbox":"danger-full-access"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
			{"missing generation", "POST", strings.Replace(body, `"runtime_generation":1,`, "", 1), token, passkeyTestOrigin, "csrf", "csrf", 400},
			{"status auth", "GET", "", "", "", "", "", 401},
			{"wrong method", "DELETE", "", token, passkeyTestOrigin, "csrf", "csrf", 405},
		} {
			t.Run(endpoint+tc.name, func(t *testing.T) {
				response := webUICommandRequestForTest(server, tc.method, "/tgw/api/v1/webui/"+endpoint, tc.body, tc.token, tc.origin, tc.header, tc.cookie)
				if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
				}
			})
		}
	}
	commands, err := store.PendingCommands(t.Context(), 10)
	if err != nil || len(commands) != 0 {
		t.Fatalf("rejected requests queued work: %+v %v", commands, err)
	}
}

func TestWebUICreationInventoryAndReadOnlyRecoveryWithoutExistingSession(t *testing.T) {
	ctx := t.Context()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	redactor, _ := auth.NewRedactor([]string{"private-sentinel"}, "[REDACTED]")
	server, err := New(store, Config{Origin: passkeyTestOrigin, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	workerToken := "private-worker-token"
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "private-sentinel worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken(workerToken)})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtimeID, requestID := uuid.New(), uuid.New(), uuid.New()
	hello := protocol.Hello{WorkerID: worker.ID.String(), WorkerName: worker.Name, OS: "linux", Arch: "arm64", ProtocolMin: 1, ProtocolMax: 1, SupportsSessionWorkspaces: true, SupportsWebUISessionCreation: true, Runtimes: []protocol.Runtime{{ID: runtimeID.String(), WorkerID: worker.ID.String(), Name: "private-sentinel runtime", ProfileID: "main", Generation: 1, State: "running", DefaultCWD: "/home/private-sentinel"}}}
	if _, err := store.RegisterConnection(ctx, workerToken, connection, hello); err != nil {
		t.Fatal(err)
	}
	response := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions", "", token, "", "", "")
	var inventory struct {
		Workers  []registry.AdminWorker  `json:"workers"`
		Runtimes []protocol.Runtime      `json:"runtimes"`
		Sessions []registry.AdminSession `json:"sessions"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &inventory) != nil || len(inventory.Workers) != 1 || len(inventory.Runtimes) != 1 || len(inventory.Sessions) != 0 || !inventory.Workers[0].SupportsWebUISessionCreation || inventory.Runtimes[0].ID != runtimeID.String() {
		t.Fatalf("empty worker inventory: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private-sentinel") || strings.Contains(response.Body.String(), workerToken) || strings.Contains(response.Body.String(), "heartbeat_metadata") {
		t.Fatalf("unsafe inventory: %s", response.Body.String())
	}
	body := `{"worker_id":"` + worker.ID.String() + `","runtime_id":"` + runtimeID.String() + `","runtime_generation":1,"request_id":"` + requestID.String() + `","name":"Project","cwd":"~/CODEX/Project"}`
	for range 2 {
		response = webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/sessions/new", body, token, passkeyTestOrigin, "csrf", "csrf")
		var result registry.WebUICreationResult
		if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Pending || result.CommandID != requestID.String() {
			t.Fatalf("creation queue: %d %s", response.Code, response.Body.String())
		}
	}
	commands, err := store.PendingCommandsForWorker(ctx, worker.ID, 10)
	if err != nil || len(commands) != 1 || commands[0].Arguments.CWD != "~/CODEX/Project" || !commands[0].Arguments.EnsureWorkspace {
		t.Fatalf("created with mutable target: %+v %v", commands, err)
	}
	session := protocol.Session{ID: uuid.NewString(), WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), ThreadID: "first-created", Name: "private-sentinel session", CWD: "/home/private-sentinel/CODEX/Project", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(protocol.Result{CommandID: requestID.String(), State: "completed", Session: &session})
	event := protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: raw}
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	for _, parameter := range []string{"request_id", "command_id"} {
		response = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions/new?worker_id="+worker.ID.String()+"&"+parameter+"="+requestID.String(), "", token, "", "", "")
		var result registry.WebUICreationResult
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Pending || result.Session == nil || result.Session.ID != session.ID {
			t.Fatalf("read-only creation recovery: %d %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private-sentinel") {
			t.Fatalf("private session details leaked: %s", response.Body.String())
		}
	}
	response = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/workspaces?worker_id="+worker.ID.String()+"&request_id="+requestID.String(), "", token, "", "", "")
	if response.Code != 404 {
		t.Fatal("browse endpoint exposed creation request")
	}
	if err := store.RevokeAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	response = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions/new?worker_id="+worker.ID.String()+"&request_id="+requestID.String(), "", token, "", "", "")
	if response.Code != 401 {
		t.Fatalf("revoked login read outcome: %d", response.Code)
	}
}
