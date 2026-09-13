package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestAgentAcceptsDuplicateAndRejectsStaleGenerationWithoutRPC(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-1", "")
	command := agentCommand(runtime, session, protocol.StartTurn)
	command.Arguments.Text = "first"
	ack, err := a.HandleCommand(context.Background(), command)
	if err != nil || ack.Status != "accepted" {
		t.Fatalf("first ack %#v %v", ack, err)
	}
	waitFor(t, func() bool { return hasCall(server.Calls(), "turn/start") })
	duplicate, err := a.HandleCommand(context.Background(), command)
	if err != nil || duplicate.Status != "duplicate" {
		t.Fatalf("duplicate ack %#v %v", duplicate, err)
	}
	stale := agentCommand(runtime, session, protocol.Steer)
	stale.RuntimeGeneration++
	stale.ExpectedTurnID = "missing"
	stale.Arguments.Text = "nope"
	ack, err = a.HandleCommand(context.Background(), stale)
	if err != nil || ack.Status != "rejected_stale_runtime" {
		t.Fatalf("stale ack %#v %v", ack, err)
	}
	if countCall(server.Calls(), "turn/steer") != 0 {
		t.Fatal("stale command reached Codex")
	}
	if countCall(server.Calls(), "turn/start") != 1 {
		t.Fatal("duplicate turn caused a second RPC")
	}
}

func TestAgentSessionFIFOAndIndependentSessions(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	first := installSession(a, runtime, "thread-a", "")
	second := installSession(a, runtime, "thread-b", "")
	c1 := agentCommand(runtime, first, protocol.StartTurn)
	c1.Arguments.Text = "one"
	c2 := agentCommand(runtime, first, protocol.StartTurn)
	c2.Arguments.Text = "two"
	c3 := agentCommand(runtime, first, protocol.StartTurn)
	c3.Arguments.Text = "three"
	c4 := agentCommand(runtime, second, protocol.StartTurn)
	c4.Arguments.Text = "other"
	for _, command := range []protocol.Command{c1, c2, c3, c4} {
		if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
			t.Fatalf("ack %#v %v", ack, err)
		}
	}
	waitFor(t, func() bool { return countCall(server.Calls(), "turn/start") == 2 }) // one active turn per session
	if countCall(server.Calls(), "turn/start") != 2 {
		t.Fatal("same-session FIFO was not held")
	}
	// Completing the first session turn releases only its queued command.
	waitFor(t, func() bool { return sessionTurn(a.store, first.ID) != "" })
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: first.ThreadID, TurnID: activeTurn(t, a.store, first.ID)})
	waitFor(t, func() bool { return countCall(server.Calls(), "turn/start") == 3 })
	waitFor(t, func() bool { return sessionTurn(a.store, first.ID) != "" })
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: first.ThreadID, TurnID: sessionTurn(a.store, first.ID)})
	waitFor(t, func() bool { return countCall(server.Calls(), "turn/start") == 4 })
	if got := turnTexts(server.Calls(), first.ThreadID); len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("same-session FIFO texts: %v", got)
	}
}

func TestAgentControlExpectedTurnFence(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-control", "turn-1")
	steer := agentCommand(runtime, session, protocol.Steer)
	steer.ExpectedTurnID = "turn-1"
	steer.Arguments.Text = "keep going"
	if ack, err := a.HandleCommand(context.Background(), steer); err != nil || ack.Status != "accepted" {
		t.Fatalf("steer %#v %v", ack, err)
	}
	waitFor(t, func() bool { return countCall(server.Calls(), "turn/steer") == 1 })
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "turn-2"})
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) == "turn-2" })
	stale := agentCommand(runtime, session, protocol.Interrupt)
	stale.ExpectedTurnID = "turn-1"
	stale.Arguments.Text = ""
	ack, err := a.HandleCommand(context.Background(), stale)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.StaleTurn {
		t.Fatalf("stale control %#v %v", ack, err)
	}
	if countCall(server.Calls(), "turn/interrupt") != 0 {
		t.Fatal("stale interrupt reached Codex")
	}
	valid := agentCommand(runtime, session, protocol.Interrupt)
	valid.ExpectedTurnID = "turn-2"
	valid.Arguments.Text = ""
	if ack, err := a.HandleCommand(context.Background(), valid); err != nil || ack.Status != "accepted" {
		t.Fatalf("interrupt %#v %v", ack, err)
	}
	waitFor(t, func() bool { return countCall(server.Calls(), "turn/interrupt") == 1 })
}

