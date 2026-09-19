package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

var (
	testWorkerID  = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	testRuntimeID = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	testSessionID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	testOtherRun  = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	testOtherSess = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	testApproval  = uuid.MustParse("66666666-6666-4666-8666-666666666666")
)

type renderStoreFake struct {
	workers     []registry.Worker
	runtimes    []protocol.Runtime
	sessions    []protocol.Session
	status      registry.SessionStatus
	approval    protocol.Approval
	callbacks   []registry.Callback
	workerErr   error
	runtimeErr  error
	sessionErr  error
	statusErr   error
	approvalErr error
	callbackErr error
}

func (f *renderStoreFake) ClaimDeliveries(context.Context, int) ([]registry.Delivery, error) {
	return nil, nil
}
func (f *renderStoreFake) DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error) {
	return nil, nil
}
func (f *renderStoreFake) ExtendDelivery(context.Context, string) error { return nil }
func (f *renderStoreFake) PrepareDeliveryChunks(context.Context, string, []json.RawMessage) ([]registry.DeliveryChunk, error) {
	return nil, nil
}
func (f *renderStoreFake) MarkDeliveryChunkSent(context.Context, string, int, int64, string, string, string, ...string) error {
	return nil
}
func (f *renderStoreFake) RetryDelivery(context.Context, string, time.Duration, string) error {
	return nil
}
func (f *renderStoreFake) SessionSnapshot(context.Context) ([]protocol.Session, error) {
	return f.sessions, f.sessionErr
}
func (f *renderStoreFake) RuntimeSnapshot(context.Context) ([]protocol.Runtime, error) {
	return f.runtimes, f.runtimeErr
}
func (f *renderStoreFake) ListWorkers(context.Context) ([]registry.Worker, error) {
	return f.workers, f.workerErr
}
func (f *renderStoreFake) TelegramSessionStatus(context.Context, uuid.UUID) (registry.SessionStatus, error) {
	return f.status, f.statusErr
}
func (f *renderStoreFake) PendingApproval(context.Context, uuid.UUID) (protocol.Approval, error) {
	return f.approval, f.approvalErr
}
func (f *renderStoreFake) CreateCallback(_ context.Context, callback registry.Callback) (string, error) {
	if f.callbackErr != nil {
		return "", f.callbackErr
	}
	f.callbacks = append(f.callbacks, callback)
	return "cb_opaque_" + string(rune('a'+len(f.callbacks)-1)), nil
}

func renderFixture() *renderStoreFake {
	lastEvent := time.Date(2026, 9, 13, 10, 11, 12, 0, time.UTC)
	return &renderStoreFake{
		workers: []registry.Worker{{ID: testWorkerID, Name: "MacBook", Connectivity: "online"}},
		runtimes: []protocol.Runtime{
			{ID: testRuntimeID.String(), WorkerID: testWorkerID.String(), Name: "Primary", Generation: 7, State: "running"},
			{ID: testOtherRun.String(), WorkerID: testWorkerID.String(), Name: "Secondary", Generation: 2, State: "stopped"},
		},
		sessions: []protocol.Session{
			{ID: testSessionID.String(), WorkerID: testWorkerID.String(), RuntimeID: testRuntimeID.String(), ThreadID: "thread-abcdefghijk", Name: "auth-fix", CWD: "/work/api", GitBranch: "fix/auth", State: "running", ActiveTurnID: "turn-abcdefghijk", Loaded: true},
			{ID: testOtherSess.String(), WorkerID: testWorkerID.String(), RuntimeID: testOtherRun.String(), ThreadID: "thread-other", Name: "other-session", CWD: "/work/other", State: "idle"},
		},
		status: registry.SessionStatus{SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String(), PendingApprovals: 2, QueuedCommands: 3, LastEventAt: &lastEvent},
	}
}

