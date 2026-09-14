package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

func TestAgentColdSessionResumesOnlyOnCommandWithThreadID(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installColdSession(a, runtime, "thread-cold")
	command := agentCommand(runtime, session, protocol.StartTurn)
	command.Arguments.Text = "resume it"
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack %#v %v", ack, err)
	}
	waitFor(t, func() bool { return hasCall(server.Calls(), "turn/start") })
	var resume map[string]any
	for _, call := range server.Calls() {
		if call.Method == "thread/resume" {
			if err := json.Unmarshal(call.Params, &resume); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(resume) != 1 || resume["threadId"] != session.ThreadID {
		t.Fatalf("cold resume parameters = %#v, want only threadId", resume)
	}
}

func TestAgentColdSessionRefusesResumedActiveTurn(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{{"id": "thread-active", "cwd": runtime.DefaultCWD, "status": "active", "turns": []map[string]any{{"id": "turn-live", "status": "inProgress"}}}}, nil)
	session := installColdSession(a, runtime, "thread-active")
	command := agentCommand(runtime, session, protocol.StartTurn)
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandFailed
	})
	if hasCall(server.Calls(), "turn/start") {
		t.Fatal("active resumed thread received a new turn")
	}
	record, found, err := a.store.LoadCommand(command.ID)
	if err != nil || !found || record.Result == nil || record.Result.Error == nil {
		t.Fatalf("record %#v found=%v err=%v", record, found, err)
	}
	if record.Result.Error.Code != protocol.SessionBusy || record.Result.Error.Message != "This thread already has an active turn. Wait for it to finish, or start a new Telegram session." {
		t.Fatalf("active resume result = %#v", record.Result.Error)
	}
}

func TestAgentReportsThreadWriterConflictActionably(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetRPCError("thread/resume", -32000, "thread writer lock is held by another Codex client")
	session := installColdSession(a, runtime, "thread-locked")
	command := agentCommand(runtime, session, protocol.StartTurn)
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandFailed
	})
	record, _, err := a.store.LoadCommand(command.ID)
	if err != nil || record.Result == nil || record.Result.Error == nil {
		t.Fatalf("record %#v err=%v", record, err)
	}
	if record.Result.Error.Code != protocol.SessionBusy || !record.Result.Error.Retryable {
		t.Fatalf("writer conflict result = %#v", record.Result.Error)
	}
	if record.Result.Error.Message != "This thread is open in another Codex client. Close that client and retry, use /fork to create a branch, or use /tgnew to start fresh." {
		t.Fatalf("writer conflict message = %q", record.Result.Error.Message)
	}
}

func TestAgentReportsUnavailableResumeAsUnsupported(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetMethodUnavailable("thread/resume", true)
	session := installColdSession(a, runtime, "thread-old-server")
	command := agentCommand(runtime, session, protocol.StartTurn)
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandFailed
	})
	record, _, err := a.store.LoadCommand(command.ID)
	if err != nil || record.Result == nil || record.Result.Error == nil {
		t.Fatalf("record %#v err=%v", record, err)
	}
	if record.Result.Error.Code != protocol.CodexMethodUnsupported {
		t.Fatalf("unavailable resume result = %#v", record.Result.Error)
	}
}

func TestAgentCodexUsageErrorIsDefiniteFailure(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-command", "")
	command := agentCommand(runtime, session, protocol.CodexCommand)
	command.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "compact", Args: "unexpected"}}
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandFailed
	})
	if hasCall(server.Calls(), "thread/compact/start") {
		t.Fatal("invalid command reached Codex")
	}
	record, _, err := a.store.LoadCommand(command.ID)
	if err != nil || record.Result == nil || record.Result.Error == nil {
		t.Fatalf("record %#v err=%v", record, err)
	}
	if record.Result.Error.Code != protocol.CodexCommandInvalid || record.Result.Error.Message != "/compact does not accept arguments" {
		t.Fatalf("usage result = %#v", record.Result.Error)
	}
}

func TestAgentCodexReviewRecordsCorrelatedTurnStarted(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-review", "")
	command := agentCommand(runtime, session, protocol.CodexCommand)
	command.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "review"}}
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack %#v %v", ack, err)
	}
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) != "" })
	turnID := sessionTurn(a.store, session.ID)
	if !hasCall(server.Calls(), "review/start") {
		t.Fatal("review command did not start its Codex turn")
	}
	waitFor(t, func() bool { return hasTurnEventForCommand(a.store, "turn_started", command.ID, turnID) })
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: turnID})
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) == "" })
	assertTurnCompletedForCommand(t, a.store, command.ID, turnID)
}

