package codexadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const failedSpawnMetadata = `{"id":"child","parentThreadId":"parent","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1,"agent_path":"/root/helper","agent_nickname":"Helper","agent_role":"reviewer"}}},"threadSource":"subagent","status":{"type":"systemError"}}`

func failedSpawn(t *testing.T) Thread {
	t.Helper()
	thread, err := decodeThread(json.RawMessage(failedSpawnMetadata))
	if err != nil {
		t.Fatal(err)
	}
	start, completion := int64(1700000000), int64(1700000010)
	thread.LatestTurn = &ThreadTurnState{ID: "failed-turn", Status: "failed", StartedAt: &start, CompletedAt: &completion}
	return thread
}

func TestReadThreadStateRetainsBoundedFailedSubAgentEvidence(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type outcome struct {
		thread Thread
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		thread, err := client.ReadThreadState(context.Background(), "child")
		done <- outcome{thread, err}
	}()
	metadata := fake.next(t)
	if method(t, metadata) != "thread/read" || string(params(t, metadata)["includeTurns"]) != "false" {
		t.Fatal("state read requested thread history")
	}
	fake.respond(t, metadata, json.RawMessage(`{"thread":`+failedSpawnMetadata+`}`))
	latest := fake.next(t)
	p := params(t, latest)
	if method(t, latest) != "thread/turns/list" || string(p["limit"]) != "1" || string(p["sortDirection"]) != `"desc"` || string(p["itemsView"]) != `"notLoaded"` {
		t.Fatalf("state read did not request one latest turn without items: %v", p)
	}
	fake.respond(t, latest, json.RawMessage(`{"data":[{"id":"failed-turn","status":"failed","startedAt":1700000000,"completedAt":1700000010,"itemsView":"notLoaded","items":[],"error":{"message":"private server error","codexErrorInfo":"serverOverloaded"}}],"nextCursor":"older-turns"}`))
	got := <-done
	if got.err != nil || !got.thread.TerminalFailedSubAgent() || got.thread.ActiveTurnID != "" {
		t.Fatalf("failed child state = %#v, %v", got.thread, got.err)
	}
	child, turn := got.thread.SubAgent, got.thread.LatestTurn
	if child.ParentThreadID != "parent" || child.AgentPath != "/root/helper" || child.AgentNickname != "Helper" || child.AgentRole != "reviewer" ||
		turn.ID != "failed-turn" || turn.Status != "failed" || *turn.StartedAt != 1700000000 || *turn.CompletedAt != 1700000010 {
		t.Fatalf("lost typed child or terminal evidence: %#v, %#v", child, turn)
	}
	encoded, err := json.Marshal(got.thread)
	if err != nil || strings.Contains(string(encoded), "private server error") || strings.Contains(string(encoded), "serverOverloaded") || strings.Contains(string(encoded), "older-turns") {
		t.Fatalf("state retained turn response bodies: %s, %v", encoded, err)
	}
}

func TestTerminalFailedSubAgentFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Thread)
	}{
		{"missing thread identity", func(t *Thread) { t.ID = "" }},
		{"user source", func(t *Thread) { t.Source = "cli" }},
		{"unknown source", func(t *Thread) { t.Source = "unknown" }},
		{"missing child identity", func(t *Thread) { t.SubAgent = nil }},
		{"review helper", func(t *Thread) { t.SubAgent.Kind = "review" }},
		{"unknown helper", func(t *Thread) { t.SubAgent.Kind = "future" }},
		{"missing ancestry", func(t *Thread) { t.SubAgent.ParentThreadID = "" }},
		{"self ancestry", func(t *Thread) { t.SubAgent.ParentThreadID, t.ParentThreadID = t.ID, t.ID }},
		{"conflicting ancestry", func(t *Thread) { t.ParentThreadID = "other-parent" }},
		{"missing depth", func(t *Thread) { t.SubAgent.Depth = 0 }},
		{"negative depth", func(t *Thread) { t.SubAgent.Depth = -1 }},
		{"conflicting source classification", func(t *Thread) { t.ThreadSource = "user" }},
		{"active metadata", func(t *Thread) { t.Status = "active" }},
		{"idle metadata", func(t *Thread) { t.Status = "idle" }},
		{"unloaded metadata", func(t *Thread) { t.Status = "notLoaded" }},
		{"unknown metadata", func(t *Thread) { t.Status = "future" }},
		{"active turn", func(t *Thread) { t.ActiveTurnID = "another-turn" }},
		{"missing latest turn", func(t *Thread) { t.LatestTurn = nil }},
		{"missing latest identity", func(t *Thread) { t.LatestTurn.ID = "" }},
		{"blank latest identity", func(t *Thread) { t.LatestTurn.ID = " " }},
		{"latest turn running", func(t *Thread) { t.LatestTurn.Status = "inProgress" }},
		{"latest turn completed", func(t *Thread) { t.LatestTurn.Status = "completed" }},
		{"latest turn interrupted", func(t *Thread) { t.LatestTurn.Status = "interrupted" }},
		{"latest turn unknown", func(t *Thread) { t.LatestTurn.Status = "future" }},
		{"missing start", func(t *Thread) { t.LatestTurn.StartedAt = nil }},
		{"missing completion", func(t *Thread) { t.LatestTurn.CompletedAt = nil }},
		{"zero start", func(t *Thread) { *t.LatestTurn.StartedAt = 0 }},
		{"negative start", func(t *Thread) { *t.LatestTurn.StartedAt = -1 }},
		{"zero completion", func(t *Thread) { *t.LatestTurn.CompletedAt = 0 }},
		{"completion before start", func(t *Thread) { *t.LatestTurn.CompletedAt = *t.LatestTurn.StartedAt - 1 }},
		{"future completion", func(t *Thread) { *t.LatestTurn.CompletedAt = time.Now().Add(time.Hour).Unix() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread := failedSpawn(t)
			if !thread.TerminalFailedSubAgent() {
				t.Fatal("valid failed fixture did not establish terminal evidence")
			}
			tc.change(&thread)
			if thread.TerminalFailedSubAgent() {
				t.Fatal("incomplete or conflicting evidence identified a terminal failed child")
			}
		})
	}
}

