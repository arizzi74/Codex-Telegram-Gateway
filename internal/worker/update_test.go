package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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

func TestAttachmentProxyDefersUpdateAndFencesNewClients(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	proxy, path := testAttachmentProxy(t)
	a.manager.mu.Lock()
	a.manager.attachments[runtime.ID] = proxy
	a.manager.runtimes[runtime.ID].runtime.LocalSocket = path
	a.manager.mu.Unlock()
	// Prepare before any native attachment. Rejected connection attempts do not
	// reach Codex and therefore do not trip the native-use safety latch.
	lease, err := a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = blocked.SetDeadline(time.Now().Add(time.Second))
	_, _ = blocked.Write([]byte("blocked"))
	if _, err := blocked.Read(make([]byte, 1)); err == nil {
		t.Fatal("new CLI attachment crossed admission fence")
	}
	blocked.Close()
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
	lease, err = a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("blocked attachment incorrectly latched native use: %v", err)
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("live")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 4)
	if _, err := io.ReadFull(client, data); err != nil || string(data) != "live" {
		t.Fatalf("attachment after abort: %q %v", data, err)
	}
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("attached idle CLI allowed update")
	}
	client.Close()
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.clients) == 0 })
	// The frontend may have disconnected after submitting an RPC that Codex has
	// not executed yet. Even a currently idle native snapshot is insufficient:
	// native usage permanently defers automatic restart for this runtime.
	if lease, err := a.prepareUpdate(context.Background(), time.Minute); err == nil {
		_ = a.abortUpdate(lease.Token)
		t.Fatal("native disconnect cleared the update safety latch")
	} else if !strings.Contains(err.Error(), "stop the worker service") {
		t.Fatalf("native-use latch omitted manual-update guidance: %v", err)
	}
	if a.update != nil {
		t.Fatal("native-use rejection left command admission paused")
	}
}

func TestAttachmentProxyHalfCloseAndShutdownDrain(t *testing.T) {
	proxy, path := testAttachmentProxy(t)
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(time.Second))
	payload := strings.Repeat("backpressure", 100000)
	wrote := make(chan error, 1)
	go func() {
		_, err := io.WriteString(client, payload)
		if err == nil {
			err = client.CloseWrite()
		}
		wrote <- err
	}()
	data, err := io.ReadAll(client)
	if err != nil || string(data) != payload {
		t.Fatalf("half-close lost bytes: got=%d want=%d err=%v", len(data), len(payload), err)
	}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	client.Close()
	idle, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	waitFor(t, func() bool { proxy.mu.Lock(); defer proxy.mu.Unlock(); return len(proxy.clients) > 0 })
	done := make(chan struct{})
	go func() { proxy.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy shutdown stranded an attached client")
	}
}

func testAttachmentProxy(t *testing.T) (*attachmentProxy, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "worker-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	backend := filepath.Join(dir, "server.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: backend, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			group.Go(func() { defer connection.Close(); _, _ = io.Copy(connection, connection) })
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done; group.Wait() })
	path := filepath.Join(dir, "app.sock")
	proxy, err := newAttachmentProxy(path, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.close)
	return proxy, path
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
