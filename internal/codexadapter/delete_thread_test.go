package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDeleteThreadUsesPermanentDeletionRPC(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { done <- client.DeleteThread(context.Background(), "thread-selected") }()
	request := fake.next(t)
	if got := method(t, request); got != "thread/delete" {
		t.Fatalf("method = %q", got)
	}
	p := params(t, request)
	if len(p) != 1 || string(p["threadId"]) != `"thread-selected"` {
		t.Fatalf("delete params = %s", request["params"])
	}
	fake.respond(t, request, map[string]any{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteThread(context.Background(), "  "); err == nil {
		t.Fatal("empty thread ID accepted")
	}
}

func TestDeleteThreadUnavailableDoesNotFallBackToArchive(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { done <- client.DeleteThread(context.Background(), "thread-selected") }()
	request := fake.next(t)
	fake.write(t, map[string]any{"id": request["id"], "error": map[string]any{"code": -32601, "message": "method not found"}})
	if err := <-done; !errors.Is(err, ErrMethodUnavailable) {
		t.Fatalf("delete error = %v", err)
	}
	if client.Supports("thread/delete") {
		t.Fatal("unavailable delete capability remained supported")
	}
}

func TestDeletedNotificationClearsActiveTurnAndRequests(t *testing.T) {
	client, _ := newFake(t)
	client.active["thread-selected"] = "turn-1"
	client.active["thread-other"] = "turn-2"
	client.serverRequests["selected"] = Request{ThreadID: "thread-selected"}
	client.serverRequests["other"] = Request{ThreadID: "thread-other"}
	event := newEvent("thread/deleted", json.RawMessage(`{"threadId":"thread-selected"}`))
	if event.Kind != "thread_deleted" || event.ThreadID != "thread-selected" || event.Unknown {
		t.Fatalf("event = %#v", event)
	}
	client.track(event)
	if _, ok := client.active["thread-selected"]; ok || client.RequestPending("selected") {
		t.Fatal("deleted thread retained active turn or pending approval")
	}
	if client.active["thread-other"] != "turn-2" || !client.RequestPending("other") {
		t.Fatal("unrelated thread changed")
	}
}