func TestAgentCompactAwaitsTurnStartedBeforeReleasingQueue(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-compact", "")
	compact := agentCommand(runtime, session, protocol.CodexCommand)
	compact.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "compact"}}
	if ack, err := a.HandleCommand(context.Background(), compact); err != nil || ack.Status != "accepted" {
		t.Fatalf("compact ack %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(compact.ID)
		return err == nil && found && record.State == CommandExecuting && hasCall(server.Calls(), "thread/compact/start")
	})
	next := agentCommand(runtime, session, protocol.StartTurn)
	next.Arguments.Text = "after compaction"
	if ack, err := a.HandleCommand(context.Background(), next); err != nil || ack.Status != "accepted" {
		t.Fatalf("queued ack %#v %v", ack, err)
	}
	if !hasCall(server.Calls(), "thread/compact/start") {
		t.Fatal("compact command did not reach Codex")
	}
	if got := countCall(server.Calls(), "turn/start"); got != 0 {
		t.Fatalf("queued turn started before compact emitted turn_started: %d", got)
	}
	compactTurn := "turn-compact"
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: compactTurn})
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) == compactTurn })
	waitFor(t, func() bool { return hasTurnEventForCommand(a.store, "turn_started", compact.ID, compactTurn) })
	record, found, err := a.store.LoadCommand(compact.ID)
	if err != nil || !found || record.State != CommandCompleted {
		t.Fatalf("compact did not complete at turn_started: %#v found=%v err=%v", record, found, err)
	}
	if got := countCall(server.Calls(), "turn/start"); got != 0 {
		t.Fatalf("queued turn started before compact completion: %d", got)
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: compactTurn})
	waitFor(t, func() bool { return countCall(server.Calls(), "turn/start") == 1 })
	assertTurnCompletedForCommand(t, a.store, compact.ID, compactTurn)
}

func TestAgentCodexStatusDuringActiveTurnPreservesOriginalCommand(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-status-active", "")
	original := agentCommand(runtime, session, protocol.StartTurn)
	original.Arguments.Text = "keep working"
	if ack, err := a.HandleCommand(context.Background(), original); err != nil || ack.Status != "accepted" {
		t.Fatalf("start ack %#v %v", ack, err)
	}
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) != "" })
	turnID := sessionTurn(a.store, session.ID)
	status := agentCommand(runtime, session, protocol.CodexCommand)
	status.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "status"}}
	status.ExpectedTurnID = ""
	if ack, err := a.HandleCommand(context.Background(), status); err != nil || ack.Status != "accepted" {
		t.Fatalf("status ack %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(status.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	if got := countCall(server.Calls(), "turn/start"); got != 1 {
		t.Fatalf("turn/start calls = %d, want original turn only", got)
	}
	if got := countCall(server.Calls(), "turn/interrupt"); got != 0 {
		t.Fatalf("turn/interrupt calls = %d", got)
	}
	if got := sessionTurn(a.store, session.ID); got != turnID {
		t.Fatalf("active turn changed to %q, want %q", got, turnID)
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: turnID})
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) == "" })
	assertTurnCompletedForCommand(t, a.store, original.ID, turnID)
}

func TestAgentExecutionErrorRedactsAndBoundsProtocolError(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-error", "")
	command := agentCommand(runtime, session, protocol.CodexCommand)
	command.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "status"}}
	if received, err := a.store.Receive(command); err != nil || !received.Accepted {
		t.Fatalf("receive %#v %v", received, err)
	}
	if err := a.store.SetCommandState(command.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	message := "sk-abcdefghijklmnopqrstuvwxyz0123456789-secret " + strings.Repeat("é", 300)
	if err := a.executionError(command, &protocol.Error{Code: "invalid_command", Message: message}); err != nil {
		t.Fatal(err)
	}
	record, _, err := a.store.LoadCommand(command.ID)
	if err != nil || record.State != CommandFailed || record.Result == nil || record.Result.Error == nil {
		t.Fatalf("record %#v err=%v", record, err)
	}
	if strings.Contains(record.Result.Error.Message, "sk-") || len([]rune(record.Result.Error.Message)) > 241 {
		t.Fatalf("unsafe bounded message = %q", record.Result.Error.Message)
	}
}

func TestAgentDiscoveryPublishesMetadataWithoutReplacingActiveTurn(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-metadata", "turn-current")
	session.Name, session.GitBranch = "Renamed session", "feature"
	session.ActiveTurnID, session.State = "", "idle"
	a.onSession(runtime, session)
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind != "session_state_changed" {
			continue
		}
		var current protocol.Session
		if err := json.Unmarshal(event.Data, &current); err != nil {
			t.Fatal(err)
		}
		if current.Name == "Renamed session" && current.GitBranch == "feature" && current.ActiveTurnID == "turn-current" {
			found = true
		}
	}
	if !found {
		t.Fatal("metadata refresh did not publish the actual fenced actor state")
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

func installColdSession(a *Agent, runtime protocol.Runtime, threadID string) protocol.Session {
	s, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: threadID, CWD: runtime.DefaultCWD, State: "not_loaded", Loaded: false})
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

func assertTurnCompletedForCommand(t *testing.T, store *Store, commandID, turnID string) {
	t.Helper()
	if !hasTurnEventForCommand(store, "turn_completed", commandID, turnID) {
		t.Fatalf("missing turn_completed for command=%s turn=%s", commandID, turnID)
	}
}

func hasTurnEventForCommand(store *Store, kind, commandID, turnID string) bool {
	events, err := store.OutboxAfter(0)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.Kind != kind {
			continue
		}
		var result protocol.Result
		if json.Unmarshal(event.Data, &result) == nil && result.CommandID == commandID && result.TurnID == turnID {
			return true
		}
	}
	return false
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
