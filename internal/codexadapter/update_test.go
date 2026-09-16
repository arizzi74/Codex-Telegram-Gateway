package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestUpdateQuiescenceTracksPendingAndConfirmedRPCError(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	if !client.UpdateQuiescent() {
		t.Fatal("initialized idle client is not quiescent")
	}
	done := make(chan error, 1)
	go func() { _, err := client.ReadThread(context.Background(), "thread-test", false); done <- err }()
	request := fake.next(t)
	if client.UpdateQuiescent() {
		t.Fatal("unanswered RPC allowed update")
	}
	fake.write(t, map[string]any{"id": json.RawMessage(request["id"]), "error": map[string]any{"code": -32001, "message": "request rejected"}})
	if err := <-done; err == nil {
		t.Fatal("expected confirmed RPC rejection")
	}
	if !client.UpdateQuiescent() {
		t.Fatal("confirmed rejection incorrectly latched update uncertainty")
	}
}

func TestUpdateQuiescenceKeepsTimedOutRPCUncertainAfterLateResponse(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.ReadThread(ctx, "thread-timeout", false); done <- err }()
	request := fake.next(t)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	if client.UpdateQuiescent() {
		t.Fatal("timed-out RPC permitted update")
	}
	fake.respond(t, request, map[string]any{"thread": map[string]any{"id": "thread-timeout", "status": "idle"}})
	go func() { _, err := client.ReadThread(context.Background(), "thread-next", false); done <- err }()
	next := fake.next(t)
	fake.respond(t, next, map[string]any{"thread": map[string]any{"id": "thread-next", "status": "idle"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if client.UpdateQuiescent() {
		t.Fatal("later idle read erased uncertainty about a previously submitted RPC")
	}
}

func TestUpdateQuiescenceTreatsCancelledSendConservatively(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ReadThread(ctx, "thread-cancelled", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v", err)
	}
	if client.UpdateQuiescent() {
		t.Fatal("cancelled send allowed automatic restart without delivery confirmation")
	}
}
