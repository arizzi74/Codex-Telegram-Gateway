package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
)

func updateFailedSubAgentFixture(threadID, parentID, turnID string) map[string]any {
	return map[string]any{
		"id": threadID, "status": map[string]any{"type": "systemError"},
		"source": map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{
			"parent_thread_id": parentID, "depth": 1, "agent_path": "/root/helper", "agent_nickname": "Helper",
		}}},
		"turns": []map[string]any{{"id": turnID, "status": "failed", "startedAt": 1700000000, "completedAt": 1700000010}},
	}
}

func updateFailedSubAgentParentFixture() map[string]any {
	return map[string]any{"id": "parent", "status": "idle", "source": "cli"}
}

func updateFailedSubAgentEmptyQueues(t *testing.T, server *codextest.Server) {
	t.Helper()
	if err := server.SetMethodResult("thread/queue/list", map[string]any{"data": []any{}}); err != nil {
		t.Fatal(err)
	}
	if err := server.SetMethodResult("thread/backgroundTerminals/list", map[string]any{"data": []any{}}); err != nil {
		t.Fatal(err)
	}
}

func updateFailedSubAgentCallCount(t *testing.T, calls []codextest.Call, method, threadID string) int {
	t.Helper()
	count := 0
	for _, call := range calls {
		if call.Method != method {
			continue
		}
		var params struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.ThreadID == threadID {
			count++
		}
	}
	return count
}

func TestUpdatePreparationAcceptsTerminalFailedSubAgentWithoutMutatingHistory(t *testing.T) {
	a, _, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{updateFailedSubAgentParentFixture(), updateFailedSubAgentFixture("child", "parent", "failed-turn")}, []string{"child", "parent"})
	updateFailedSubAgentEmptyQueues(t, server)
	lease, err := a.prepareUpdate(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("fully verified failed child blocked preparation: %v", err)
	}
	if err := a.abortUpdate(lease.Token); err != nil {
		t.Fatal(err)
	}
	calls := server.Calls()
	if updateFailedSubAgentCallCount(t, calls, "thread/read", "child") < 2 || updateFailedSubAgentCallCount(t, calls, "thread/read", "parent") == 0 ||
		updateFailedSubAgentCallCount(t, calls, "thread/queue/list", "child") == 0 || updateFailedSubAgentCallCount(t, calls, "thread/queue/list", "parent") == 0 {
		t.Fatal("preparation omitted the child's fresh recheck or parent/child queue verification")
	}
	for _, call := range calls {
		switch call.Method {
		case "thread/archive", "thread/delete", "thread/unsubscribe", "turn/interrupt", "turn/start", "thread/resume", "thread/backgroundTerminals/clean", "thread/backgroundTerminals/terminate":
			t.Fatalf("idle verification mutated the failed child: %s", call.Method)
		case "thread/read", "thread/turns/list", "thread/queue/list":
			var params map[string]any
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			if call.Method == "thread/read" && params["includeTurns"] != false {
				t.Fatal("preparation fetched unbounded history")
			}
			if call.Method == "thread/turns/list" && (params["limit"] != float64(1) || params["sortDirection"] != "desc" || params["itemsView"] != "notLoaded") {
				t.Fatal("preparation fetched turn items or an unbounded turn page")
			}
			if call.Method == "thread/queue/list" && params["limit"] != float64(1) {
				t.Fatal("preparation fetched an unbounded queued prompt page")
			}
		}
	}
}

