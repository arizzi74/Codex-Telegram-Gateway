package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func inventoryByThread(t *testing.T, store *Store, runtime protocol.Runtime) map[string]protocol.Session {
	t.Helper()
	sessions, err := store.ListSessions(runtime.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]protocol.Session{}
	for _, session := range sessions {
		result[session.ThreadID] = session
	}
	return result
}

func TestDiscoveryHidesHelpersAndObsoleteInventory(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	old := map[string]protocol.Session{}
	for _, id := range []string{"cli", "old-helper", "loaded-helper", "missing", "outside"} {
		saved, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: id, CWD: runtime.DefaultCWD, State: "not_loaded"})
		if err != nil {
			t.Fatal(err)
		}
		old[id] = saved
	}
	threads := []map[string]any{}
	for _, source := range []string{"cli", "vscode", "exec", "appServer"} {
		threads = append(threads, map[string]any{"id": source, "cwd": runtime.DefaultCWD, "source": source, "status": "idle"})
	}
	threads = append(threads,
		map[string]any{"id": "loaded-helper", "cwd": runtime.DefaultCWD, "source": map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "cli"}}}, "status": "active", "turns": []map[string]any{{"id": "helper-turn", "status": "inProgress"}}},
		map[string]any{"id": "new-helper", "cwd": runtime.DefaultCWD, "source": map[string]any{"subAgent": "review"}},
		map[string]any{"id": "ephemeral", "cwd": runtime.DefaultCWD, "source": "appServer", "ephemeral": true},
		map[string]any{"id": "outside", "cwd": t.TempDir(), "source": "cli"},
	)
	server.SetThreads(threads, []string{"cli", "loaded-helper", "new-helper", "ephemeral"})
	client, _, _ := a.manager.Client(runtime.ID)
	if err := a.manager.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	sessions := inventoryByThread(t, a.store, runtime)
	for _, id := range []string{"cli", "vscode", "exec", "appServer"} {
		if session, ok := sessions[id]; !ok || session.Archived {
			t.Fatalf("user session %s missing or hidden: %#v", id, session)
		}
	}
	if sessions["cli"].ID != old["cli"].ID {
		t.Fatal("existing user identity changed")
	}
	for _, id := range []string{"old-helper", "loaded-helper", "missing", "outside"} {
		if session := sessions[id]; !session.Archived || session.ID != old[id].ID || session.Loaded || session.ActiveTurnID != "" {
			t.Fatalf("obsolete session %s was not retained and hidden: %#v", id, session)
		}
		if a.actorForThread(runtime.ID, id) != nil {
			t.Fatalf("hidden session %s acquired an actor", id)
		}
	}
	for _, id := range []string{"new-helper", "ephemeral"} {
		if _, exists := sessions[id]; exists {
			t.Fatalf("internal session %s registered", id)
		}
	}
	assertResumeCalls(t, server.Calls(), 1)
	for _, call := range server.Calls() {
		if call.Method == "thread/archive" || call.Method == "thread/delete" {
			t.Fatal("inventory cleanup changed Codex history")
		}
	}
	// Update safety must still inspect hidden helpers' actual activity.
	if err := a.manager.verifyUpdateIdle(context.Background()); err == nil {
		t.Fatal("active helper no longer blocked maintenance")
	}
	// Reappearance restores the original identity and publishes visibility.
	server.SetThreads([]map[string]any{{"id": "missing", "cwd": runtime.DefaultCWD, "source": "cli"}}, nil)
	if err := a.manager.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	restored := inventoryByThread(t, a.store, runtime)["missing"]
	if restored.Archived || restored.ID != old["missing"].ID {
		t.Fatalf("reappearing user session was not restored: %#v", restored)
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	var restoredEvent bool
	for _, event := range events {
		var session protocol.Session
		if event.SessionID == restored.ID && event.Kind == "session_state_changed" && json.Unmarshal(event.Data, &session) == nil && !session.Archived {
			restoredEvent = true
		}
	}
	if !restoredEvent {
		t.Fatal("restored visibility was not queued for gateway")
	}
}

