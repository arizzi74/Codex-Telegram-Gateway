package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func deletionFixture(t *testing.T) (*sessionActor, *codexadapter.Client, *codextest.Server, protocol.Command) {
	t.Helper()
	a, runtime, server, cleanup := testAgent(t)
	t.Cleanup(cleanup)
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "delete-selected", CWD: runtime.DefaultCWD, State: "not_loaded"})
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}}, nil)
	client, _, _ := a.manager.Client(runtime.ID)
	actor := &sessionActor{agent: a, runtime: runtime, session: session, pending: map[string]pendingRequest{}}
	return actor, client, server, agentCommand(runtime, session, protocol.Operation("delete_session"))
}

func TestDeleteSessionKeepsWorkingDirectoryAndPermanentlyRetiresIdentity(t *testing.T) {
	actor, client, server, command := deletionFixture(t)
	before := actor.session
	file := filepath.Join(before.CWD, "keep.txt")
	if err := os.WriteFile(file, []byte("project contents"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := actor.deleteSession(context.Background(), client, command)
	if err != nil || result.Session == nil || !result.Session.Deleted || !result.Session.Archived {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if countCall(server.Calls(), "thread/delete") != 1 || hasCall(server.Calls(), "thread/archive") || hasCall(server.Calls(), "thread/resume") {
		t.Fatalf("unexpected RPCs = %#v", server.Calls())
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "project contents" {
		t.Fatalf("workspace modified: %q, %v", data, err)
	}
	// A discovery scan that started before deletion must not resurrect this ID.
	restored, err := actor.agent.store.UpsertSession(before)
	if err != nil || !restored.Deleted || !restored.Archived {
		t.Fatalf("stale upsert restored deletion: %#v, %v", restored, err)
	}
	candidate := before
	_, changed, err := actor.agent.store.RestoreDiscoveredSession(actor.runtime, restored, candidate)
	if err != nil || changed {
		t.Fatalf("discovery restore changed permanent deletion: %v, %v", changed, err)
	}
	if !updateSessionIdle(restored) {
		t.Fatal("deleted session blocks idle auto updates")
	}
	actor.deleted() // Duplicate RPC notification does not append another event.
	events, err := actor.agent.store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "session_state_changed" {
		t.Fatalf("permanent deletion events = %#v, %v", events, err)
	}
}

func TestDeleteSessionRefusesBusyOrChangedTargets(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*sessionActor, *codextest.Server, *protocol.Command)
		code   string
	}{
		{"local turn", func(a *sessionActor, _ *codextest.Server, _ *protocol.Command) { a.session.ActiveTurnID = "running" }, protocol.SessionBusy},
		{"queued turn", func(a *sessionActor, _ *codextest.Server, c *protocol.Command) { a.queue = []protocol.Command{*c} }, protocol.SessionBusy},
		{"pending input", func(a *sessionActor, _ *codextest.Server, _ *protocol.Command) { a.pending["input"] = pendingRequest{} }, protocol.SessionBusy},
		{"remote turn", func(a *sessionActor, s *codextest.Server, _ *protocol.Command) {
			s.SetThreads([]map[string]any{{"id": a.session.ThreadID, "cwd": a.session.CWD, "status": "active", "turns": []map[string]any{{"id": "active-turn", "status": "inProgress"}}}}, nil)
		}, protocol.SessionBusy},
		{"unrecognized remote state", func(a *sessionActor, s *codextest.Server, _ *protocol.Command) {
			s.SetThreads([]map[string]any{{"id": a.session.ThreadID, "cwd": a.session.CWD, "status": "unknown"}}, nil)
		}, protocol.SessionBusy},
		{"changed directory", func(a *sessionActor, s *codextest.Server, _ *protocol.Command) {
			s.SetThreads([]map[string]any{{"id": a.session.ThreadID, "cwd": t.TempDir(), "status": "idle"}}, nil)
		}, protocol.InvalidWorkspace},
		{"changed thread", func(_ *sessionActor, _ *codextest.Server, c *protocol.Command) { c.ThreadID = "different" }, protocol.UnknownSession},
		{"stale runtime", func(_ *sessionActor, _ *codextest.Server, c *protocol.Command) { c.RuntimeGeneration++ }, protocol.StaleRuntime},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, client, server, command := deletionFixture(t)
			test.change(actor, server, &command)
			_, err := actor.deleteSession(context.Background(), client, command)
			var refusal *protocol.Error
			if !errors.As(err, &refusal) || refusal.Code != test.code {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
			if hasCall(server.Calls(), "thread/delete") {
				t.Fatal("rejected target reached permanent deletion RPC")
			}
		})
	}
}

func TestDeleteSessionFailureDoesNotHideSession(t *testing.T) {
	actor, client, server, command := deletionFixture(t)
	server.SetRPCError("thread/delete", -32000, "cannot delete this thread")
	if _, err := actor.deleteSession(context.Background(), client, command); err == nil {
		t.Fatal("delete failure reported success")
	}
	sessions, err := actor.agent.store.ListSessions(actor.runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].Archived || sessions[0].Deleted {
		t.Fatalf("failed deletion retired session: %#v, %v", sessions, err)
	}
	events, err := actor.agent.store.OutboxAfter(0)
	if err != nil || len(events) != 0 {
		t.Fatalf("failure published deletion: %#v, %v", events, err)
	}
}

func TestDeleteEmptySessionAllowsOnlyExplicitUnmaterializedTurnHistory(t *testing.T) {
	for _, test := range []struct {
		name, message string
		allowed       bool
	}{
		{"no first message", "thread delete-selected is not materialized yet; thread/turns/list is unavailable before first user message", true},
		{"different thread", "thread other is not materialized yet; thread/turns/list is unavailable before first user message", false},
		{"different failure", "turn history unavailable due to read failure", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, client, server, command := deletionFixture(t)
			server.SetRPCError("thread/turns/list", -32600, test.message)
			_, err := actor.deleteSession(context.Background(), client, command)
			if test.allowed {
				if err != nil || countCall(server.Calls(), "thread/delete") != 1 {
					t.Fatalf("empty session refused: %v", err)
				}
			} else if err == nil || hasCall(server.Calls(), "thread/delete") {
				t.Fatalf("unconfirmed state allowed deletion: %v", err)
			}
		})
	}
}

