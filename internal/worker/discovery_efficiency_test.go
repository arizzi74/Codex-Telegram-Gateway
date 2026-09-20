package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestDiscoveryDoesNotRefetchLoadedOrInternalHistoryEveryTick(t *testing.T) {
	m, runtime, client, fixture, root := recoveryFixture(t)
	user := map[string]any{"id": "loaded", "cwd": root, "status": "active", "turns": []map[string]any{{"id": "already-running", "status": "inProgress"}}}
	threads := []map[string]any{user}
	loaded := []string{"loaded"}
	for i := range 150 {
		id := fmt.Sprintf("internal-%d", i)
		threads = append(threads, map[string]any{"id": id, "cwd": root, "status": "idle", "parentThreadId": "loaded"})
		loaded = append(loaded, id)
	}
	fixture.SetThreads(threads, loaded)
	if err := fixture.SetMethodResult("thread/list", map[string]any{"data": []map[string]any{user}}); err != nil {
		t.Fatal(err)
	}
	var snapshot protocol.Session
	m.hooks.OnSession = func(_ protocol.Runtime, s protocol.Session) { snapshot = s }
	if err := m.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveTurnID != "already-running" {
		t.Fatalf("initial turn missing: %+v", snapshot)
	}
	before := len(fixture.Calls())
	for range 3 {
		if err := m.discover(context.Background(), runtime, client); err != nil {
			t.Fatal(err)
		}
		if snapshot.ActiveTurnID != "already-running" {
			t.Fatalf("turn lost without start replay: %+v", snapshot)
		}
	}
	for _, call := range fixture.Calls()[before:] {
		if call.Method == "thread/read" || call.Method == "thread/turns/list" || call.Method == "thread/resume" {
			t.Fatalf("unchanged inventory opened history again: %s", call.Method)
		}
	}
	sessions, err := m.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("internal threads leaked into inventory: %d %v", len(sessions), err)
	}
	t.Log("151 loaded threads: subsequent inventory cycles made zero history/read/resume RPCs")
}

func TestThreadStatusCannotFinishTurnBeforeFinalMessage(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "status-notifications", "")
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	actor.event(codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "running"})
	actor.event(codexadapter.Event{Kind: "thread_status_changed", ThreadID: session.ThreadID, State: "idle"})
	if actor.session.ActiveTurnID != "running" || actor.session.State != "running" {
		t.Fatal("status ended the turn early")
	}
	actor.event(codexadapter.Event{Kind: "agent_message_completed", ThreadID: session.ThreadID, TurnID: "running", ItemID: "final", Phase: "final_answer", Text: "The final answer"})
	actor.event(codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: "running"})
	if actor.session.ActiveTurnID != "" || actor.session.State != "idle" {
		t.Fatal("actual completion did not end turn")
	}
	actor.event(codexadapter.Event{Kind: "thread_status_changed", ThreadID: session.ThreadID, State: "active"})
	if actor.session.State != "running" {
		t.Fatal("active notification ignored")
	}
	actor.event(codexadapter.Event{Kind: "thread_status_changed", ThreadID: session.ThreadID, State: "idle"})
	if actor.session.State != "idle" {
		t.Fatal("idle notification without known turn ignored")
	}
}
