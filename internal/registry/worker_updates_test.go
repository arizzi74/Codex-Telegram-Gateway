package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func updateTestWorker(t *testing.T, store *Store, name string) Worker {
	t.Helper()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	worker, err := store.CreateWorker(context.Background(), CreateWorkerInput{Name: name, OS: "linux", Arch: "arm64", Version: "1.0.0", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func updateTestInput(id int64) IncomingUpdate {
	return IncomingUpdate{BotID: "bot", UpdateID: id, UserID: 10, ChatID: 20, Action: "update_workers"}
}

func updateTestAccept(t *testing.T, store *Store, in IncomingUpdate) AcceptResult {
	t.Helper()
	result, err := store.AcceptTelegram(context.Background(), in)
	if err != nil || result.ErrorCode != "" || (!result.Duplicate && result.View != "worker_updates") {
		t.Fatalf("queue workers: %+v %v", result, err)
	}
	return result
}

func pendingUpdate(t *testing.T, store *Store, worker uuid.UUID) protocol.WorkerUpdateRequest {
	t.Helper()
	requests, err := store.PendingWorkerUpdates(context.Background(), worker)
	if err != nil || len(requests) != 1 {
		t.Fatalf("pending worker updates: %+v %v", requests, err)
	}
	return requests[0]
}

func workerUpdateEvent(t *testing.T, worker uuid.UUID, seq uint64, result protocol.WorkerUpdateResult) protocol.Event {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: worker.String(), Kind: "worker_update_result", OccurredAt: time.Now().UTC(), Data: data}
}

func TestWorkerUpdatesQueueOfflineWorkersWithoutRuntimesAndSurviveRestart(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	first := updateTestWorker(t, store, "First")
	second := updateTestWorker(t, store, "Second")
	disabled := updateTestWorker(t, store, "Disabled")
	if err := store.RevokeWorker(ctx, disabled.ID); err != nil {
		t.Fatal(err)
	}
	result := updateTestAccept(t, store, updateTestInput(1))
	want := []WorkerUpdateStatus{{WorkerID: first.ID.String(), Name: "First", State: "queued", Version: "1.0.0"}, {WorkerID: second.ID.String(), Name: "Second", State: "queued", Version: "1.0.0"}}
	if !reflect.DeepEqual(result.WorkerUpdates, want) {
		t.Fatalf("queued workers = %+v, want %+v", result.WorkerUpdates, want)
	}
	firstRequest := pendingUpdate(t, store, first.ID)
	secondRequest := pendingUpdate(t, store, second.ID)
	if requests, err := store.PendingWorkerUpdates(ctx, disabled.ID); err != nil || len(requests) != 0 {
		t.Fatalf("disabled requests = %+v %v", requests, err)
	}
	if result := updateTestAccept(t, store, updateTestInput(1)); !result.Duplicate {
		t.Fatalf("Telegram replay not deduplicated: %+v", result)
	}
	result = updateTestAccept(t, store, updateTestInput(2))
	for _, worker := range result.WorkerUpdates {
		if worker.State != "already_queued" {
			t.Fatalf("repeated command did not join pending request: %+v", worker)
		}
	}
	if pendingUpdate(t, store, first.ID) != firstRequest || pendingUpdate(t, store, second.ID) != secondRequest {
		t.Fatal("repeated command created a different update request")
	}
	var requests, watchers, commands, bindings, runtimes int
	if err := store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM worker_update_requests),
        (SELECT count(*) FROM worker_update_watchers),(SELECT count(*) FROM commands),
        (SELECT count(*) FROM telegram_bindings),(SELECT count(*) FROM runtimes)`).Scan(&requests, &watchers, &commands, &bindings, &runtimes); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || watchers != 2 || commands != 0 || bindings != 0 || runtimes != 0 {
		t.Fatalf("queue changed unrelated state: requests=%d watchers=%d commands=%d bindings=%d runtimes=%d", requests, watchers, commands, bindings, runtimes)
	}
	path := store.pool.path
	store.Close()
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if pendingUpdate(t, reopened, first.ID) != firstRequest || pendingUpdate(t, reopened, second.ID) != secondRequest {
		t.Fatal("gateway restart lost frozen update requests")
	}
	if err := reopened.BindConnection(ctx, first.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if pendingUpdate(t, reopened, first.ID) != firstRequest {
		t.Fatal("worker reconnect changed maintenance identity")
	}
}

func TestWorkerUpdateDispatchLeaseAndDisabledWorker(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	worker := updateTestWorker(t, store, "worker")
	updateTestAccept(t, store, updateTestInput(1))
	request := pendingUpdate(t, store, worker.ID)
	requestID := uuid.MustParse(request.RequestID)
	if err := store.MarkWorkerUpdateDispatched(ctx, uuid.New(), requestID); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("wrong-worker dispatch = %v", err)
	}
	before := time.Now().UTC()
	if err := store.MarkWorkerUpdateDispatched(ctx, worker.ID, requestID); err != nil {
		t.Fatal(err)
	}
	var next time.Time
	if err := store.pool.QueryRow(ctx, `SELECT next_attempt_at FROM worker_update_requests WHERE request_id=$1`, requestID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next.Before(before.Add(5*time.Second)) || next.After(time.Now().UTC().Add(5*time.Second)) {
		t.Fatalf("retry time %s is not five seconds after dispatch", next)
	}
	if requests, err := store.PendingWorkerUpdates(ctx, worker.ID); err != nil || len(requests) != 0 {
		t.Fatalf("leased request immediately redispatched: %+v %v", requests, err)
	}
	if err := store.MarkWorkerUpdateDispatched(ctx, worker.ID, requestID); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("parallel dispatch leased the same request: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE worker_update_requests SET next_attempt_at=$2 WHERE request_id=$1`, requestID, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if pendingUpdate(t, store, worker.ID) != request {
		t.Fatal("retry changed request identity")
	}
	if err := store.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if requests, err := store.PendingWorkerUpdates(ctx, worker.ID); err != nil || len(requests) != 0 {
		t.Fatalf("disabled worker still dispatchable: %+v %v", requests, err)
	}
	if err := store.MarkWorkerUpdateDispatched(ctx, worker.ID, requestID); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("disabled worker dispatch = %v", err)
	}
}

