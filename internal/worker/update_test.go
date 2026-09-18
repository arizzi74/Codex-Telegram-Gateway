package worker

import (
	"context"
	"encoding/json"
	"errors"
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

func TestUpdatePreparationRefusesPreviouslyTimedOutRuntimeRPC(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	client := mustClient(t, a, runtime)
	server.SetResponseDelay("thread/read", 40*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.ReadThread(ctx, "native-timeout", true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delayed RPC = %v", err)
	}
	// A subsequent successful read proves the connection recovered, without
	// proving whether timed-out server work can start in the future.
	if _, err := client.LoadedThreads(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	}
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("worker allowed restart after an unconfirmed runtime RPC")
	} else if !strings.Contains(err.Error(), "unconfirmed RPCs") {
		t.Fatalf("unexpected uncertainty refusal: %v", err)
	}
}

func TestUpdatePreparationAndCommandAcceptanceHaveOneWinner(t *testing.T) {
	for range 10 {
		a, runtime, _, cleanup := testAgent(t)
		session := installSession(a, runtime, "thread-race", "")
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