func TestAgentRefusesQueueOverflowAndExpiredCommands(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.MaxQueuedTurns = 1
	session := installSession(a, runtime, "thread-queue", "turn-active")
	first := agentCommand(runtime, session, protocol.StartTurn)
	first.Arguments.Text = "queued"
	if ack, err := a.HandleCommand(context.Background(), first); err != nil || ack.Status != "accepted" {
		t.Fatalf("queue first %#v %v", ack, err)
	}
	second := agentCommand(runtime, session, protocol.StartTurn)
	second.Arguments.Text = "overflow"
	ack, err := a.HandleCommand(context.Background(), second)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.SessionQueueFull {
		t.Fatalf("queue overflow %#v %v", ack, err)
	}
	expired := agentCommand(runtime, session, protocol.Interrupt)
	expired.ExpectedTurnID = "turn-active"
	expired.Arguments.Text = ""
	expired.CreatedAt = time.Now().Add(-2 * time.Hour)
	expired.ExpiresAt = time.Now().Add(-time.Hour)
	ack, err = a.HandleCommand(context.Background(), expired)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.CommandExpired {
		t.Fatalf("expired %#v %v", ack, err)
	}
}

func TestAgentApprovalRequiresExactPendingRequestAndClears(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-approval", "turn-live")
	if err := server.Request("item/commandExecution/requestApproval", 42, map[string]any{"threadId": session.ThreadID, "turnId": "turn-live", "command": "make test", "availableDecisions": []string{"accept", "decline"}}); err != nil {
		t.Fatal(err)
	}
	var first codexadapter.Request
	select {
	case first = <-mustClient(t, a, runtime).Requests():
	case <-time.After(time.Second):
		t.Fatal("adapter request missing")
	}
	a.onRequest(runtime, first)
	if err := server.Request("item/commandExecution/requestApproval", 43, map[string]any{"threadId": session.ThreadID, "turnId": "turn-live", "command": "make other", "availableDecisions": []string{"accept", "decline"}}); err != nil {
		t.Fatal(err)
	}
	var second codexadapter.Request
	select {
	case second = <-mustClient(t, a, runtime).Requests():
	case <-time.After(time.Second):
		t.Fatal("second adapter request missing")
	}
	a.onRequest(runtime, second)
	waitFor(t, func() bool { return len(approvalIDs(t, a.store)) == 2 })
	ids := approvalIDs(t, a.store)
	bad := agentCommand(runtime, session, protocol.ApprovalResponse)
	bad.Arguments = protocol.Arguments{RequestID: first.RequestID, ApprovalID: uuid.NewString(), Decision: "accept"}
	bad.ExpectedTurnID = "turn-live"
	ack, err := a.HandleCommand(context.Background(), bad)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.ApprovalNotPending {
		t.Fatalf("mismatched approval accepted: %#v %v", ack, err)
	}
	valid := agentCommand(runtime, session, protocol.ApprovalResponse)
	valid.Arguments = protocol.Arguments{RequestID: first.RequestID, ApprovalID: ids[first.RequestID], Decision: "accept"}
	valid.ExpectedTurnID = "turn-live"
	ack, err = a.HandleCommand(context.Background(), valid)
	if err != nil || ack.Status != "accepted" {
		t.Fatalf("valid approval rejected: %#v %v", ack, err)
	}
	deadline := time.Now().Add(time.Second)
	for !responseID(server.Responses(), "42") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !responseID(server.Responses(), "42") {
		record, _, _ := a.store.LoadCommand(valid.ID)
		t.Fatalf("approval response ID 42 missing: %#v command=%#v", server.Responses(), record)
	}
	// Resolution for X removes only X. A later click on X cannot consume Y.
	a.onEvent(runtime, codexadapter.Event{Kind: "server_request_resolved", ThreadID: session.ThreadID, RequestID: first.RequestID})
	waitFor(t, func() bool { return approvalResolved(t, a.store, first.RequestID) })
	old := agentCommand(runtime, session, protocol.ApprovalResponse)
	old.Arguments = protocol.Arguments{RequestID: first.RequestID, ApprovalID: ids[first.RequestID], Decision: "accept"}
	old.ExpectedTurnID = "turn-live"
	ack, err = a.HandleCommand(context.Background(), old)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.ApprovalNotPending {
		t.Fatalf("cleared approval accepted: %#v %v", ack, err)
	}
	y := agentCommand(runtime, session, protocol.ApprovalResponse)
	y.Arguments = protocol.Arguments{RequestID: second.RequestID, ApprovalID: ids[second.RequestID], Decision: "accept"}
	y.ExpectedTurnID = "turn-live"
	ack, err = a.HandleCommand(context.Background(), y)
	if err != nil || ack.Status != "accepted" {
		t.Fatalf("independent approval Y rejected: %#v %v", ack, err)
	}
	waitFor(t, func() bool { return responseID(server.Responses(), "43") })
}