func TestUpdateFailedSubAgentRequiresIdleParentAndDescendants(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parent     map[string]any
		descendant map[string]any
	}{
		{name: "active parent outside inventory", parent: map[string]any{"id": "parent", "status": "active", "source": "cli", "turns": []map[string]any{{"id": "parent-turn", "status": "inProgress"}}}},
		{name: "parent active turn despite idle metadata", parent: map[string]any{"id": "parent", "status": "idle", "source": "cli", "turns": []map[string]any{{"id": "parent-turn", "status": "inProgress"}}}},
		{name: "active descendant", descendant: map[string]any{"id": "descendant", "parentThreadId": "child", "status": "active", "turns": []map[string]any{{"id": "descendant-turn", "status": "inProgress"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, server, cleanup := testAgent(t)
			defer cleanup()
			parent := tc.parent
			if parent == nil {
				parent = updateFailedSubAgentParentFixture()
			}
			threads := []map[string]any{parent, updateFailedSubAgentFixture("child", "parent", "failed-turn")}
			loaded := []string{"child"}
			if tc.descendant != nil {
				threads = append(threads, tc.descendant)
				loaded = append(loaded, "descendant")
			}
			server.SetThreads(threads, loaded)
			updateFailedSubAgentEmptyQueues(t, server)
			if err := a.manager.verifyUpdateIdle(context.Background()); err == nil {
				t.Fatal("failed child hid active work in its family")
			}
		})
	}
}

func TestUpdateFailedSubAgentRequiresVerifiableEmptyQueues(t *testing.T) {
	for _, state := range []string{"queued", "unavailable", "malformed", "continuation"} {
		t.Run(state, func(t *testing.T) {
			a, _, server, cleanup := testAgent(t)
			defer cleanup()
			server.SetThreads([]map[string]any{updateFailedSubAgentParentFixture(), updateFailedSubAgentFixture("child", "parent", "failed-turn")}, []string{"child"})
			result := map[string]any{"data": []any{}}
			switch state {
			case "queued":
				result["data"] = []map[string]any{{"id": "queued", "text": "PRIVATE_QUEUED_PROMPT"}}
			case "unavailable":
				server.SetMethodUnavailable("thread/queue/list", true)
			case "malformed":
				delete(result, "data")
			case "continuation":
				result["nextCursor"] = "remaining-inputs"
			}
			if err := server.SetMethodResult("thread/queue/list", result); err != nil {
				t.Fatal(err)
			}
			err := a.manager.verifyUpdateIdle(context.Background())
			if err == nil || !strings.Contains(err.Error(), "queued work") || strings.Contains(err.Error(), "PRIVATE_QUEUED_PROMPT") {
				t.Fatalf("unsafe or unverifiable queue allowed update or leaked text: %v", err)
			}
		})
	}
}

func TestUpdateFailedSubAgentRejectsUserUnknownAndInvalidChildEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"user system error", func(thread map[string]any) { thread["source"] = "cli" }},
		{"unknown system error", func(thread map[string]any) { thread["source"] = "unknown" }},
		{"legacy helper classification", func(thread map[string]any) { thread["source"] = "subAgent" }},
		{"review helper", func(thread map[string]any) { thread["source"] = map[string]any{"subAgent": "review"} }},
		{"conflicting ancestry", func(thread map[string]any) { thread["parentThreadId"] = "other" }},
		{"self ancestry", func(thread map[string]any) {
			thread["source"] = map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "child", "depth": 1}}}
		}},
		{"missing completion", func(thread map[string]any) { delete(thread["turns"].([]map[string]any)[0], "completedAt") }},
		{"running latest turn", func(thread map[string]any) { thread["turns"].([]map[string]any)[0]["status"] = "inProgress" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, server, cleanup := testAgent(t)
			defer cleanup()
			child := updateFailedSubAgentFixture("child", "parent", "failed-turn")
			tc.change(child)
			server.SetThreads([]map[string]any{updateFailedSubAgentParentFixture(), child}, []string{"child"})
			updateFailedSubAgentEmptyQueues(t, server)
			if err := a.manager.verifyUpdateIdle(context.Background()); err == nil {
				t.Fatal("nonterminal or unverified internal identity allowed update")
			}
		})
	}
}

func TestUpdateFailedSubAgentVerifiesCandidatesOutsideLoadedInventory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected string
		present  bool
		wantErr  bool
	}{
		{name: "exact terminal snapshot", expected: "failed-turn", present: true},
		{name: "status marker", present: true},
		{name: "snapshot turn mismatch", expected: "other-turn", present: true, wantErr: true},
		{name: "missing candidate metadata", expected: "failed-turn", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			threads := []map[string]any{updateFailedSubAgentParentFixture()}
			if tc.present {
				threads = append(threads, updateFailedSubAgentFixture("child", "parent", "failed-turn"))
			}
			server.SetThreads(threads, nil)
			updateFailedSubAgentEmptyQueues(t, server)
			proofs, err := a.manager.verifyUpdateIdleWithRecovery(context.Background(), nil, map[string]map[string]string{runtime.ID: {"child": tc.expected}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("candidate verification = %#v, %v; want error %t", proofs, err, tc.wantErr)
			}
			if tc.wantErr {
				if proofs != nil {
					t.Fatal("failed candidate verification returned trusted proof")
				}
				return
			}
			if proofs[runtime.ID]["child"] != "failed-turn" || updateFailedSubAgentCallCount(t, server.Calls(), "thread/read", "child") < 2 ||
				updateFailedSubAgentCallCount(t, server.Calls(), "thread/queue/list", "parent") == 0 {
				t.Fatalf("candidate outside inventory was not independently verified: %#v", proofs)
			}
		})
	}
}

