package worker

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/workerdb"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestStoreReopenPreservesLedgerOutboxAndGeneration(t *testing.T) {
	path, workerID := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	command := testCommand(workerID)
	received, err := store.Receive(command)
	if err != nil || !received.Accepted {
		t.Fatalf("receive: %#v, %v", received, err)
	}
	if err := store.SetCommandState(command.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	event, err := store.AppendEvent(protocol.Event{Kind: "turn_started"})
	if err != nil {
		t.Fatal(err)
	}
	if event.Seq != 1 || event.ID == "" || event.WorkerID != workerID {
		t.Fatalf("unexpected event: %#v", event)
	}
	runtime, err := store.BeginRuntime("primary", "Primary", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Generation != 1 {
		t.Fatalf("first generation = %d", runtime.Generation)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, found, err := store.LoadCommand(command.ID)
	if err != nil || !found {
		t.Fatalf("load command: found=%t err=%v", found, err)
	}
	if loaded.State != CommandOutcomeUnknown || loaded.Result == nil || loaded.Result.Error.Code != protocol.OutcomeUnknown {
		t.Fatalf("executing command was replayable: %#v", loaded)
	}
	duplicate, err := store.Receive(command)
	if err != nil || duplicate.Accepted || duplicate.Record.State != CommandOutcomeUnknown {
		t.Fatalf("duplicate receipt: %#v, %v", duplicate, err)
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 2 || events[0].Seq != event.Seq || events[1].Kind != "command_result_unknown" {
		t.Fatalf("outbox after reopen: %#v, %v", events, err)
	}
	runtime, err = store.BeginRuntime("primary", "Primary", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Generation != 2 {
		t.Fatalf("generation did not advance before second spawn: %d", runtime.Generation)
	}
}

func TestRecordResultRollsBackLedgerWhenEventInvalid(t *testing.T) {
	id := uuid.NewString()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), id)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := testCommand(id)
	if _, err = store.Receive(c); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordResult(c.ID, CommandCompleted, nil, protocol.Event{Kind: "agent_message_delta"}); err == nil {
		t.Fatal("transient result event accepted")
	}
	record, _, err := store.LoadCommand(c.ID)
	if err != nil || record.State != CommandReceived {
		t.Fatal("partial outcome persisted", err)
	}
	if _, err = store.RecordResult(c.ID, CommandCompleted, &protocol.Result{CommandID: c.ID}, protocol.Event{Kind: "command_completed"}); err != nil {
		t.Fatal(err)
	}
	record, _, _ = store.LoadCommand(c.ID)
	events, err := store.OutboxAfter(0)
	if record.State != CommandCompleted || err != nil || len(events) != 1 {
		t.Fatal("atomic result missing", err)
	}
}

func TestAckThroughRejectsFutureSequenceWithoutDeletion(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for range 2 {
		if _, err := store.AppendEvent(protocol.Event{Kind: "runtime_started"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AckThrough(3); err == nil {
		t.Fatal("future acknowledgement accepted")
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 2 {
		t.Fatalf("future ACK deleted outbox: %d, %v", len(events), err)
	}
	if err := store.AckThrough(1); err != nil {
		t.Fatal(err)
	}
	events, err = store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Seq != 2 {
		t.Fatalf("ACK did not preserve future event: %#v, %v", events, err)
	}
}

func TestStoreRejectsMismatchedWorkerIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(path, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, uuid.NewString()); err == nil {
		t.Fatal("different enrolled worker identity accepted")
	}
}

func TestSQLiteTransactionRollbackDoesNotPersistOutboxMutation(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.db.Update(func(tx *workerdb.Tx) error {
		if err := tx.Bucket(bucketOutbox).Put(sequenceKey(1), []byte(`{"event_seq":1}`)); err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("transaction unexpectedly committed")
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 0 {
		t.Fatalf("rolled-back outbox record persisted: %#v, %v", events, err)
	}
}

func TestSessionUpsertKeepsIdentityPerRuntimeThread(t *testing.T) {
	workerID := uuid.NewString()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime, err := store.BeginRuntime("profile", "Profile", "/work")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread-1", Name: "First", State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread-1", Name: "Renamed", State: "running"})
	if err != nil || first.ID != second.ID || second.WorkerID != workerID {
		t.Fatalf("session identity changed: %#v %#v %v", first, second, err)
	}
	sessions, err := store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].Name != "Renamed" {
		t.Fatalf("sessions: %#v, %v", sessions, err)
	}
}

func testCommand(workerID string) protocol.Command {
	now := time.Now().UTC()
	return protocol.Command{ID: uuid.NewString(), WorkerID: workerID, RuntimeID: uuid.NewString(), RuntimeGeneration: 1, Operation: protocol.NewSession, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
}
