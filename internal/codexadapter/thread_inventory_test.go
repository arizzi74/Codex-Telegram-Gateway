package codexadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestThreadUserSessionClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		source string
		user   bool
	}{
		{name: "legacy missing source", user: true},
		{name: "legacy null source", fields: `,"source":null`, user: true},
		{name: "cli", fields: `,"source":"cli"`, source: "cli", user: true},
		{name: "vscode", fields: `,"source":"vscode"`, source: "vscode", user: true},
		{name: "exec", fields: `,"source":"exec"`, source: "exec", user: true},
		{name: "app server", fields: `,"source":"appServer"`, source: "appServer", user: true},
		{name: "user analytics", fields: `,"source":"cli","threadSource":"user"`, source: "cli", user: true},
		{name: "feature analytics", fields: `,"source":"appServer","threadSource":"automation"`, source: "appServer", user: true},
		{name: "fork is not helper", fields: `,"source":"cli","forkedFromId":"original"`, source: "cli", user: true},
		{name: "ephemeral", fields: `,"source":"cli","ephemeral":true`, source: "cli"},
		{name: "ephemeral legacy", fields: `,"ephemeral":true`},
		{name: "helper string", fields: `,"source":"subAgent"`, source: "subAgent"},
		{name: "helper spawn", fields: `,"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}`, source: "subAgent"},
		{name: "helper review", fields: `,"source":{"subAgent":"review"}`, source: "subAgent"},
		{name: "helper compact", fields: `,"source":{"subAgent":"compact"}`, source: "subAgent"},
		{name: "helper memory", fields: `,"source":{"subAgent":"memory_consolidation"}`, source: "subAgent"},
		{name: "helper other", fields: `,"source":{"subAgent":{"other":"custom-helper"}}`, source: "subAgent"},
		{name: "helper future", fields: `,"source":{"subAgent":{"future_kind":{}}}`, source: "subAgent"},
		{name: "parent metadata", fields: `,"source":"cli","parentThreadId":"parent"`, source: "cli"},
		{name: "helper analytics", fields: `,"source":"appServer","threadSource":"subagent"`, source: "appServer"},
		{name: "internal analytics", fields: `,"threadSource":"memory_consolidation"`},
		{name: "explicit unknown", fields: `,"source":"unknown"`, source: "unknown"},
		{name: "future string", fields: `,"source":"future-source"`, source: "future-source"},
		{name: "custom", fields: `,"source":{"custom":"custom-app"}`, source: "custom"},
		{name: "empty string", fields: `,"source":""`, source: "unknown"},
		{name: "empty object", fields: `,"source":{}`, source: "unknown"},
		{name: "future object", fields: `,"source":{"future":{}}`, source: "unknown"},
		{name: "malformed source", fields: `,"source":42`, source: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(`{"id":"thread","cwd":"/workspace/project"` + tc.fields + `}`)
			thread, err := decodeThread(raw)
			if err != nil {
				t.Fatal(err)
			}
			if thread.Source != tc.source || thread.UserSession() != tc.user {
				t.Fatalf("source = %q, user session = %v; want %q, %v", thread.Source, thread.UserSession(), tc.source, tc.user)
			}
			if string(thread.Raw) != string(raw) {
				t.Fatal("source projection changed the raw protocol payload")
			}
		})
	}
}

func TestThreadStartedPreservesHelperClassification(t *testing.T) {
	event := newEvent("thread/started", json.RawMessage(`{"thread":{"id":"helper","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}},"parentThreadId":"parent","threadSource":"subagent"}}`))
	if event.Thread == nil || event.Thread.Source != "subAgent" || event.Thread.ParentThreadID != "parent" || event.Thread.ThreadSource != "subagent" || event.Thread.UserSession() {
		t.Fatalf("helper notification classification = %#v", event.Thread)
	}
}

func TestSpawnedThreadParentUsesSessionSourceFallback(t *testing.T) {
	thread, err := decodeThread(json.RawMessage(`{"id":"child","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent","depth":1}}}}`))
	if err != nil || thread.ParentThreadID != "parent" {
		t.Fatalf("spawned parent = %q, %v", thread.ParentThreadID, err)
	}
}