func TestDiscoveryDoesNotHideSessionsAfterFailedOrIncompleteScan(t *testing.T) {
	for _, failure := range []string{"thread/list", "thread/loaded/list", "thread/read", "limit", "loaded_limit"} {
		t.Run(failure, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			prior := installColdSession(a, runtime, "existing")
			capped := failure == "limit" || failure == "loaded_limit"
			if failure == "loaded_limit" {
				loaded := make([]string, discoveryLimit+1)
				for i := range loaded {
					loaded[i] = fmt.Sprintf("loaded-%d", i)
				}
				server.SetThreads(nil, loaded)
			} else if failure == "limit" {
				threads := make([]map[string]any, discoveryLimit+1)
				outside := t.TempDir()
				for i := range threads {
					threads[i] = map[string]any{"id": "other", "cwd": outside, "source": "cli"}
				}
				server.SetThreads(threads, nil)
			} else {
				server.SetThreads(nil, []string{"unlisted-loaded"})
				server.SetRPCError(failure, -32000, "temporary failure")
			}
			client, _, _ := a.manager.Client(runtime.ID)
			err := a.manager.discover(context.Background(), runtime, client)
			if !capped && err == nil {
				t.Fatal("discovery failure was swallowed")
			}
			if capped && err != nil {
				t.Fatal(err)
			}
			if session := inventoryByThread(t, a.store, runtime)[prior.ThreadID]; session.Archived {
				t.Fatal("partial or failed scan hid an existing session")
			}
		})
	}
}

func TestDiscoveryDoesNotHideSessionCreatedDuringScan(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetResponseDelay("thread/loaded/list", 100*time.Millisecond)
	client, _, _ := a.manager.Client(runtime.ID)
	done := make(chan error, 1)
	go func() { done <- a.manager.discover(context.Background(), runtime, client) }()
	waitFor(t, func() bool { return hasCall(server.Calls(), "thread/loaded/list") })
	created := installSession(a, runtime, "just-created", "")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if session := inventoryByThread(t, a.store, runtime)[created.ThreadID]; session.Archived {
		t.Fatal("session created during scan was hidden")
	}
}

func TestThreadEventsExcludeHelperAndEphemeralSessions(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	for _, thread := range []codexadapter.Thread{
		{ID: "helper", Source: "subAgent"},
		{ID: "ephemeral", Source: "appServer", Ephemeral: true},
		{ID: "user", Source: "appServer"},
	} {
		thread.CWD = runtime.DefaultCWD
		a.onEvent(runtime, codexadapter.Event{Thread: &thread, ThreadID: thread.ID, Kind: "thread_started"})
	}
	sessions := inventoryByThread(t, a.store, runtime)
	if len(sessions) != 1 || sessions["user"].ID == "" {
		t.Fatalf("thread notifications registered internal sessions: %#v", sessions)
	}
}

func TestArchivedActorRejectsCommandsAndPreservesLiveTurn(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "old-helper", "active-turn")
	queued := agentCommand(runtime, session, protocol.StartTurn)
	if ack, err := a.HandleCommand(context.Background(), queued); err != nil || ack.Status != "accepted" {
		t.Fatalf("queue command: %#v %v", ack, err)
	}
	archived := session
	archived.Archived, archived.Loaded, archived.ActiveTurnID, archived.State = true, false, "", "not_loaded"
	a.onSession(runtime, archived)
	current := inventoryByThread(t, a.store, runtime)[session.ThreadID]
	if !current.Archived || current.ActiveTurnID != "active-turn" {
		t.Fatalf("hiding an actor lost visibility or its live turn: %#v", current)
	}
	command := agentCommand(runtime, session, protocol.StartTurn)
	ack, err := a.HandleCommand(context.Background(), command)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.UnknownSession {
		t.Fatalf("hidden actor accepted command: %#v %v", ack, err)
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: "active-turn"})
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(queued.ID)
		return err == nil && found && record.State == CommandFailed
	})
	if hasCall(server.Calls(), "turn/start") {
		t.Fatal("previously queued command ran on a hidden actor")
	}
}
