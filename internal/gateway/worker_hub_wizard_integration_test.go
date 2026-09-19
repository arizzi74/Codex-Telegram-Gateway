package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Exercise the version-skew failure through the real wizard, command ledger,
// dispatcher, websocket hub and acknowledgement recovery. Only the old worker
// is a wire fixture; an unsupported command must never reach its connection.
func TestOldWorkerDeletionRejectionEndsWizardAndAllowsNextPromptIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	probe, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	workerID, runtimeID, sessionID := uuid.New(), uuid.New(), uuid.New()
	token, err := auth.GenerateWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "old-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(store, logger, time.Second, 20*time.Second)
	hub.AckHandler = store.AcknowledgeCommand
	hub.EventHandler = store.IngestEvent
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: workerID.String(), WorkerName: "old-worker", OS: "linux", Arch: "amd64", WorkerVersion: "0.5.17", ProtocolMin: 1, ProtocolMax: 1,
		Runtimes: []protocol.Runtime{{ID: runtimeID.String(), WorkerID: workerID.String(), ProfileID: "main", Name: "Main", Generation: 1, State: "running", DefaultCWD: "/work"}}})
	if frame, err := readEnvelope(ctx, conn); err != nil || frame.Type != "hello_ack" {
		t.Fatalf("hello acknowledgement: %s %v", frame.Type, err)
	}
	session := protocol.Session{ID: sessionID.String(), WorkerID: workerID.String(), RuntimeID: runtimeID.String(), ThreadID: "saved-thread", Name: "Project", CWD: "/work/Project", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	sendFrame(t, ctx, conn, "worker_event", protocol.Event{Seq: 1, ID: uuid.NewString(), WorkerID: workerID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, SessionID: sessionID.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: data})
	if frame, err := readEnvelope(ctx, conn); err != nil || frame.Type != "event_ack" {
		t.Fatalf("session discovery acknowledgement: %s %v", frame.Type, err)
	}
	var updateID int64
	accept := func(in registry.IncomingUpdate) registry.AcceptResult {
		t.Helper()
		updateID++
		in.BotID, in.UserID, in.ChatID, in.UpdateID = "bot", 7, 9, updateID
		result, err := store.AcceptTelegram(ctx, in)
		if err != nil || result.ErrorCode != "" {
			t.Fatalf("accept Telegram: %#v %v", result, err)
		}
		return result
	}
	click := func(result registry.AcceptResult, action string) registry.AcceptResult {
		t.Helper()
		token, err := store.CreateCallback(ctx, registry.Callback{Action: action, BotID: "bot", UserID: 7, ChatID: 9,
			RuntimeID: runtimeID, SessionID: sessionID, Generation: 1, WizardID: result.WizardID, WizardRevision: result.WizardRevision, ExpiresAt: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		return accept(registry.IncomingUpdate{CallbackToken: token})
	}
	accept(registry.IncomingUpdate{Action: "select", Target: sessionID.String()})
	picker := accept(registry.IncomingUpdate{Action: "delete_session", Target: runtimeID.String()})
	if picker.View != "delete_sessions" {
		t.Fatalf("delete picker: %#v", picker)
	}
	confirmation := click(picker, "delete_session_pick")
	if confirmation.View != "delete_session_confirm" {
		t.Fatalf("delete confirmation: %#v", confirmation)
	}
	deleting := click(confirmation, "delete_session_confirm")
	if deleting.View != "session_deleting" || deleting.CommandID == "" {
		t.Fatalf("delete command: %#v", deleting)
	}
	dispatcher := NewDispatcher(store, hub, logger)
	if err := dispatcher.dispatchWorker(ctx, workerID); err != nil {
		t.Fatal(err)
	}
	var status, code, phase, wizardCommandID string
	if err := probe.QueryRowContext(ctx, `SELECT status, error_code FROM commands WHERE command_id=?`, deleting.CommandID).Scan(&status, &code); err != nil || status != "failed" || code != protocol.UnsupportedOperation {
		t.Fatalf("delete command outcome: status=%q code=%q error=%v", status, code, err)
	}
	if err := probe.QueryRowContext(ctx, `SELECT phase,COALESCE(command_id,'') FROM telegram_session_wizards WHERE bot_id='bot' AND user_id=7 AND chat_id=9`).Scan(&phase, &wizardCommandID); err != nil || phase != "done" || wizardCommandID != "" {
		t.Fatalf("wizard retained input lock: phase=%q command=%q error=%v", phase, wizardCommandID, err)
	}
	var responseJSON []byte
	if err := probe.QueryRowContext(ctx, `SELECT payload FROM telegram_deliveries WHERE kind='ui_response' ORDER BY rowid DESC LIMIT 1`).Scan(&responseJSON); err != nil {
		t.Fatal(err)
	}
	var response registry.AcceptResult
	if err := json.Unmarshal(responseJSON, &response); err != nil || response.View != "error" || response.ErrorCode != protocol.UnsupportedOperation {
		t.Fatalf("durable rejection reply: %#v %v", response, err)
	}
	sender := NewSender(store, nil, logger, SenderOptions{BotID: "bot", OwnerID: 7})
	text, _, err := sender.render(ctx, registry.Delivery{Kind: "ui_response", ChatID: 9, Payload: responseJSON})
	if err != nil || !strings.Contains(strings.ToLower(text), "update") {
		t.Fatalf("rejection omitted actionable update guidance: %q %v", text, err)
	}
	// The very next ordinary message routes normally, and the next websocket
	// frame is that prompt: no unsupported delete was sent or left for retry.
	prompt := accept(registry.IncomingUpdate{Text: "continue the conversation"})
	if prompt.CommandID == "" || prompt.SessionID != sessionID.String() {
		t.Fatalf("next prompt remained blocked by wizard: %#v", prompt)
	}
	if err := dispatcher.dispatchWorker(ctx, workerID); err != nil {
		t.Fatal(err)
	}
	frame, err := readEnvelope(ctx, conn)
	if err != nil || frame.Type != "command" {
		t.Fatalf("old-worker connection did not remain usable: %s %v", frame.Type, err)
	}
	command, err := protocol.Payload[protocol.Command](frame)
	if err != nil || command.ID != prompt.CommandID || command.Operation != protocol.StartTurn || command.Arguments.Text != "continue the conversation" {
		t.Fatalf("unexpected command reached old worker: %#v %v", command, err)
	}
	sessions, err := store.SessionSnapshot(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != sessionID.String() || sessions[0].Deleted || sessions[0].Archived {
		t.Fatalf("rejected deletion changed saved session: %#v %v", sessions, err)
	}
}