func TestRenderResolvedInputDoesNotRetryObsoletePrompt(t *testing.T) {
	store := renderFixture()
	store.approvalErr = registry.ErrTelegramTarget
	payload, _ := json.Marshal(registry.AcceptResult{View: "input_pending", ApprovalID: testApproval.String(), QuestionID: "q2"})
	text, keyboard, err := testSender(store, nil).render(context.Background(), registry.Delivery{Kind: "ui_response", Payload: payload})
	if err != nil || keyboard != nil || !strings.Contains(text, "no longer pending") {
		t.Fatalf("obsolete prompt: %q %v %v", text, keyboard, err)
	}
}

func testSender(store *renderStoreFake, redactor *auth.Redactor) *Sender {
	return NewSender(store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
}

func TestCodexCommandOutputAndForkReplyRouting(t *testing.T) {
	store := renderFixture()
	row := eventRow(t, "command_completed", protocol.Result{Text: "Model: sample\nContext: 123 tokens"}, testSessionID.String())
	text, _, err := testSender(store, nil).render(context.Background(), row)
	if err != nil || !strings.Contains(text, "Model: sample\nContext: 123 tokens") || strings.Contains(text, "Command completed.") {
		t.Fatalf("Codex output: %q %v", text, err)
	}
	row = eventRow(t, "command_completed", protocol.Result{Session: &store.sessions[1]}, testSessionID.String())
	session, turn, approval := deliveryRoute(row)
	if session != testOtherSess.String() || turn != "" || approval != "" {
		t.Fatalf("fork response routed to source: %s %s %s", session, turn, approval)
	}
}

func uiRow(t *testing.T, response registry.AcceptResult) registry.Delivery {
	t.Helper()
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return registry.Delivery{ChatID: 99, TopicID: 4, Kind: "ui_response", Payload: payload}
}

func eventRow(t *testing.T, kind string, data any, sessionID string) registry.Delivery {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{RuntimeID: testRuntimeID.String(), RuntimeGeneration: 7, SessionID: sessionID, Kind: kind, Data: raw}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return registry.Delivery{ChatID: 99, TopicID: 4, Kind: kind, Payload: payload}
}

func TestRenderInstancesListsWorkersAndRuntimeState(t *testing.T) {
	store := renderFixture()
	text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "instances"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MacBook", "Primary · running · 1 sessions · 1 running", "Secondary · stopped"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if keyboard == nil || len(keyboard.Rows) != 2 || len(store.callbacks) != 2 {
		t.Fatalf("runtime controls = %#v, callbacks = %#v", keyboard, store.callbacks)
	}
	for _, callback := range store.callbacks {
		if callback.Action != "sessions" || callback.RuntimeID == uuid.Nil || callback.BotID != "bot" || callback.UserID != 42 || callback.ChatID != 99 || callback.TopicID != 4 {
			t.Fatalf("bad instances callback: %#v", callback)
		}
	}
}

func TestRenderRuntimePickerPreservesRequestedAction(t *testing.T) {
	for _, action := range []string{"sessions", "new"} {
		store := renderFixture()
		_, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "runtime_picker", Action: action}))
		if err != nil {
			t.Fatal(err)
		}
		if keyboard == nil || len(store.callbacks) != 2 {
			t.Fatal("missing runtime choices")
		}
		for _, callback := range store.callbacks {
			if callback.Action != action {
				t.Fatalf("action = %q, want %q", callback.Action, action)
			}
		}
	}
}

func TestRenderSessionsFiltersRuntimeAndCreatesExactControls(t *testing.T) {
	store := renderFixture()
	text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "auth-fix") || !strings.Contains(text, "Running · Loaded\n/work/api") || strings.Contains(text, "other-session") {
		t.Fatalf("wrong filtered session view: %q", text)
	}
	if keyboard == nil || len(store.callbacks) != 3 {
		t.Fatalf("controls = %#v", keyboard)
	}
	for index, action := range []string{"select", "status", "new"} {
		if store.callbacks[index].Action != action || store.callbacks[index].RuntimeID != testRuntimeID {
			t.Fatalf("callback %d = %#v", index, store.callbacks[index])
		}
	}
	if store.callbacks[0].SessionID != testSessionID || store.callbacks[1].SessionID != testSessionID || store.callbacks[2].SessionID != uuid.Nil {
		t.Fatal("session controls target the wrong identities")
	}
	for _, row := range keyboard.Rows {
		for _, button := range row {
			if strings.Contains(button.Data, testSessionID.String()) || !strings.HasPrefix(button.Data, "cb:cb_opaque_") {
				t.Fatalf("callback is not opaque: %q", button.Data)
			}
		}
	}
}