func TestListAllThreadsComplete(t *testing.T) {
	type page struct {
		count  int
		cursor string
	}
	for _, tc := range []struct {
		name     string
		max      int
		pages    []page
		count    int
		complete bool
		wantErr  bool
	}{
		{name: "empty complete", max: 1000, pages: []page{{}}, complete: true},
		{name: "multiple complete pages", max: 1000, pages: []page{{2, "next"}, {1, ""}}, count: 3, complete: true},
		{name: "unbounded complete", pages: []page{{2, "next"}, {2, ""}}, count: 4, complete: true},
		{name: "empty intermediate page", max: 1000, pages: []page{{0, "next"}, {1, ""}}, count: 1, complete: true},
		{name: "exact cap complete", max: 1000, pages: []page{{1000, ""}}, count: 1000, complete: true},
		{name: "exact cap incomplete", max: 1000, pages: []page{{1000, "next"}}, count: 1000},
		{name: "oversized final page incomplete", max: 1000, pages: []page{{1001, ""}}, count: 1000},
		{name: "cap across pages", max: 3, pages: []page{{2, "next"}, {2, ""}}, count: 3},
		{name: "repeated cursor", max: 1000, pages: []page{{1, "next"}, {1, "next"}}, wantErr: true},
		{name: "cursor cycle", max: 1000, pages: []page{{1, "one"}, {1, "two"}, {1, "one"}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			type outcome struct {
				threads  []Thread
				complete bool
				err      error
			}
			done := make(chan outcome, 1)
			go func() {
				threads, complete, err := client.ListAllThreadsComplete(context.Background(), 100, tc.max)
				done <- outcome{threads, complete, err}
			}()
			cursor, serial := "", 0
			for _, p := range tc.pages {
				request := fake.next(t)
				if method(t, request) != "thread/list" {
					t.Fatalf("unexpected discovery RPC: %s", request)
				}
				fields := params(t, request)
				var gotCursor string
				if raw, ok := fields["cursor"]; ok {
					if err := json.Unmarshal(raw, &gotCursor); err != nil {
						t.Fatal(err)
					}
				}
				if gotCursor != cursor || string(fields["limit"]) != "100" {
					t.Fatalf("pagination parameters = %v; want cursor %q and limit 100", fields, cursor)
				}
				data := make([]map[string]any, p.count)
				for i := range data {
					data[i] = map[string]any{"id": fmt.Sprintf("thread-%d", serial), "source": "cli"}
					serial++
				}
				fake.respond(t, request, map[string]any{"data": data, "nextCursor": p.cursor})
				cursor = p.cursor
			}
			select {
			case result := <-done:
				if (result.err != nil) != tc.wantErr || result.complete != tc.complete || len(result.threads) != tc.count {
					t.Fatalf("scan returned %d threads, complete=%v, error=%v; want count=%d complete=%v error=%v", len(result.threads), result.complete, result.err, tc.count, tc.complete, tc.wantErr)
				}
				if tc.wantErr && !strings.Contains(result.err.Error(), "repeated pagination cursor") {
					t.Fatalf("unexpected cursor error: %v", result.err)
				}
				for i, thread := range result.threads {
					if thread.ID != fmt.Sprintf("thread-%d", i) {
						t.Fatalf("discovery order changed at %d: %q", i, thread.ID)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("discovery failed to stop after completion, cap, or repeated cursor")
			}
		})
	}
}

func TestListAllThreadsCompleteDoesNotReportFailedScanAsComplete(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type outcome struct {
		threads  []Thread
		complete bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		threads, complete, err := client.ListAllThreadsComplete(context.Background(), 100, 1000)
		done <- outcome{threads, complete, err}
	}()
	first := fake.next(t)
	fake.respond(t, first, map[string]any{"data": []map[string]any{{"id": "first", "source": "cli"}}, "nextCursor": "next"})
	second := fake.next(t)
	fake.write(t, map[string]any{"id": second["id"], "error": RPCError{Code: -32000, Message: "temporary failure"}})
	select {
	case result := <-done:
		if result.err == nil || result.complete || len(result.threads) != 0 {
			t.Fatalf("failed scan = %d threads, complete=%v, err=%v", len(result.threads), result.complete, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed discovery did not return")
	}
}
