package registry

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
)

func TestSidebarInventoryChangesWithoutSessionsAndIgnoresIdleHeartbeats(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	changes, unsubscribe, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	read := func() SessionActivitySnapshot {
		t.Helper()
		result, err := env.store.LiveSessionActivity(ctx)
		if err != nil || len(result.Sessions) != 0 || len(result.InventoryRevision) != 64 {
			t.Fatalf("empty-worker inventory: %+v %v", result, err)
		}
		return result
	}
	previous := read()
	assertChanged := func() {
		t.Helper()
		activityChanged(t, changes)
		next := read()
		if next.InventoryRevision == previous.InventoryRevision || next.Revision <= previous.Revision {
			t.Fatalf("worker/runtime change did not update inventory: before=%+v after=%+v", previous, next)
		}
		previous = next
	}
	worker, err := env.store.CreateWorker(ctx, CreateWorkerInput{Name: "Fresh worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-new-token")})
	if err != nil {
		t.Fatal(err)
	}
	assertChanged()
	connection := uuid.New()
	if err := env.store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	hb := Heartbeat{WorkerID: worker.ID, ConnectionID: connection,
		Metadata: json.RawMessage(`{"supports_session_workspaces":true,"supports_webui_session_creation":true}`),
		Runtimes: []Runtime{{ID: uuid.New(), WorkerID: worker.ID, ProfileID: "primary", Name: "Primary", State: "running", Generation: 1,
			CodexVersion: "codex-cli 0.160.0", DefaultCWD: "/home/example"}}}
	if err := env.store.RecordHeartbeat(ctx, hb); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	for range 3 {
		if err := env.store.RecordHeartbeat(ctx, hb); err != nil {
			t.Fatal(err)
		}
	}
	activityUnchanged(t, changes)
	if next := read(); next.InventoryRevision != previous.InventoryRevision || next.Revision != previous.Revision {
		t.Fatalf("idle heartbeat reloaded inventory: %+v", next)
	}
	hb.Runtimes[0].Name = "Renamed runtime"
	hb.Runtimes[0].DefaultCWD = "/home/example/CODEX"
	if err := env.store.RecordHeartbeat(ctx, hb); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	hb.Metadata = json.RawMessage(`{"supports_session_workspaces":true,"supports_webui_session_creation":false}`)
	if err := env.store.RecordHeartbeat(ctx, hb); err != nil {
		t.Fatal(err)
	}
	assertChanged()
	if err := env.store.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	assertChanged()
}

func TestSidebarInventoryNotifiesOnEnrollmentRedemption(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	changes, unsubscribe, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	initial, err := env.store.LiveSessionActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, code, err := env.store.CreateWorkerEnrollment(ctx, "restricted")
	if err != nil {
		t.Fatal(err)
	}
	activityUnchanged(t, changes)
	if _, err := env.store.RedeemWorkerEnrollment(ctx, RedeemWorkerEnrollmentInput{Code: code, Name: "Newly enrolled", OS: "linux", Arch: "arm64"}); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	result, err := env.store.LiveSessionActivity(ctx)
	if err != nil || result.InventoryRevision == initial.InventoryRevision || len(result.Sessions) != 0 {
		t.Fatalf("redeemed worker did not change empty inventory: %+v %v", result, err)
	}
}