func TestRenderStatusSelectedDisconnectAndHelp(t *testing.T) {
	store := renderFixture()
	sender := testSender(store, nil)
	status, keyboard, err := sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "status", SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String()}))
	if err != nil || keyboard != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MacBook / Primary / auth-fix", "Runtime generation: 7", "Codex thread: thread-a", "Active turn: turn-abc", "Pending approvals: 2", "Queued commands: 3", "Last event: 2026-09-13T10:11:12Z"} {
		if !strings.Contains(status, want) {
			t.Fatalf("missing %q in status %q", want, status)
		}
	}
	selected, _, err := sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "selected", SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String()}))
	if err != nil || !strings.Contains(selected, "Connected to auth-fix") || !strings.Contains(selected, "Messages in this chat now target this session") {
		t.Fatal(selected, err)
	}
	disconnected, _, err := sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "disconnected"}))
	if err != nil || !strings.Contains(disconnected, "Selection cleared") || !strings.Contains(disconnected, "not changed") {
		t.Fatal(disconnected, err)
	}
	help, _, err := sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "help"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/tgstart", "/tghelp", "/tginstances", "/tgsessions", "/tgstatus", "/tgdisconnect", "/tgdeletesession", "/tgsteer", "/tginterrupt", "/tglastmessages", "/tgmultisession"} {
		if !strings.Contains(help, command) {
			t.Fatalf("help omits %s", command)
		}
	}
}

