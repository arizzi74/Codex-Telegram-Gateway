package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestReadThreadStateUsesMetadataAndOneTurnWithoutItems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		data   any
		active string
		bad    bool
	}{
		{name: "empty", data: []any{}},
		{name: "completed", data: []map[string]any{{"id": "old", "status": "completed"}}},
		{name: "active", data: []map[string]any{{"id": "live", "status": "inProgress"}}, active: "live"},
		{name: "missing page", bad: true},
		{name: "too many turns", data: []map[string]any{{"id": "a", "status": "completed"}, {"id": "b", "status": "completed"}}, bad: true},
		{name: "unknown status", data: []map[string]any{{"id": "a", "status": "future"}}, bad: true},
		{name: "missing identity", data: []map[string]any{{"status": "inProgress"}}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan struct {
				thread Thread
				err    error
			}, 1)
			go func() {
				thread, err := client.ReadThreadState(context.Background(), "thread")
				done <- struct {
					thread Thread
					err    error
				}{thread, err}
			}()
			metadata := fake.next(t)
			if method(t, metadata) != "thread/read" || string(params(t, metadata)["includeTurns"]) != "false" {
				t.Fatal("state inspection requested unbounded thread history")
			}
			fake.respond(t, metadata, map[string]any{"thread": map[string]any{"id": "thread", "name": "Saved session", "status": "idle"}})
			latest := fake.next(t)
			p := params(t, latest)
			if method(t, latest) != "thread/turns/list" || string(p["threadId"]) != `"thread"` || string(p["limit"]) != "1" || string(p["sortDirection"]) != `"desc"` || string(p["itemsView"]) != `"notLoaded"` {
				t.Fatalf("latest turn request = %#v", p)
			}
			fake.respond(t, latest, map[string]any{"data": tc.data})
			result := <-done
			if (result.err != nil) != tc.bad {
				t.Fatalf("state read error = %v, want error %v", result.err, tc.bad)
			}
			if !tc.bad && (result.thread.Name != "Saved session" || result.thread.ActiveTurnID != tc.active) {
				t.Fatalf("state metadata = %#v", result.thread)
			}
		})
	}
}

func TestReadThreadStateFailsClosedWithoutBoundedTurnMethod(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { _, err := client.ReadThreadState(context.Background(), "thread"); done <- err }()
	metadata := fake.next(t)
	fake.respond(t, metadata, map[string]any{"thread": map[string]any{"id": "thread", "status": "idle"}})
	latest := fake.next(t)
	fake.write(t, map[string]any{"id": json.RawMessage(latest["id"]), "error": map[string]any{"code": -32601, "message": "unavailable"}})
	if err := <-done; !errors.Is(err, ErrMethodUnavailable) {
		t.Fatalf("unavailable bounded state method = %v", err)
	}
}
