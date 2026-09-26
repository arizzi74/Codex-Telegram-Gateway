package registry

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIWorkerUpdatesQueueDurablyWithoutTelegram(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	worker := updateTestWorker(t, store, "Offline worker")
	disabled := updateTestWorker(t, store, "Disabled")
	if err := store.RevokeWorker(ctx, disabled.ID); err != nil {
		t.Fatal(err)
	}
	updates, err := store.QueueWebUIWorkerUpdates(ctx)
	if err != nil || len(updates) != 1 || updates[0].WorkerID != worker.ID.String() || updates[0].State != "queued" {
		t.Fatalf("queue = %+v %v", updates, err)
	}
	request := pendingUpdate(t, store, worker.ID)
	if updates[0].RequestID != request.RequestID {
		t.Fatalf("queue omitted durable request identity: %+v", updates[0])
	}
	updates, err = store.QueueWebUIWorkerUpdates(ctx)
	if err != nil || len(updates) != 1 || updates[0].State != "already_queued" || updates[0].RequestID != request.RequestID || pendingUpdate(t, store, worker.ID) != request {
		t.Fatalf("repeat = %+v %v", updates, err)
	}
	path := store.pool.path
	store.Close()
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if pendingUpdate(t, store, worker.ID) != request {
		t.Fatal("gateway restart lost update request")
	}
	if requests, err := store.PendingWorkerUpdates(ctx, disabled.ID); err != nil || len(requests) != 0 {
		t.Fatalf("disabled worker: %+v %v", requests, err)
	}
	var watchers, commands, bindings, deliveries int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM worker_update_watchers),
        (SELECT count(*) FROM commands),(SELECT count(*) FROM telegram_bindings),
        (SELECT count(*) FROM telegram_deliveries)`).Scan(&watchers, &commands, &bindings, &deliveries); err != nil {
		t.Fatal(err)
	}
	if watchers != 0 || commands != 0 || bindings != 0 || deliveries != 0 {
		t.Fatalf("web command changed Telegram state: %d %d %d %d", watchers, commands, bindings, deliveries)
	}
	connection := uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	event := workerUpdateEvent(t, worker.ID, 1, protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "up_to_date", Version: "1.0.0"})
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	updates, err = store.WorkerUpdateSnapshot(ctx)
	if err != nil || len(updates) != 1 || updates[0].State != "up_to_date" || updates[0].Version != "1.0.0" || updates[0].RequestID != request.RequestID || updates[0].Codex != nil {
		t.Fatalf("completed snapshot = %+v %v", updates, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("web update completion sent a Telegram notice: %d %v", deliveries, err)
	}
	if _, err := store.QueueWebUIWorkerUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	// SQLite timestamp defaults have millisecond precision. A newer request must
	// still win when two requests were created in the same clock tick.
	if _, err := store.pool.Exec(ctx, `UPDATE worker_update_requests SET created_at='2026-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	updates, err = store.WorkerUpdateSnapshot(ctx)
	if err != nil || len(updates) != 1 || updates[0].State != "pending" || updates[0].RequestID == request.RequestID {
		t.Fatalf("new pending snapshot = %+v %v", updates, err)
	}
}

func TestWebUIWorkerUpdatesConcurrentRequestsJoinTelegramMaintenance(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	worker := updateTestWorker(t, store, "Shared worker")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			updates, err := store.QueueWebUIWorkerUpdates(ctx)
			if err != nil || len(updates) != 1 {
				t.Errorf("concurrent queue = %+v %v", updates, err)
			}
		}()
	}
	wg.Wait()
	request := pendingUpdate(t, store, worker.ID)
	result := updateTestAccept(t, store, updateTestInput(1))
	if len(result.WorkerUpdates) != 1 || result.WorkerUpdates[0].State != "already_queued" || pendingUpdate(t, store, worker.ID) != request {
		t.Fatalf("Telegram did not join web maintenance: %+v", result)
	}
	var requests, watchers int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM worker_update_requests),
        (SELECT count(*) FROM worker_update_watchers)`).Scan(&requests, &watchers); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || watchers != 1 {
		t.Fatalf("requests=%d watchers=%d", requests, watchers)
	}
}