func TestRenderApprovalUsesOnlyOfferedDecisions(t *testing.T) {
	store := renderFixture()
	approval := protocol.Approval{ID: testApproval.String(), Type: "command_execution", Summary: "Run go test ./...", Decisions: []string{"acceptOnce", "acceptForSession", "decline"}}
	text, keyboard, err := testSender(store, nil).render(context.Background(), eventRow(t, "approval_requested", approval, testSessionID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Approval required · MacBook / Primary / auth-fix") || !strings.Contains(text, "Run go test ./...") || keyboard == nil {
		t.Fatal(text)
	}
	for index, decision := range approval.Decisions {
		callback := store.callbacks[index]
		if callback.Action != "approval" || callback.Decision != decision || callback.ApprovalID != testApproval || callback.SessionID != testSessionID || callback.Generation != 7 {
			t.Fatalf("approval callback = %#v", callback)
		}
	}
	if keyboard.Rows[2][0].Text != "Decline" {
		t.Fatalf("decline label = %q", keyboard.Rows[2][0].Text)
	}
}

func TestRenderInputOptionsAndNextQuestionUseExactQuestion(t *testing.T) {
	store := renderFixture()
	approval := protocol.Approval{ID: testApproval.String(), Type: "input", Questions: []protocol.Question{
		{ID: "q-one", Header: "Scope", Prompt: "Choose scope", Options: []string{"Current", "All"}},
		{ID: "q-two", Header: "Reason", Prompt: "Why?", Options: []string{"Safety"}},
	}}
	store.approval = approval
	text, keyboard, err := testSender(store, nil).render(context.Background(), eventRow(t, "user_input_requested", approval, testSessionID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Choose scope") || !strings.Contains(text, "Question 1 of 2") || keyboard == nil || len(store.callbacks) != 3 {
		t.Fatalf("first question: %q %#v", text, keyboard)
	}
	for index, answer := range []string{"Current", "All"} {
		callback := store.callbacks[index]
		if callback.Action != "input" || callback.QuestionID != "q-one" || callback.Answer != answer {
			t.Fatalf("option callback = %#v", callback)
		}
	}
	if reply := store.callbacks[2]; reply.Action != "input_prompt" || reply.QuestionID != "q-one" || reply.Answer != "" {
		t.Fatalf("reply callback = %#v", reply)
	}

	store.callbacks = nil
	next := registry.AcceptResult{View: "input_pending", SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String(), ApprovalID: testApproval.String(), QuestionID: "q-two"}
	text, _, err = testSender(store, nil).render(context.Background(), uiRow(t, next))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Why?") || !strings.Contains(text, "Question 2 of 2") || len(store.callbacks) != 2 || store.callbacks[0].QuestionID != "q-two" || store.callbacks[0].Answer != "Safety" {
		t.Fatalf("next question: %q %#v", text, store.callbacks)
	}
}

func TestRenderFinalFailuresDegradedAndNewSession(t *testing.T) {
	store := renderFixture()
	sender := testSender(store, nil)
	final, _, err := sender.render(context.Background(), eventRow(t, "turn_completed", protocol.Result{Text: "Tests pass."}, testSessionID.String()))
	if err != nil || !strings.HasPrefix(final, "✅ MacBook / Primary / auth-fix") || !strings.Contains(final, "Tests pass.") {
		t.Fatal(final, err)
	}
	failed, _, err := sender.render(context.Background(), eventRow(t, "command_failed", protocol.Result{Error: &protocol.Error{Code: protocol.StaleTurn, Message: "Turn changed."}}, testSessionID.String()))
	if err != nil || !strings.Contains(failed, "Turn changed. (stale_turn)") {
		t.Fatal(failed, err)
	}
	degraded, _, err := sender.render(context.Background(), eventRow(t, "runtime_degraded", protocol.Runtime{State: "degraded", CodexVersion: "1.2.3"}, ""))
	if err != nil || !strings.Contains(degraded, "compatibility is degraded") || !strings.Contains(degraded, "1.2.3") {
		t.Fatal(degraded, err)
	}
	createdSession := store.sessions[0]
	created, _, err := sender.render(context.Background(), eventRow(t, "command_completed", protocol.Result{Session: &createdSession}, ""))
	if err != nil || !strings.Contains(created, "New session ready · MacBook / Primary / auth-fix") || !strings.Contains(created, "Reply to this message to use this session") {
		t.Fatal(created, err)
	}
}

func TestRenderRedactsTextAndButtons(t *testing.T) {
	store := renderFixture()
	store.sessions[0].Name = "secret-session"
	redactor, err := auth.NewRedactor([]string{`secret-[a-z]+`, `token=[^ ]+`}, "")
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := testSender(store, redactor).render(context.Background(), eventRow(t, "turn_completed", protocol.Result{Text: "token=abc complete"}, testSessionID.String()))
	if err != nil || strings.Contains(text, "secret-session") || strings.Contains(text, "token=abc") || strings.Count(text, "[REDACTED]") != 2 {
		t.Fatalf("redacted final = %q, err %v", text, err)
	}
	store.callbacks = nil
	store.approval = protocol.Approval{ID: testApproval.String(), Questions: []protocol.Question{{ID: "q", Prompt: "Pick", Options: []string{"secret-option"}}}}
	_, keyboard, err := testSender(store, redactor).render(context.Background(), uiRow(t, registry.AcceptResult{View: "input_pending", SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String(), ApprovalID: testApproval.String(), QuestionID: "q"}))
	if err != nil || keyboard.Rows[0][0].Text != "[REDACTED]" {
		t.Fatalf("redacted button = %#v, err %v", keyboard, err)
	}
}

func TestRenderPropagatesSnapshotCallbackAndPayloadErrors(t *testing.T) {
	boom := errors.New("boom")
	store := renderFixture()
	store.workerErr = boom
	if _, _, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "instances"})); !errors.Is(err, boom) {
		t.Fatalf("snapshot error = %v", err)
	}
	store = renderFixture()
	store.callbackErr = boom
	if _, _, err := testSender(store, nil).render(context.Background(), uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()})); !errors.Is(err, boom) {
		t.Fatalf("callback error = %v", err)
	}
	if _, _, err := testSender(renderFixture(), nil).render(context.Background(), registry.Delivery{Kind: "ui_response", Payload: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed payload rendered successfully")
	}
}
