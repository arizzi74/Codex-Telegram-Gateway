package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestUpdatePreparationFencesCommandsWithoutReceivingOrFailingThem(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-idle", "")
	waitUpdateSessionReady(t, a, runtime, session)
	lease, err := a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer a.abortUpdate(lease.Token)
	if lease.WorkerID != a.cfg.WorkerID || lease.PID != os.Getpid() || lease.Token == "" || time.Until(lease.ExpiresAt) < 50*time.Second {
		t.Fatalf("invalid preparation lease: %+v", lease)
	}
	command := agentCommand(runtime, session, protocol.StartTurn)
	if _, err := a.HandleCommand(context.Background(), command); !errors.Is(err, ErrUpdatePrepared) {
		t.Fatalf("paused command = %v", err)
	}
	if _, found, err := a.store.LoadCommand(command.ID); err != nil || found {
		t.Fatalf("paused command entered ledger: found=%t err=%v", found, err)
	}
	writes := make(chan outbound, 1)
	connection := &Connection{onCommand: a.HandleCommand}
	connection.handleCommand(context.Background(), writes, command)
	if len(writes) != 0 {
		t.Fatal("paused command was acknowledged or permanently rejected")
	}
	if err := a.abortUpdate("wrong-token"); err == nil {
		t.Fatal("wrong lease token cleared admission fence")
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("command after abort = %+v %v", ack, err)
	}
	waitFor(t, func() bool { return hasCall(server.Calls(), "turn/start") })
}

func TestUpdatePreparationExpiresAndReopensAdmission(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-idle", "")
	waitUpdateSessionReady(t, a, runtime, session)
	lease, err := a.prepareUpdate(context.Background(), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { a.updateMu.Lock(); defer a.updateMu.Unlock(); return a.update == nil })
	if err := a.abortUpdate(lease.Token); err == nil {
		t.Fatal("expired lease remained prepared")
	}
	if ack, err := a.HandleCommand(context.Background(), agentCommand(runtime, session, protocol.StartTurn)); err != nil || ack.Status != "accepted" {
		t.Fatalf("admission did not recover after timeout: %+v %v", ack, err)
	}
}

func TestUpdatePreparationRefusesDurableAndActorWork(t *testing.T) {
	for _, state := range []string{"active_turn", "waiting_approval", "waiting_input", "queued_command", "executing_command", "unacked_event", "runtime_starting"} {
		t.Run(state, func(t *testing.T) {
			a, runtime, _, cleanup := testAgent(t)
			defer cleanup()
			session := protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread-busy", State: "idle", Loaded: true, CWD: runtime.DefaultCWD}
			switch state {
			case "active_turn":
				session.ActiveTurnID = "turn-live"
			case "waiting_approval", "waiting_input":
				session.State = state
			}
			session, err := a.store.UpsertSession(session)
			if err != nil {
				t.Fatal(err)
			}
			a.onSession(runtime, session)
			switch state {
			case "queued_command", "executing_command":
				command := agentCommand(runtime, session, protocol.StartTurn)
				if _, err := a.store.Receive(command); err != nil {
					t.Fatal(err)
				}
				if state == "executing_command" {
					if err := a.store.SetCommandState(command.ID, CommandExecuting, nil); err != nil {
						t.Fatal(err)
					}
				}
			case "unacked_event":
				if _, err := a.store.AppendEvent(protocol.Event{Kind: "runtime_started", RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, Data: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
			case "runtime_starting":
				a.manager.lifecycle.RLock()
				defer a.manager.lifecycle.RUnlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if lease, err := a.prepareUpdate(ctx, time.Minute); err == nil {
				_ = a.abortUpdate(lease.Token)
				t.Fatal("busy worker allowed update")
			}
			if a.update != nil {
				t.Fatal("failed preparation left admission paused")
			}
		})
	}
}

func TestUpdatePreparationReadsNativeThreadsOutsideDiscoveredSessions(t *testing.T) {
	a, _, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{{"id": "external-native", "cwd": "/outside/gateway/workspaces", "status": "active", "turns": []map[string]any{{"id": "native-turn", "status": "inProgress"}}}}, []string{"external-native"})
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("native turn outside gateway session registry allowed update")
	}
}

func TestUpdateIdleCheckUsesBoundedTurnStateAndFailsClosed(t *testing.T) {
	for _, state := range []string{"completed", "inProgress", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			a, _, server, cleanup := testAgent(t)
			defer cleanup()
			server.SetThreads([]map[string]any{{"id": "native-image-thread", "status": "idle", "turns": []map[string]any{{"id": "latest", "status": state}}}}, []string{"native-image-thread"})
			server.SetMethodUnavailable("thread/turns/list", state == "unavailable")
			err := a.manager.verifyUpdateIdle(context.Background())
			if (err != nil) != (state != "completed") {
				t.Fatalf("idle check for %s = %v", state, err)
			}
			for _, call := range server.Calls() {
				var p map[string]any
				if err := json.Unmarshal(call.Params, &p); err != nil {
					t.Fatal(err)
				}
				if call.Method == "thread/read" && p["includeTurns"] != false {
					t.Fatal("update check requested image history")
				}
				if call.Method == "thread/turns/list" && (p["limit"] != float64(1) || p["itemsView"] != "notLoaded") {
					t.Fatal("update check requested unbounded turn items")
				}
			}
		})
	}
}