func TestExternalDeletedNotificationCannotBeUndoneByQueuedEvents(t *testing.T) {
	actor, _, _, _ := deletionFixture(t)
	actor.event(codexadapter.Event{Kind: "thread_deleted", ThreadID: actor.session.ThreadID})
	actor.event(codexadapter.Event{Kind: "turn_started", ThreadID: actor.session.ThreadID, TurnID: "stale-turn"})
	if !actor.session.Deleted || !actor.session.Archived || actor.session.ActiveTurnID != "" {
		t.Fatalf("stale event restored external deletion: %#v", actor.session)
	}
}

func TestDeleteSessionCommandRetriesDoNotRepeatRPC(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installColdSession(a, runtime, "delete-through-agent")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "notLoaded"}}, nil)
	command := agentCommand(runtime, session, protocol.DeleteSession)
	command.Arguments = protocol.Arguments{}
	ack, err := a.HandleCommand(context.Background(), command)
	if err != nil || ack.Status != "accepted" {
		t.Fatalf("delete command = %#v, %v", ack, err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	ack, err = a.HandleCommand(context.Background(), command)
	if err != nil || ack.Status != "duplicate" || countCall(server.Calls(), "thread/delete") != 1 {
		t.Fatalf("duplicate deletion = %#v, %v, calls = %#v", ack, err, server.Calls())
	}
	newCommand := agentCommand(runtime, session, protocol.DeleteSession)
	newCommand.Arguments = protocol.Arguments{}
	ack, err = a.HandleCommand(context.Background(), newCommand)
	if err != nil || ack.Error == nil || ack.Error.Code != protocol.UnknownSession || countCall(server.Calls(), "thread/delete") != 1 {
		t.Fatalf("retired-session deletion = %#v, %v", ack, err)
	}
}

func TestExternalDeletionRetiresArchivedSessionWithoutActor(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "archived-delete", CWD: runtime.DefaultCWD, State: "not_loaded", Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "thread_deleted", ThreadID: session.ThreadID})
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || !sessions[0].Deleted || !sessions[0].Archived {
		t.Fatalf("external deletion not persisted: %#v, %v", sessions, err)
	}
}

func TestDeletionChecksSpawnedDescendantsWithoutBlockingUnrelatedThreads(t *testing.T) {
	for _, test := range []struct {
		name, parent string
		busy         bool
	}{
		{"running child", "delete-selected", true},
		{"running grandchild", "middle-child", true},
		{"unrelated running user thread", "", false},
		{"unrelated running child", "unrelated-root", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, client, server, command := deletionFixture(t)
			server.SetThreads([]map[string]any{
				{"id": actor.session.ThreadID, "cwd": actor.session.CWD, "status": "idle"},
				{"id": "busy-entry", "cwd": actor.session.CWD, "status": "active", "parentThreadId": test.parent, "source": "cli", "turns": []map[string]any{{"id": "running-turn", "status": "inProgress"}}},
				{"id": "middle-child", "cwd": actor.session.CWD, "status": "idle", "parentThreadId": actor.session.ThreadID},
				{"id": "unrelated-root", "cwd": actor.session.CWD, "status": "idle", "source": "cli"},
			}, []string{"busy-entry"})
			_, err := actor.deleteSession(context.Background(), client, command)
			if test.busy {
				var busy *protocol.Error
				if !errors.As(err, &busy) || busy.Code != protocol.SessionBusy || hasCall(server.Calls(), "thread/delete") {
					t.Fatalf("running descendant allowed deletion: %v", err)
				}
			} else if err != nil || countCall(server.Calls(), "thread/delete") != 1 {
				t.Fatalf("unrelated running thread prevented deletion: %v", err)
			}
		})
	}
}
