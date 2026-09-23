package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIActivityRequiresPasskeyAndExactOrigin(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, origin, token string
		code                  int
	}{
		{"GET", passkeyTestOrigin, "", 401}, {"GET", passkeyTestOrigin, "bad", 401},
		{"GET", "", token, 403}, {"GET", "https://evil.example", token, 403}, {"GET", passkeyTestOrigin + ":8443", token, 403}, {"POST", passkeyTestOrigin, token, 405},
	} {
		r := httptest.NewRequest(tc.method, "/tgw/api/v1/webui/activity", nil)
		r.Header.Set("Origin", tc.origin)
		if tc.token != "" {
			r.AddCookie(cookie(adminCookie, tc.token, true))
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != tc.code || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("activity guard: status=%d want=%d", w.Code, tc.code)
		}
	}
	r := httptest.NewRequest("GET", "/tgw/api/v1/webui/activity?v=3", nil)
	r.Header.Set("Origin", passkeyTestOrigin)
	r.AddCookie(cookie(adminCookie, token, true))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported activity version status=%d", w.Code)
	}
}

func TestWebUIActivityKeepsLegacyOpenTabsCompatible(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	console, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(console)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/activity", &websocket.DialOptions{HTTPClient: passkeyHTTPClient(t, server), HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}, "Cookie": []string{adminCookie + "=" + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read := func() registry.SessionActivitySnapshot {
		t.Helper()
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame struct {
			Type string `json:"type"`
			registry.SessionActivitySnapshot
		}
		if err := json.Unmarshal(raw, &frame); err != nil || frame.Type != "activity" || frame.Sessions == nil || strings.Contains(string(raw), `"sequence"`) {
			t.Fatalf("legacy activity frame: %s (%v)", raw, err)
		}
		return frame.SessionActivitySnapshot
	}
	initial := read()
	if len(initial.Sessions) != 0 {
		t.Fatalf("unexpected initial sessions: %+v", initial)
	}
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtimeID, sessionID := uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtimeID, WorkerID: worker.ID, Name: "Runtime", ProfileID: "main", State: "running", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(protocol.Session{ID: sessionID.String(), WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), ThreadID: "thread", State: "idle", UpdatedAt: time.Now().UTC()})
	if err := store.IngestEvent(ctx, worker.ID, connection, protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, SessionID: sessionID.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: raw}); err != nil {
		t.Fatal(err)
	}
	// Connectivity can produce an earlier empty snapshot; eventually the new
	// session still arrives in the original full-snapshot wire format.
	for {
		latest := read()
		if len(latest.Sessions) == 1 {
			if latest.Revision <= initial.Revision || latest.Sessions[0].SessionID != sessionID.String() {
				t.Fatalf("legacy update: %+v", latest)
			}
			break
		}
	}
}

func TestWebUIActivityStreamsOtherSessionsAndRevalidatesAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtimeID, sessionID := uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtimeID, WorkerID: worker.ID, Name: "Runtime", ProfileID: "main", State: "running", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	emit := func(kind string, value any) {
		t.Helper()
		seq++
		raw, _ := json.Marshal(value)
		if err := store.IngestEvent(ctx, worker.ID, connection, protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, SessionID: sessionID.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	emit("session_discovered", protocol.Session{ID: sessionID.String(), WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), ThreadID: "thread", Name: "PRIVATE SESSION", CWD: "/private/work", State: "idle", UpdatedAt: time.Now().UTC()})
	console, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(console)
	defer server.Close()
	client := passkeyHTTPClient(t, server)
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/activity?v=1", &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}, "Cookie": []string{adminCookie + "=" + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	type activityFrame struct {
		Type      string                     `json:"type"`
		Version   int                        `json:"version"`
		Sequence  uint64                     `json:"sequence"`
		Revision  uint64                     `json:"revision"`
		Event     string                     `json:"event"`
		SessionID string                     `json:"session_id"`
		Session   *registry.SessionActivity  `json:"session"`
		Sessions  []registry.SessionActivity `json:"sessions"`
	}
	wantSequence := uint64(0)
	lastRevision := uint64(0)
	read := func(kind, event string) activityFrame {
		t.Helper()
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var frame activityFrame
		if json.Unmarshal(raw, &frame) != nil || frame.Type != kind || frame.Event != event || frame.Version != 1 || frame.Sequence != wantSequence || frame.Revision < lastRevision {
			t.Fatalf("activity frame: %s", raw)
		}
		wantSequence++
		lastRevision = frame.Revision
		if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "/private") || strings.Contains(string(raw), "token") {
			t.Fatalf("activity leaked details: %s", raw)
		}
		if kind == "activity_event" && frame.SessionID != sessionID.String() {
			t.Fatalf("incorrect event scope: %+v", frame)
		}
		return frame
	}
	initial := read("activity_snapshot", "")
	if len(initial.Sessions) != 1 || initial.Sessions[0].State != "idle" {
		t.Fatalf("initial state: %+v", initial)
	}
	emit("turn_started", protocol.Result{TurnID: "turn", State: "running"})
	running := read("activity_event", "turn_started")
	if running.Revision <= initial.Revision || running.Session == nil || running.Session.ActiveTurnID != "turn" {
		t.Fatalf("running indicator: %+v", running)
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async-question", ThreadID: "thread", TurnID: "turn", Type: "input", Async: true, Questions: []protocol.Question{{ID: "answer", Prompt: "PRIVATE question text"}}}
	emit("user_input_requested", approval)
	question := read("activity_event", "question_requested")
	if question.Session == nil || question.Session.PendingQuestions != 1 || question.Session.PendingRevision == "" {
		t.Fatalf("pending question: %+v", question)
	}
	// Retain both transitions even if their wakeups coalesce, including a
	// replacement request that leaves the final pending count unchanged.
	approval.State = "approved"
	emit("approval_resolved", approval)
	approval.ID, approval.RequestID, approval.State = uuid.NewString(), "replacement-question", "pending"
	emit("user_input_requested", approval)
	resolved := read("activity_event", "question_resolved")
	if resolved.Session == nil || resolved.Session.PendingQuestions != 0 {
		t.Fatalf("resolution was hidden: %+v", resolved)
	}
	replacement := read("activity_event", "question_requested")
	if replacement.Session == nil || replacement.Session.PendingQuestions != 1 || replacement.Session.PendingRevision == question.Session.PendingRevision {
		t.Fatalf("question replacement was hidden: %+v", replacement)
	}
	approval.State = "approved"
	emit("approval_resolved", approval)
	answered := read("activity_event", "question_resolved")
	if answered.Session == nil || answered.Session.PendingQuestions != 0 || answered.Session.PendingRevision != "" || answered.Session.ActiveTurnID != "turn" {
		t.Fatalf("answer left stale indicator: %+v", answered)
	}
	emit("turn_completed", protocol.Result{TurnID: "turn", State: "idle"})
	ended := read("activity_event", "turn_ended")
	if ended.Session == nil || ended.Session.ActiveTurnID != "" || ended.Session.State != "idle" {
		t.Fatalf("turn end left stale indicator: %+v", ended)
	}
	if err := store.RevokeAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	emit("turn_started", protocol.Result{TurnID: "next-turn", State: "running"})
	_, raw, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(raw), `"type":"error"`) || strings.Contains(string(raw), "sessions") {
		t.Fatalf("revocation reply: %s %v", raw, err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != 4001 {
		t.Fatalf("revoked feed close=%v", err)
	}
}