func TestUpdatePreparationWithThreadWithoutFirstUserMessage(t *testing.T) {
	for _, state := range []string{"idle", "active", "unknown"} {
		t.Run(state, func(t *testing.T) {
			a, _, server, cleanup := testAgent(t)
			defer cleanup()
			server.SetThreads([]map[string]any{{"id": "empty-native", "status": state}}, []string{"empty-native"})
			server.SetRPCError("thread/turns/list", -32600, "thread empty-native is not materialized yet; thread/turns/list is unavailable before first user message")
			lease, err := a.prepareUpdate(context.Background(), time.Minute)
			if err == nil {
				defer a.abortUpdate(lease.Token)
			}
			if (err != nil) != (state != "idle") {
				t.Fatalf("update preparation for empty %s thread = %v", state, err)
			}
		})
	}
}

func TestUpdatePreparationRefusesInFlightTurnStart(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-starting", "")
	server.SetResponseDelay("turn/start", 100*time.Millisecond)
	if _, err := a.HandleCommand(context.Background(), agentCommand(runtime, session, protocol.StartTurn)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hasCall(server.Calls(), "turn/start") })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if lease, err := a.prepareUpdate(ctx, time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("in-flight turn start allowed update")
	}
}

func TestUpdatePreparationRecoversCancelledMetadataReadAndStillChecksTurns(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint("active=", active), func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			client := mustClient(t, a, runtime)
			server.SetResponseDelay("thread/read", 100*time.Millisecond)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.ReadThread(ctx, "native-timeout", false); done <- err }()
			waitFor(t, func() bool { return hasCall(server.Calls(), "thread/read") })
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled read = %v", err)
			}
			// A later reply and successful read must not leave a permanent blocker.
			if _, err := client.LoadedThreads(context.Background(), "", 1); err != nil {
				t.Fatal(err)
			}
			server.SetResponseDelay("thread/read", 0)
			if active {
				server.SetThreads([]map[string]any{{"id": "outside-inventory", "status": "active", "turns": []map[string]any{{"id": "turn-live", "status": "inProgress"}}}}, []string{"outside-inventory"})
			}
			lease, err := a.prepareUpdate(context.Background(), time.Minute)
			if err == nil {
				defer a.abortUpdate(lease.Token)
			}
			if active && err == nil {
				t.Fatal("metadata-read recovery bypassed live turn verification")
			}
			if !active && err != nil {
				t.Fatalf("cancelled metadata read blocked idle update: %v", err)
			}
		})
	}
}

func TestUpdatePreparationRetainsCancelledMutationAndPublishesSafeDiagnostics(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	client := mustClient(t, a, runtime)
	server.SetResponseDelay("thread/name/set", 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.RenameThread(ctx, "private-thread-id", "private-session-name") }()
	waitFor(t, func() bool { return hasCall(server.Calls(), "thread/name/set") })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled mutation = %v", err)
	}
	// Even a late success plus an unrelated healthy read cannot establish
	// completion of arbitrary accepted work from the caller's point of view.
	if _, err := client.LoadedThreads(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("cancelled mutation allowed restart")
	} else if !strings.Contains(err.Error(), "unconfirmed mutating requests") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	a.cfg.StateFile = filepath.Join(t.TempDir(), "diagnostics.db")
	if err := a.writeStatus(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.cfg.StateFile + ".status.json")
	if err != nil {
		t.Fatal(err)
	}
	var status Status
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.RPCUpdateSafety) != 1 {
		t.Fatalf("missing runtime diagnostics: %+v", status.RPCUpdateSafety)
	}
	rpc := status.RPCUpdateSafety[0]
	if rpc.RuntimeID != runtime.ID || rpc.Generation != runtime.Generation || rpc.UnconfirmedRequests != 1 || len(rpc.Blockers) != 1 || rpc.Blockers[0].Method != "thread/name/set" || rpc.Blockers[0].StartedAt.IsZero() {
		t.Fatalf("incorrect RPC diagnostics: %+v", rpc)
	}
	metadata, err := json.Marshal(rpc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metadata), "private-thread-id") || strings.Contains(string(metadata), "private-session-name") {
		t.Fatalf("diagnostics leaked RPC parameters: %s", metadata)
	}
}

