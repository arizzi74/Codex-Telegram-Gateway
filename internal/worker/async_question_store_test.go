package worker

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	bolt "go.etcd.io/bbolt"
)

func TestAsyncQuestionStoreSurvivesReopenAndDeduplicatesGeneration(t *testing.T) {
	path, workerID := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 4}
	sessionID := uuid.NewString()
	question := testAsyncQuestionApproval()
	first, pending, err := store.announceAsyncQuestion(runtime, sessionID, question)
	if err != nil || !pending || first.ID == "" || !first.Async || first.State != "pending" {
		t.Fatalf("announce = %#v pending=%v err=%v", first, pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path, workerID)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.asyncQuestionRecords(sessionID)
	if err != nil || len(records) != 1 || records[0].State != "pending" || records[0].Generation != runtime.Generation || !reflect.DeepEqual(records[0].Approval, first) {
		t.Fatalf("reopened question = %#v, %v", records, err)
	}
	duplicate, pending, err := store.announceAsyncQuestion(runtime, sessionID, question)
	if err != nil || !pending || !reflect.DeepEqual(duplicate, first) {
		t.Fatalf("duplicate announce = %#v pending=%v err=%v", duplicate, pending, err)
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "user_input_requested" || events[0].SessionID != sessionID || events[0].RuntimeGeneration != 4 {
		t.Fatalf("duplicate or missing outbox event: %#v, %v", events, err)
	}
	var published protocol.Approval
	if err := json.Unmarshal(events[0].Data, &published); err != nil || !reflect.DeepEqual(published, first) {
		t.Fatalf("persisted question differs from published question: %#v, %v", published, err)
	}
	runtime.Generation++
	refreshed, pending, err := store.announceAsyncQuestion(runtime, sessionID, question)
	if err != nil || !pending || refreshed.ID == first.ID || refreshed.RequestID != first.RequestID {
		t.Fatalf("new runtime generation did not refresh callback identity: %#v pending=%v err=%v", refreshed, pending, err)
	}
	events, err = store.OutboxAfter(0)
	if err != nil || len(events) != 2 || events[1].RuntimeGeneration != 5 {
		t.Fatalf("new generation did not reannounce: %#v, %v", events, err)
	}
	if _, _, err := store.announceAsyncQuestion(runtime, sessionID, question); err != nil {
		t.Fatal(err)
	}
	events, err = store.OutboxAfter(0)
	if err != nil || len(events) != 2 {
		t.Fatalf("new generation duplicate created another notification: %#v, %v", events, err)
	}
}

func TestAsyncQuestionStoreTombstonesDoNotReopenOnHistoryReplay(t *testing.T) {
	for _, state := range []string{"submitting", "submitted", "outcome_unknown", "resolved", "superseded"} {
		t.Run(state, func(t *testing.T) {
			workerID := uuid.NewString()
			store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), workerID)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1}
			sessionID := uuid.NewString()
			question := testAsyncQuestionApproval()
			announced, _, err := store.announceAsyncQuestion(runtime, sessionID, question)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.setAsyncQuestionState(runtime, sessionID, question.RequestID, state, state == "resolved"); err != nil {
				t.Fatal(err)
			}
			wantEvents := 1
			if state == "resolved" {
				wantEvents++
			}
			for _, generation := range []uint64{1, 2} {
				runtime.Generation = generation
				if _, pending, err := store.announceAsyncQuestion(runtime, sessionID, question); err != nil || pending {
					t.Fatalf("%s question reopened on generation %d: pending=%v err=%v", state, generation, pending, err)
				}
			}
			records, err := store.asyncQuestionRecords(sessionID)
			if err != nil || len(records) != 1 || records[0].State != state {
				t.Fatalf("tombstone was lost: %#v, %v", records, err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil || len(events) != wantEvents {
				t.Fatalf("replayed history emitted another question: %#v, %v", events, err)
			}
			if state == "resolved" {
				var resolved protocol.Approval
				if err := json.Unmarshal(events[1].Data, &resolved); err != nil || events[1].Kind != "approval_resolved" || resolved.ID != announced.ID || resolved.State != "cleared" || !resolved.Async {
					t.Fatalf("durable resolution = %#v / %#v, %v", events[1], resolved, err)
				}
			}
		})
	}
}

func TestAsyncQuestionStoreRollsBackWhenOutboxAppendFails(t *testing.T) {
	workerID := uuid.NewString()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1}
	sessionID := uuid.NewString()
	question := testAsyncQuestionApproval()
	setNext := func(next uint64) {
		t.Helper()
		if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(next)) }); err != nil {
			t.Fatal(err)
		}
	}
	setNext(math.MaxInt64)
	if _, pending, err := store.announceAsyncQuestion(runtime, sessionID, question); err == nil || pending {
		t.Fatalf("exhausted outbox unexpectedly accepted question: pending=%v err=%v", pending, err)
	}
	records, err := store.asyncQuestionRecords(sessionID)
	if err != nil || len(records) != 0 {
		t.Fatalf("question persisted without its notification: %#v, %v", records, err)
	}
	setNext(1)
	if _, _, err := store.announceAsyncQuestion(runtime, sessionID, question); err != nil {
		t.Fatal(err)
	}
	setNext(math.MaxInt64)
	if err := store.setAsyncQuestionState(runtime, sessionID, question.RequestID, "resolved", true); err == nil {
		t.Fatal("exhausted outbox unexpectedly accepted question resolution")
	}
	records, err = store.asyncQuestionRecords(sessionID)
	if err != nil || len(records) != 1 || records[0].State != "pending" {
		t.Fatalf("question resolved without its resolution notification: %#v, %v", records, err)
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "user_input_requested" {
		t.Fatalf("failed atomic transaction altered outbox: %#v, %v", events, err)
	}
}

func testAsyncQuestionApproval() protocol.Approval {
	return protocol.Approval{
		RequestID: "async:question-item", ThreadID: "thread", TurnID: "turn", ItemID: "question-item", Type: "user_input",
		Questions: []protocol.Question{{ID: "q1", Prompt: "Which hardware?", Options: []string{"Mac", "Windows"}}},
	}
}
