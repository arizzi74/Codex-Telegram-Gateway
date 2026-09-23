package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func webuiTestLogin(t *testing.T, store *registry.Store) string {
	t.Helper()
	ctx := context.Background()
	bootstrap, err := store.BootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	handle := []byte("webui-test-user-handle-32-bytes!!!")
	ceremony, err := store.NewAdminCeremony(ctx, "registration", handle, []byte(`{"challenge":"dGVzdC13ZWJ1aS1jaGFsbGVuZ2U"}`))
	if err != nil {
		t.Fatal(err)
	}
	cred := registry.AdminCredential{ID: []byte("webui-test-passkey"), UserHandle: handle, CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)}
	if err := store.CompleteBootstrapCredential(ctx, ceremony.ID, ceremony.Binding, bootstrap, cred); err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateAdminSession(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestWebUIRequiresPasskeyAndExactOrigin(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	s, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, origin, token string
		code                int
	}{
		{"/tgw/api/v1/webui/sessions", "", "", 401},
		{"/tgw/api/v1/webui/sessions", "", "bad-cookie", 401},
		{"/tgw/api/v1/webui/sessions", "", token, 200},
		{"/tgw/api/v1/webui/connect", "", token, 403},
		{"/tgw/api/v1/webui/connect", "https://evil.example", token, 403},
		{"/tgw/api/v1/webui/connect", "http://gateway.example.com", token, 403},
		{"/tgw/api/v1/webui/connect", passkeyTestOrigin, "", 401},
		{"/tgw/api/v1/webui/connect", passkeyTestOrigin, token, 503},
		{"/webui/", "", token, 404},
		{"/tgui/", "", token, 404},
	} {
		t.Run(tc.path+tc.origin+tc.token[:min(3, len(tc.token))], func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Header.Set("Origin", tc.origin)
			if tc.token != "" {
				r.AddCookie(cookie(adminCookie, tc.token, true))
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("status=%d want=%d", w.Code, tc.code)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("lost browser security headers")
			}
		})
	}
}