func TestWorkerUpdateResultsNotifyOnlyOriginalSubscribersOnce(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	worker := updateTestWorker(t, store, "Frozen worker name")
	connection := uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	updateTestAccept(t, store, updateTestInput(1))
	request := pendingUpdate(t, store, worker.ID)
	second := updateTestInput(2)
	second.ChatID, second.TopicID = 30, 40
	updateTestAccept(t, store, second)
	// A different authorized user in the same topic joins the same notification.
	second.UpdateID, second.UserID = 3, 11
	updateTestAccept(t, store, second)
	if _, err := store.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: 4, UserID: 10, ChatID: 99, Action: "help"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workers SET name='Renamed after queue' WHERE worker_id=$1`, worker.ID); err != nil {
		t.Fatal(err)
	}
	result := protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "completed", Version: "1.1.0"}
	event := workerUpdateEvent(t, worker.ID, 1, result)
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	// A retry with a fresh sequence is harmless too, and a late failure cannot
	// overwrite the successful result or create another completion notice.
	event.ID, event.Seq = uuid.NewString(), 2
	if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	late := workerUpdateEvent(t, worker.ID, 3, protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "failed", ErrorCode: "late_failure"})
	if err := store.IngestEvent(ctx, worker.ID, connection, late); err != nil {
		t.Fatal(err)
	}
	rows, err := store.pool.Query(ctx, `SELECT chat_id,message_thread_id,payload FROM telegram_deliveries
        WHERE kind='ui_response' AND json_extract(payload,'$.view')='worker_update_result' ORDER BY chat_id`)
	if err != nil {
		t.Fatal(err)
	}
	var destinations [][2]int64
	for rows.Next() {
		var chat, topic int64
		var raw []byte
		if err := rows.Scan(&chat, &topic, &raw); err != nil {
			t.Fatal(err)
		}
		var response AcceptResult
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		want := []WorkerUpdateStatus{{WorkerID: worker.ID.String(), Name: "Frozen worker name", State: "completed", Version: "1.1.0"}}
		if !reflect.DeepEqual(response.WorkerUpdates, want) || response.SessionID != "" || response.RuntimeID != "" {
			t.Fatalf("completion notice was not frozen/private: %+v", response)
		}
		destinations = append(destinations, [2]int64{chat, topic})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(destinations, [][2]int64{{20, 0}, {30, 40}}) {
		t.Fatalf("completion destinations = %+v", destinations)
	}
	if pending, err := store.PendingWorkerUpdates(ctx, worker.ID); err != nil || len(pending) != 0 {
		t.Fatalf("completed request remains pending: %+v %v", pending, err)
	}
	updateTestAccept(t, store, updateTestInput(5))
	newRequest := pendingUpdate(t, store, worker.ID)
	if newRequest.RequestID == request.RequestID {
		t.Fatal("new check reused a completed request")
	}
	late.ID, late.Seq = uuid.NewString(), 4
	if err := store.IngestEvent(ctx, worker.ID, connection, late); err != nil {
		t.Fatal(err)
	}
	if pendingUpdate(t, store, worker.ID) != newRequest {
		t.Fatal("old result affected newer maintenance request")
	}
}

func TestWorkerUpdateResultsRejectForeignTargetsAndInvalidData(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := updateTestWorker(t, env.store, "other")
	updateTestAccept(t, env.store, updateTestInput(1))
	request := pendingUpdate(t, env.store, env.worker)
	foreign := pendingUpdate(t, env.store, other.ID)
	result := protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "up_to_date", Version: "1.0.0"}
	for _, tc := range []struct {
		name   string
		mutate func(*protocol.Event)
	}{
		{"runtime", func(e *protocol.Event) { e.RuntimeID, e.RuntimeGeneration = env.runtime.String(), 1 }},
		{"session", func(e *protocol.Event) {
			e.RuntimeID, e.RuntimeGeneration, e.SessionID = env.runtime.String(), 1, env.session.String()
		}},
		{"generation", func(e *protocol.Event) { e.RuntimeGeneration = 1 }},
		{"foreign worker", func(e *protocol.Event) { e.WorkerID = other.ID.String() }},
		{"foreign request", func(e *protocol.Event) {
			e.Data, _ = json.Marshal(protocol.WorkerUpdateResult{RequestID: foreign.RequestID, State: "up_to_date", Version: "1.0.0"})
		}},
		{"unknown request", func(e *protocol.Event) {
			e.Data, _ = json.Marshal(protocol.WorkerUpdateResult{RequestID: uuid.NewString(), State: "up_to_date", Version: "1.0.0"})
		}},
		{"missing version", func(e *protocol.Event) {
			e.Data, _ = json.Marshal(protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "completed"})
		}},
		{"nonterminal", func(e *protocol.Event) {
			e.Data, _ = json.Marshal(protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "pending"})
		}},
		{"unsafe error", func(e *protocol.Event) {
			e.Data, _ = json.Marshal(protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "failed", ErrorCode: "unbounded text with private details"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := workerUpdateEvent(t, env.worker, 2, result)
			tc.mutate(&event)
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); !errors.Is(err, ErrEventTarget) {
				t.Fatalf("invalid result accepted: %v", err)
			}
			if watermark, err := env.store.EventWatermark(ctx, env.worker); err != nil || watermark != 1 {
				t.Fatalf("invalid result changed watermark: %d %v", watermark, err)
			}
			if pendingUpdate(t, env.store, env.worker) != request {
				t.Fatal("invalid result changed pending request")
			}
		})
	}
	// Maintenance results remain valid after a runtime generation changes.
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, workerUpdateEvent(t, env.worker, 2, result)); err != nil {
		t.Fatalf("runtime-independent result rejected: %v", err)
	}
}

func TestWorkerUpdateUnsupportedFailureAndArgumentValidation(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	if result := updateTestAccept(t, store, updateTestInput(1)); len(result.WorkerUpdates) != 0 {
		t.Fatalf("empty enrollment list: %+v", result)
	}
	worker := updateTestWorker(t, store, "worker")
	for index, field := range []string{"text", "target"} {
		in := updateTestInput(int64(index + 2))
		if field == "text" {
			in.Text = "--force"
		} else {
			in.Target = worker.ID.String()
		}
		result, err := store.AcceptTelegram(ctx, in)
		if err != nil || result.ErrorCode != "worker_updates_usage" {
			t.Fatalf("invalid argument = %+v %v", result, err)
		}
	}
	if requests, err := store.PendingWorkerUpdates(ctx, worker.ID); err != nil || len(requests) != 0 {
		t.Fatalf("invalid arguments queued requests: %+v %v", requests, err)
	}
	updateTestAccept(t, store, updateTestInput(4))
	request := pendingUpdate(t, store, worker.ID)
	if err := store.FailWorkerUpdate(ctx, uuid.New(), uuid.MustParse(request.RequestID), "unsupported_worker"); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("foreign failure = %v", err)
	}
	for range 2 {
		if err := store.FailWorkerUpdate(ctx, worker.ID, uuid.MustParse(request.RequestID), "unsupported_worker"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE json_extract(payload,'$.view')='worker_update_result'
        AND json_extract(payload,'$.worker_updates[0].state')='failed' AND json_extract(payload,'$.worker_updates[0].error_code')='unsupported_worker'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unsupported completion notices=%d err=%v", count, err)
	}
}

func TestWorkerUpdatesPreserveSelectionAndSessionWizard(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_session_wizards
        (wizard_id,bot_id,user_id,chat_id,revision,kind,phase,expires_at) VALUES ($1,'bot',10,20,1,'new','name',$2)`, uuid.New(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := env.store.pool.QueryRow(ctx, `SELECT revision FROM telegram_selection_revisions WHERE bot_id='bot' AND user_id=10 AND chat_id=20`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	updateTestAccept(t, env.store, updateTestInput(2))
	var selected, phase string
	var after, commands int64
	if err := env.store.pool.QueryRow(ctx, `SELECT (SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20),
        (SELECT revision FROM telegram_selection_revisions WHERE bot_id='bot' AND user_id=10 AND chat_id=20),
        (SELECT phase FROM telegram_session_wizards WHERE bot_id='bot' AND user_id=10 AND chat_id=20),
        (SELECT count(*) FROM commands)`).Scan(&selected, &after, &phase, &commands); err != nil {
		t.Fatal(err)
	}
	if selected != env.session.String() || before != after || phase != "name" || commands != 0 {
		t.Fatalf("maintenance altered session action: selected=%s revision=%d/%d phase=%s commands=%d", selected, before, after, phase, commands)
	}
}
