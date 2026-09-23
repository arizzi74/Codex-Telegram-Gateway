package admin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUISessionDeleteRequiresAuthenticationOriginCSRFAndConfirmation(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"session_id":"` + uuid.NewString() + `","request_id":"` + uuid.NewString() + `","confirmed":true}`
	for _, tc := range []struct {
		name, method, query, body, token, origin, header, cookie string
		status                                                   int
	}{
		{"missing auth", "POST", "", body, "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"invalid auth", "POST", "", body, "bad", passkeyTestOrigin, "csrf", "csrf", 401},
		{"missing origin", "POST", "", body, token, "", "csrf", "csrf", 403},
		{"wrong origin", "POST", "", body, token, "https://evil.example", "csrf", "csrf", 403},
		{"wrong port", "POST", "", body, token, passkeyTestOrigin + ":8443", "csrf", "csrf", 403},
		{"no CSRF header", "POST", "", body, token, passkeyTestOrigin, "", "csrf", 403},
		{"no CSRF cookie", "POST", "", body, token, passkeyTestOrigin, "csrf", "", 403},
		{"wrong CSRF", "POST", "", body, token, passkeyTestOrigin, "different", "csrf", 403},
		{"no confirmation", "POST", "", `{"session_id":"` + uuid.NewString() + `","request_id":"` + uuid.NewString() + `"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"arbitrary path rejected", "POST", "", body[:len(body)-1] + `,"cwd":"/private/path"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"malformed ID", "POST", "", `{"session_id":"bad","request_id":"bad","confirmed":true}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"unknown session", "POST", "", body, token, passkeyTestOrigin, "csrf", "csrf", 404},
		{"status needs login", "GET", "?session_id=" + uuid.NewString() + "&request_id=" + uuid.NewString(), "", "", "", "", "", 401},
		{"unknown status", "GET", "?session_id=" + uuid.NewString() + "&request_id=" + uuid.NewString(), "", token, "", "", "", 404},
		{"conflicting status IDs", "GET", "?session_id=" + uuid.NewString() + "&request_id=" + uuid.NewString() + "&command_id=" + uuid.NewString(), "", token, "", "", "", 400},
		{"wrong method", "DELETE", "", body, token, passkeyTestOrigin, "csrf", "csrf", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := webUICommandRequestForTest(server, tc.method, "/tgw/api/v1/webui/sessions/delete"+tc.query, tc.body, tc.token, tc.origin, tc.header, tc.cookie)
			if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d want=%d response=%s", response.Code, tc.status, response.Body.String())
			}
		})
	}
	commands, err := store.PendingCommands(t.Context(), 10)
	if err != nil || len(commands) != 0 {
		t.Fatalf("rejected requests mutated commands: %+v %v", commands, err)
	}
}

func TestWebUISessionDeleteQueuesAndReportsDurableOutcome(t *testing.T) {
	ctx := t.Context()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtimeID, sessionID, requestID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtimeID, WorkerID: worker.ID, Name: "Runtime", ProfileID: "main", State: "running", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	session := protocol.Session{ID: sessionID.String(), WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), ThreadID: "thread", CWD: "/work/kept", State: "idle", UpdatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(session)
	event := protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, SessionID: sessionID.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: raw}
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	body := `{"session_id":"` + sessionID.String() + `","request_id":"` + requestID.String() + `","confirmed":true}`
	for range 2 {
		response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/sessions/delete", body, token, passkeyTestOrigin, "csrf", "csrf")
		var queued registry.WebUISessionDeletion
		if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &queued) != nil || queued.CommandID != requestID.String() || !queued.Pending || queued.Deleted {
			t.Fatalf("queued deletion: %d %s", response.Code, response.Body.String())
		}
	}
	commands, err := store.PendingCommandsForWorker(ctx, worker.ID, 10)
	if err != nil || len(commands) != 1 || commands[0].Operation != protocol.DeleteSession {
		t.Fatalf("duplicate/missing worker command: %+v %v", commands, err)
	}
	for _, parameter := range []string{"command_id", "request_id"} {
		response := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions/delete?session_id="+sessionID.String()+"&"+parameter+"="+requestID.String(), "", token, "", "", "")
		var status registry.WebUISessionDeletion
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || !status.Pending || status.Deleted {
			t.Fatalf("read-only recovery lookup: %d %s", response.Code, response.Body.String())
		}
	}
	session.Archived, session.Deleted, session.State = true, true, "not_loaded"
	raw, _ = json.Marshal(protocol.Result{CommandID: requestID.String(), Session: &session})
	event.ID, event.Seq, event.Kind, event.Data = uuid.NewString(), 2, "command_completed", raw
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	response := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions/delete?session_id="+sessionID.String()+"&request_id="+requestID.String(), "", token, "", "", "")
	var completed registry.WebUISessionDeletion
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &completed) != nil || completed.Pending || !completed.Deleted || completed.Status != "completed" {
		t.Fatalf("completed deletion: %d %s", response.Code, response.Body.String())
	}
	if err := store.RevokeAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	response = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/sessions/delete?session_id="+sessionID.String()+"&request_id="+requestID.String(), "", token, "", "", "")
	if response.Code != 401 {
		t.Fatalf("revoked session read deletion status: %d", response.Code)
	}
}