func TestUpdatePreparationAndCommandAcceptanceHaveOneWinner(t *testing.T) {
	for range 10 {
		a, runtime, _, cleanup := testAgent(t)
		session := installSession(a, runtime, "thread-race", "")
		waitUpdateSessionReady(t, a, runtime, session)
		start := make(chan struct{})
		var lease UpdateLease
		var prepareErr, commandErr error
		var ack protocol.CommandAck
		var group sync.WaitGroup
		group.Go(func() { <-start; lease, prepareErr = a.prepareUpdate(context.Background(), time.Minute) })
		group.Go(func() {
			<-start
			ack, commandErr = a.HandleCommand(context.Background(), agentCommand(runtime, session, protocol.StartTurn))
		})
		close(start)
		group.Wait()
		if prepareErr == nil {
			if !errors.Is(commandErr, ErrUpdatePrepared) {
				t.Errorf("update and command were both admitted: ack=%+v err=%v", ack, commandErr)
			}
			_ = a.abortUpdate(lease.Token)
		} else if commandErr != nil || ack.Status != "accepted" {
			t.Errorf("neither racing operation admitted: update=%v command=%+v,%v", prepareErr, ack, commandErr)
		}
		cleanup()
	}
}

// Installing an actor is asynchronous. Let its initial question-history read
// finish before a test starts from an idle worker; the production updater must
// still refuse an RPC that races its idle check.
func waitUpdateSessionReady(t *testing.T, a *Agent, runtime protocol.Runtime, session protocol.Session) {
	t.Helper()
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	if actor == nil {
		t.Fatal("session actor is missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reply := make(chan bool, 1)
	select {
	case actor.updateChecks <- reply:
	case <-ctx.Done():
		t.Fatal("session initialization did not finish")
	}
	select {
	case idle := <-reply:
		if !idle {
			t.Fatal("session fixture is not idle")
		}
	case <-ctx.Done():
		t.Fatal("session initialization was not acknowledged")
	}
}

func TestUpdateControlUsesLiveWorkerAndPrivateSocket(t *testing.T) {
	a, _, _, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.StateFile = filepath.Join(t.TempDir(), "worker.db")
	stop, err := a.startUpdateControl(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	info, err := os.Stat(updateControlPath(a.cfg.StateFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("control socket permissions = %v %v", info, err)
	}
	lease, err := RequestUpdate(context.Background(), a.cfg, "prepare", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RequestUpdate(context.Background(), a.cfg, "abort", "wrong"); err == nil {
		t.Fatal("wrong abort token accepted")
	}
	if _, err := RequestUpdate(context.Background(), a.cfg, "abort", lease.Token); err != nil {
		t.Fatal(err)
	}
	wrong := a.cfg
	wrong.WorkerID = "another-worker"
	if _, err := RequestUpdate(context.Background(), wrong, "prepare", ""); err == nil {
		t.Fatal("control socket accepted wrong worker identity")
	}
	if _, err := RequestUpdate(context.Background(), config.WorkerConfig{WorkerID: a.cfg.WorkerID, StateFile: filepath.Join(t.TempDir(), "missing.db")}, "prepare", ""); err == nil {
		t.Fatal("status-only/legacy worker authorized a restart")
	}
}

func TestUpdateControlShutdownCancelsInFlightPreparation(t *testing.T) {
	a, _, server, cleanup := testAgent(t)
	defer cleanup()
	a.cfg.StateFile = filepath.Join(t.TempDir(), "worker.db")
	server.SetResponseDelay("thread/loaded/list", time.Second)
	stop, err := a.startUpdateControl(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := RequestUpdate(context.Background(), a.cfg, "prepare", ""); done <- err }()
	waitFor(t, func() bool { return hasCall(server.Calls(), "thread/loaded/list") })
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("control shutdown waited for an unrelated runtime RPC")
	}
	if err := <-done; err == nil {
		t.Fatal("shutdown returned a usable update lease")
	}
}

func TestUpdateLeaseDoesNotBlockRuntimeShutdown(t *testing.T) {
	a, _, _, cleanup := testAgent(t)
	defer cleanup()
	a.manager.cfg.Runtimes[0].Autostart = true
	a.manager.lifecycle.Lock()
	defer a.manager.lifecycle.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.manager.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("worker shutdown waited for update lease expiration")
	}
}

// Ensure the update check mailbox also treats a pending Codex request as busy,
// before its durable session state or the final turn result can change.
func TestUpdatePreparationRefusesPendingApprovalMailbox(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-approval", "")
	if err := server.Request("item/commandExecution/requestApproval", 42, map[string]any{"threadId": session.ThreadID, "turnId": "turn-approval", "command": "make", "availableDecisions": []string{"accept", "decline"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-mustClient(t, a, runtime).Requests():
		a.onRequest(runtime, request)
	case <-time.After(time.Second):
		t.Fatal("adapter request missing")
	}
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("pending approval allowed update")
	}
}