func TestTerminalFailedSubAgentUsesSourceAncestryFallback(t *testing.T) {
	metadata := strings.Replace(failedSpawnMetadata, `"parentThreadId":"parent",`, "", 1)
	metadata = strings.Replace(metadata, `"threadSource":"subagent",`, "", 1)
	thread, err := decodeThread(json.RawMessage(metadata))
	if err != nil {
		t.Fatal(err)
	}
	thread.LatestTurn = failedSpawn(t).LatestTurn
	if thread.ParentThreadID != "parent" || !thread.TerminalFailedSubAgent() {
		t.Fatalf("explicit source ancestry did not supply legacy child evidence: %#v", thread)
	}
}

func TestSubAgentRecoveryIdentityRejectsAmbiguousSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
	}{
		{"legacy helper string", `"source":"subAgent"`},
		{"ancestry only", `"parentThreadId":"parent"`},
		{"source classification only", `"threadSource":"subagent"`},
		{"duplicate source", `"source":"cli","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"duplicate metadata status", `"status":{"type":"active","type":"systemError"},"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"whitespace duplicate metadata status", `"status":  {"type":"active","type":"systemError"},"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"duplicate subagent", `"source":{"subAgent":"review","subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"conflicting source variants", `"source":{"custom":"user","subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"duplicate spawn variant", `"source":{"subAgent":{"thread_spawn":{},"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"conflicting helper variants", `"source":{"subAgent":{"other":"user","thread_spawn":{"parent_thread_id":"parent","depth":1}}}`},
		{"duplicate parent", `"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"other","parent_thread_id":"parent","depth":1}}}`},
		{"missing parent", `"source":{"subAgent":{"thread_spawn":{"depth":1}}}`},
		{"null parent", `"source":{"subAgent":{"thread_spawn":{"parent_thread_id":null,"depth":1}}}`},
		{"missing depth", `"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent"}}}`},
		{"null depth", `"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":null}}}`},
		{"invalid depth", `"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":0}}}`},
		{"invalid spawn type", `"source":{"subAgent":{"thread_spawn":"parent"}}`},
		{"unknown helper", `"source":{"subAgent":{"future":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thread, err := decodeThread(json.RawMessage(`{"id":"child",` + tc.fields + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if thread.SubAgent != nil {
				t.Fatalf("ambiguous source supplied recovery identity: %#v", thread.SubAgent)
			}
		})
	}
}

func TestReadThreadStateRejectsAmbiguousTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		page string
	}{
		{"duplicate data", `{"data":[{"id":"live","status":"inProgress"}],"data":[{"id":"failed","status":"failed"}]}`},
		{"duplicate status", `{"data":[{"id":"turn","status":"inProgress","status":"failed"}]}`},
		{"duplicate identity", `{"data":[{"id":"live","id":"failed","status":"failed"}]}`},
		{"duplicate completion", `{"data":[{"id":"failed","status":"failed","completedAt":null,"completedAt":1700000010}]}`},
		{"invalid timestamp type", `{"data":[{"id":"failed","status":"failed","completedAt":"1700000010"}]}`},
		{"null turn", `{"data":[null]}`},
		{"blank identity", `{"data":[{"id":" ","status":"failed"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() { _, err := client.ReadThreadState(context.Background(), "child"); done <- err }()
			metadata := fake.next(t)
			fake.respond(t, metadata, json.RawMessage(`{"thread":`+failedSpawnMetadata+`}`))
			latest := fake.next(t)
			fake.respond(t, latest, json.RawMessage(tc.page))
			if err := <-done; err == nil {
				t.Fatal("ambiguous terminal evidence was accepted")
			}
		})
	}
}

func TestReadThreadStateMetadataOnlyFallbackCannotProveFailedSubAgent(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type outcome struct {
		thread Thread
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		thread, err := client.ReadThreadState(context.Background(), "child")
		done <- outcome{thread, err}
	}()
	metadata := fake.next(t)
	fake.respond(t, metadata, json.RawMessage(`{"thread":`+failedSpawnMetadata+`}`))
	latest := fake.next(t)
	fake.write(t, map[string]any{"id": json.RawMessage(latest["id"]), "error": map[string]any{"code": -32600, "message": "thread child is not materialized yet; thread/turns/list is unavailable before first user message"}})
	fresh := fake.next(t)
	fake.respond(t, fresh, json.RawMessage(`{"thread":`+failedSpawnMetadata+`}`))
	got := <-done
	if got.err != nil || got.thread.SubAgent == nil || got.thread.LatestTurn != nil || got.thread.TerminalFailedSubAgent() {
		t.Fatalf("metadata-only fallback supplied terminal-turn proof: %#v, %v", got.thread, got.err)
	}
}