func TestWebUIStreamRoundTripDisconnectAndRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := adminIntegrationStore(t)
	adminToken := webuiTestLogin(t, store)
	workerToken, _ := auth.GenerateWorkerToken()
	workerID, runtimeID, sessionID := uuid.New(), uuid.New(), uuid.New()
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "Test worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(workerToken)}); err != nil {
		t.Fatal(err)
	}
	hub := gateway.NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 30*time.Second)
	hub.EventHandler = store.IngestEvent
	redactor, _ := auth.NewRedactor([]string{"secret-sentinel"}, "[REDACTED]")
	server := httptest.NewUnstartedServer(nil)
	origin := passkeyTestOrigin
	console, err := New(store, Config{Origin: origin, WebUI: hub, Redactor: redactor})
	if err != nil {
		t.Fatal(err)
	}
	mux := gateway.NewMux(store, hub)
	mux.Handle("/tgw/api/v1/webui/", console)
	server.Config.Handler = mux
	server.StartTLS()
	defer server.Close()
	client := passkeyHTTPClient(t, server)
	base := "wss" + strings.TrimPrefix(origin, "https")
	worker, _, err := websocket.Dial(ctx, base+"/tgw/api/v1/workers/connect", &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + workerToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.CloseNow()
	workerSend := func(kind string, payload any) {
		t.Helper()
		e, err := protocol.NewEnvelope(kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(e)
		if err := worker.Write(ctx, websocket.MessageText, body); err != nil {
			t.Fatal(err)
		}
	}
	workerRead := func(want string) protocol.Envelope {
		t.Helper()
		_, body, err := worker.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e, err := protocol.Decode(body)
		if err != nil || e.Type != want {
			t.Fatalf("worker message %s want %s: %v", e.Type, want, err)
		}
		return e
	}
	workerSend("hello", protocol.Hello{WorkerID: workerID.String(), WorkerName: "Test", OS: "linux", Arch: "amd64", ProtocolMin: 1, ProtocolMax: 1, SupportsWebUI: true,
		Runtimes: []protocol.Runtime{{ID: runtimeID.String(), WorkerID: workerID.String(), ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}})
	workerRead("hello_ack")
	session := protocol.Session{ID: sessionID.String(), WorkerID: workerID.String(), RuntimeID: runtimeID.String(), ThreadID: "thread-test", Name: "Test session", CWD: "/work", State: "idle", UpdatedAt: time.Now().UTC()}
	body, _ := json.Marshal(session)
	workerSend("worker_event", protocol.Event{Seq: 1, ID: uuid.NewString(), WorkerID: workerID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, SessionID: sessionID.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: body})
	workerRead("event_ack")
	dialBrowser := func() (*websocket.Conn, string) {
		t.Helper()
		browser, _, err := websocket.Dial(ctx, base+"/tgw/api/v1/webui/connect?session_id="+sessionID.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{origin}, "Cookie": []string{adminCookie + "=" + adminToken}}})
		if err != nil {
			t.Fatal(err)
		}
		e := workerRead("webui")
		frame, err := protocol.Payload[protocol.WebUIFrame](e)
		if err != nil || frame.Action != "open" || frame.SessionID != sessionID.String() || frame.RuntimeGeneration != 1 {
			t.Fatalf("bad open: %+v %v", frame, err)
		}
		workerSend("webui", protocol.WebUIFrame{ID: frame.ID, Action: "ready"})
		_, body, err := browser.Read(ctx)
		if err != nil || !strings.Contains(string(body), `"type":"ready"`) {
			t.Fatalf("browser not ready: %s %v", body, err)
		}
		return browser, frame.ID
	}
	browser, id := dialBrowser()
	defer browser.CloseNow()
	input := `{"id":9007199254740993,"method":"thread/read","params":{"includeTurns":false}}`
	if err := browser.Write(ctx, websocket.MessageText, []byte(input)); err != nil {
		t.Fatal(err)
	}
	frame, _ := protocol.Payload[protocol.WebUIFrame](workerRead("webui"))
	if frame.Action != "input" || string(frame.Data) != input {
		t.Fatalf("input changed: %+v", frame)
	}
	workerSend("webui", protocol.WebUIFrame{ID: id, Action: "output", Data: json.RawMessage(`{"id":9007199254740993,"result":{"text":"secret-sentinel"}}`)})
	_, response, err := browser.Read(ctx)
	if err != nil || strings.Contains(string(response), "secret-sentinel") || !strings.Contains(string(response), "9007199254740993") || !strings.Contains(string(response), "[REDACTED]") {
		t.Fatalf("unsafe/changed response: %s %v", response, err)
	}
	browser.CloseNow()
	frame, _ = protocol.Payload[protocol.WebUIFrame](workerRead("webui"))
	if frame.Action != "close" || frame.ID != id {
		t.Fatalf("browser must detach only: %+v", frame)
	}
	// Reconnect creates a fresh relay and cannot replay the previous request.
	browser, id = dialBrowser()
	defer browser.CloseNow()
	if err := store.RevokeAdminSession(ctx, adminToken); err != nil {
		t.Fatal(err)
	}
	if err := browser.Write(ctx, websocket.MessageText, []byte(`{"id":2,"method":"turn/start","params":{"input":[{"type":"text","text":"must not run"}]}}`)); err != nil {
		t.Fatal(err)
	}
	_, response, err = browser.Read(ctx)
	if err != nil || !strings.Contains(string(response), "session expired") {
		t.Fatalf("revocation message: %s %v", response, err)
	}
	// Consume the close handshake so cleanup need not wait for the close timeout.
	_, _, _ = browser.Read(ctx)
	frame, _ = protocol.Payload[protocol.WebUIFrame](workerRead("webui"))
	if frame.Action != "close" || frame.ID != id {
		t.Fatalf("revoked browser input reached worker: %+v", frame)
	}
	// An older worker's upgrade advice must reach the browser over the socket;
	// browser WebSocket APIs hide HTTP error bodies from failed upgrades.
	worker.CloseNow()
	worker, _, err = websocket.Dial(ctx, base+"/tgw/api/v1/workers/connect", &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + workerToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.CloseNow()
	workerSend("hello", protocol.Hello{WorkerID: workerID.String(), WorkerName: "Old worker", OS: "linux", Arch: "amd64", ProtocolMin: 1, ProtocolMax: 1,
		Runtimes: []protocol.Runtime{{ID: runtimeID.String(), WorkerID: workerID.String(), ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}})
	workerRead("hello_ack")
	adminToken, err = store.CreateAdminSession(ctx, []byte("webui-test-passkey"))
	if err != nil {
		t.Fatal(err)
	}
	browser, _, err = websocket.Dial(ctx, base+"/tgw/api/v1/webui/connect?session_id="+sessionID.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{origin}, "Cookie": []string{adminCookie + "=" + adminToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.CloseNow()
	_, response, err = browser.Read(ctx)
	if err != nil || !strings.Contains(string(response), "update this worker") {
		t.Fatalf("worker upgrade advice missing: %s %v", response, err)
	}
	_, _, err = browser.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("old worker should require explicit update, not automatic retries: %v", err)
	}
}