func TestUpdateFailedSubAgentRejectsChangedLatestTurnDuringVerification(t *testing.T) {
	a, _, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{updateFailedSubAgentParentFixture(), updateFailedSubAgentFixture("child", "parent", "failed-turn")}, []string{"child"})
	updateFailedSubAgentEmptyQueues(t, server)
	server.SetResponseDelay("thread/queue/list", 50*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- a.manager.verifyUpdateIdle(context.Background()) }()
	waitFor(t, func() bool {
		return updateFailedSubAgentCallCount(t, server.Calls(), "thread/queue/list", "child") != 0
	})
	server.SetThreads([]map[string]any{updateFailedSubAgentParentFixture(), updateFailedSubAgentFixture("child", "parent", "another-failed-turn")}, []string{"child"})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "changed during idle verification") {
		t.Fatalf("latest-turn change was accepted: %v", err)
	}
}

func TestUpdateFailedSubAgentRejectsBackgroundTerminalsAcrossFamily(t *testing.T) {
	for _, target := range []string{"child", "parent", "descendant", "unavailable"} {
		t.Run(target, func(t *testing.T) {
			a, _, server, cleanup := testAgent(t)
			defer cleanup()
			server.SetThreads([]map[string]any{
				updateFailedSubAgentParentFixture(), updateFailedSubAgentFixture("child", "parent", "failed-turn"),
				{"id": "descendant", "parentThreadId": "child", "status": "idle"},
			}, []string{"child", "descendant"})
			updateFailedSubAgentEmptyQueues(t, server)
			if target == "unavailable" {
				server.SetMethodUnavailable("thread/backgroundTerminals/list", true)
			} else if err := server.SetThreadMethodResult("thread/backgroundTerminals/list", target, map[string]any{
				"data": []map[string]any{{"processId": "process", "itemId": "item", "command": "PRIVATE_COMMAND", "cwd": "PRIVATE_CWD"}},
			}); err != nil {
				t.Fatal(err)
			}
			err := a.manager.verifyUpdateIdle(context.Background())
			if err == nil || !strings.Contains(err.Error(), "background terminals") || strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("active or unverifiable terminal allowed recovery or leaked command text: %v", err)
			}
			if target != "unavailable" && updateFailedSubAgentCallCount(t, server.Calls(), "thread/backgroundTerminals/list", target) == 0 {
				t.Fatal("recovery did not verify the family member's background terminals")
			}
		})
	}
}

func TestUpdateFailedSubAgentRejectsQueuedIdleDescendant(t *testing.T) {
	a, _, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{
		updateFailedSubAgentParentFixture(), updateFailedSubAgentFixture("child", "parent", "failed-turn"),
		{"id": "descendant", "parentThreadId": "child", "status": "idle"},
	}, []string{"child", "descendant"})
	updateFailedSubAgentEmptyQueues(t, server)
	if err := server.SetThreadMethodResult("thread/queue/list", "descendant", map[string]any{
		"data": []map[string]any{{"id": "queued", "text": "PRIVATE_DESCENDANT_PROMPT"}},
	}); err != nil {
		t.Fatal(err)
	}
	err := a.manager.verifyUpdateIdle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "queued work") || strings.Contains(err.Error(), "PRIVATE_") {
		t.Fatalf("idle descendant hid queued work: %v", err)
	}
	if updateFailedSubAgentCallCount(t, server.Calls(), "thread/queue/list", "descendant") == 0 {
		t.Fatal("recovery skipped the idle descendant's queue")
	}
}
