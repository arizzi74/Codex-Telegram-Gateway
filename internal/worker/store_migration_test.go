package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	legacybolt "go.etcd.io/bbolt"
)

func TestLegacyWorkerMigrationPreservesLedgerAndReconcilesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	workerID := uuid.NewString()
	now := time.Now().UTC()
	runtime := protocol.Runtime{ID: uuid.NewString(), ProfileID: "primary", Generation: 7, Name: "Primary", State: "running", DefaultCWD: "/work"}
	session := protocol.Session{ID: uuid.NewString(), WorkerID: workerID, RuntimeID: runtime.ID, ThreadID: "saved-thread", Name: "Saved conversation", CWD: "/work/project", Loaded: true, State: "running", ActiveTurnID: "old-turn"}
	command := testCommand(workerID)
	command.RuntimeID, command.RuntimeGeneration, command.SessionID = runtime.ID, runtime.Generation, session.ID
	command.Operation, command.ThreadID, command.Arguments.Text = protocol.StartTurn, session.ThreadID, "Saved prompt"
	received := command
	received.ID = uuid.NewString()
	completed := command
	completed.ID = uuid.NewString()
	result := &protocol.Result{CommandID: completed.ID, State: "completed", Text: "Saved final response"}
	event := protocol.Event{ID: uuid.NewString(), WorkerID: workerID, RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: session.ID, Seq: 6, Kind: "turn_started"}
	question := asyncQuestionRecord{Generation: runtime.Generation, State: "pending", Approval: protocol.Approval{ID: uuid.NewString(), RequestID: "async:pending", Async: true, ThreadID: session.ThreadID, ItemID: "pending", State: "pending", Questions: []protocol.Question{{ID: "q1", Prompt: "Which hardware?"}}}}
	resolved := question
	resolved.State, resolved.Approval.RequestID, resolved.Approval.ItemID = "resolved", "async:resolved", "resolved"
	legacy, err := legacybolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = legacy.Update(func(tx *legacybolt.Tx) error {
		for _, name := range [][]byte{bucketMeta, bucketCommands, bucketOutbox, bucketRuntimes, bucketSessions, bucketAsyncQuestions} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		for key, value := range map[string][]byte{string(keySchema): []byte("1"), string(keyWorkerID): []byte(workerID), string(keyLastAck): sequenceKey(5), string(keyNextEvent): sequenceKey(7), "worker_update_result/test-request": []byte("recorded")} {
			if err := meta.Put([]byte(key), value); err != nil {
				return err
			}
		}
		put := func(bucket, key []byte, value any) error {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return tx.Bucket(bucket).Put(key, encoded)
		}
		for _, item := range []struct {
			bucket, key []byte
			value       any
		}{
			{bucketCommands, []byte(command.ID), CommandRecord{Command: command, ReceivedAt: now, State: CommandExecuting}},
			{bucketCommands, []byte(received.ID), CommandRecord{Command: received, ReceivedAt: now, State: CommandReceived}},
			{bucketCommands, []byte(completed.ID), CommandRecord{Command: completed, ReceivedAt: now, State: CommandCompleted, Result: result}},
			{bucketOutbox, sequenceKey(6), event},
			{bucketRuntimes, []byte("primary"), runtime},
			{bucketSessions, []byte(runtime.ID + "\x00" + session.ThreadID), session},
			{bucketAsyncQuestions, asyncQuestionKey(session.ID, question.Approval.RequestID), question},
			{bucketAsyncQuestions, asyncQuestionKey(session.ID, resolved.Approval.RequestID), resolved},
		} {
			if err := put(item.bucket, item.key, item.value); err != nil {
				return err
			}
		}
		return nil
	})
	closeErr := legacy.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("legacy fixture: %v %v", err, closeErr)
	}

	for open := 0; open < 2; open++ {
		store, err := OpenStore(path, workerID)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer store.Close()
			unknown, found, err := store.LoadCommand(command.ID)
			if err != nil || !found || unknown.State != CommandOutcomeUnknown || unknown.Result == nil || unknown.Result.Error == nil || unknown.Result.Error.Code != protocol.OutcomeUnknown {
				t.Fatalf("executing command became replayable after migration: %#v %v", unknown, err)
			}
			duplicate, err := store.Receive(command)
			if err != nil || duplicate.Accepted || duplicate.Record.State != CommandOutcomeUnknown {
				t.Fatalf("migrated command was accepted twice: %#v %v", duplicate, err)
			}
			loaded, found, err := store.LoadCommand(received.ID)
			if err != nil || !found || loaded.State != CommandReceived {
				t.Fatalf("received command changed: %#v %v", loaded, err)
			}
			loaded, found, err = store.LoadCommand(completed.ID)
			if err != nil || !found || loaded.State != CommandCompleted || !reflect.DeepEqual(loaded.Result, result) {
				t.Fatalf("completed result changed: %#v %v", loaded, err)
			}
			acked, high, err := store.EventWatermarks()
			if err != nil || acked != 5 || high != 7 {
				t.Fatalf("watermarks = %d/%d %v", acked, high, err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil || len(events) != 2 || events[0].Seq != 6 || events[0].ID != event.ID || events[1].Seq != 7 || events[1].Kind != "command_result_unknown" {
				t.Fatalf("outbox lost or duplicated event across migration/reopen: %#v %v", events, err)
			}
			loadedRuntime, found, err := store.RuntimeForProfile("primary")
			if err != nil || !found || !reflect.DeepEqual(loadedRuntime, runtime) {
				t.Fatalf("runtime identity/generation changed: %#v %v", loadedRuntime, err)
			}
			sessions, err := store.ListSessions(runtime.ID)
			if err != nil || len(sessions) != 1 || !reflect.DeepEqual(sessions[0], session) {
				t.Fatalf("session changed: %#v %v", sessions, err)
			}
			questions, err := store.asyncQuestionRecords(session.ID)
			if err != nil || len(questions) != 2 || !reflect.DeepEqual(questions[0], question) || !reflect.DeepEqual(questions[1], resolved) {
				t.Fatalf("pending question or tombstone changed: %#v %v", questions, err)
			}
		}()
	}
	header, err := os.ReadFile(path)
	if err != nil || len(header) < 16 || string(header[:16]) != "SQLite format 3\x00" {
		t.Fatalf("worker backend did not become SQLite: %v", err)
	}
	if _, err := os.Stat(path + ".bbolt-backup"); err != nil {
		t.Fatalf("pre-migration backup missing: %v", err)
	}
}