func testAgent(t *testing.T) (*Agent, protocol.Runtime, *codextest.Server, func()) {
	t.Helper()
	root := t.TempDir()
	workerID := uuid.NewString()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), workerID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.WorkerConfig{WorkerID: workerID, AllowedWorkspaceRoots: []string{root}, MaxQueuedTurns: 20, Runtimes: []config.RuntimeProfile{{ID: "profile", WorkingDirectory: root}}}
	a, err := NewAgent(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.ctx = ctx
	client, server, err := codextest.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.BeginRuntime("profile", "Profile", root)
	if err != nil {
		t.Fatal(err)
	}
	runtime.State = "running"
	a.manager.install(runtime, client)
	return a, runtime, server, func() { cancel(); a.group.Wait(); server.Close(); _ = store.Close() }
}

func installSession(a *Agent, runtime protocol.Runtime, threadID, active string) protocol.Session {
	s, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: threadID, CWD: runtime.DefaultCWD, State: "idle", Loaded: true, ActiveTurnID: active})
	if err != nil {
		panic(err)
	}
	a.onSession(runtime, s)
	return s
}
func agentCommand(runtime protocol.Runtime, s protocol.Session, op protocol.Operation) protocol.Command {
	now := time.Now()
	return protocol.Command{ID: uuid.NewString(), WorkerID: runtime.WorkerID, RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: s.ID, ThreadID: s.ThreadID, Operation: op, ExpectedTurnID: s.ActiveTurnID, Arguments: protocol.Arguments{Text: "x"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met")
		}
		time.Sleep(time.Millisecond)
	}
}
func hasCall(calls []codextest.Call, method string) bool { return countCall(calls, method) > 0 }
func countCall(calls []codextest.Call, method string) int {
	n := 0
	for _, call := range calls {
		if call.Method == method {
			n++
		}
	}
	return n
}
func turnTexts(calls []codextest.Call, threadID string) []string {
	var texts []string
	for _, call := range calls {
		if call.Method != "turn/start" {
			continue
		}
		var payload struct {
			ThreadID string `json:"threadId"`
			Input    []struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.Unmarshal(call.Params, &payload)
		if payload.ThreadID == threadID && len(payload.Input) > 0 {
			texts = append(texts, payload.Input[0].Text)
		}
	}
	return texts
}
func activeTurn(t *testing.T, store *Store, id string) string {
	t.Helper()
	if turn := sessionTurn(store, id); turn != "" {
		return turn
	}
	t.Fatal("active turn missing")
	return ""
}
func sessionTurn(store *Store, id string) string {
	sessions, err := store.ListSessions("")
	if err != nil {
		return ""
	}
	for _, s := range sessions {
		if s.ID == id {
			return s.ActiveTurnID
		}
	}
	return ""
}
func mustClient(t *testing.T, a *Agent, runtime protocol.Runtime) *codexadapter.Client {
	t.Helper()
	c, _, ok := a.manager.Client(runtime.ID)
	if !ok {
		t.Fatal("missing client")
	}
	return c
}
func approvalIDs(t *testing.T, store *Store) map[string]string {
	t.Helper()
	result := map[string]string{}
	events, err := store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "approval_requested" {
			var approval protocol.Approval
			if json.Unmarshal(event.Data, &approval) == nil {
				result[approval.RequestID] = approval.ID
			}
		}
	}
	return result
}
func approvalResolved(t *testing.T, store *Store, requestID string) bool {
	t.Helper()
	events, err := store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "approval_resolved" {
			var approval protocol.Approval
			if json.Unmarshal(event.Data, &approval) == nil && approval.RequestID == requestID {
				return true
			}
		}
	}
	return false
}
func responseID(responses []codextest.Response, want string) bool {
	for _, response := range responses {
		if string(response.ID) == want {
			return true
		}
	}
	return false
}
