package worker

import (
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"

	"github.com/iaia/telegramgw/internal/protocol"
)

func inventoryStoreFixture(t *testing.T) (*Store, protocol.Runtime, protocol.Session) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime, err := store.BeginRuntime("primary", "Primary", "/work")
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.UpsertSession(protocol.Session{
		RuntimeID: runtime.ID, ThreadID: "old-thread", Name: "Old session", Preview: "Saved input",
		CWD: "/work", GitRoot: "/work", GitBranch: "main", Loaded: true, State: "running", ActiveTurnID: "stale-turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, runtime, session
}

func TestArchiveDiscoveredSessionRetainsIdentityAndHistory(t *testing.T) {
	store, runtime, original := inventoryStoreFixture(t)
	history, err := store.AppendEvent(protocol.Event{
		RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: original.ID,
		Kind: "user_message", Data: json.RawMessage(`{"text":"Saved input"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, changed, err := store.ArchiveDiscoveredSession(runtime, original)
	if err != nil || !changed {
		t.Fatalf("archive changed=%t err=%v", changed, err)
	}
	if saved.ID != original.ID || saved.ThreadID != original.ThreadID || saved.Name != original.Name ||
		saved.Preview != original.Preview || saved.CWD != original.CWD || saved.GitRoot != original.GitRoot ||
		saved.GitBranch != original.GitBranch || !saved.Archived || saved.Loaded || saved.ActiveTurnID != "" ||
		saved.State != "not_loaded" || saved.UpdatedAt.Before(original.UpdatedAt) {
		t.Fatalf("incorrect inventory tombstone: %#v", saved)
	}
	sessions, err := store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0] != saved {
		t.Fatalf("retained session = %#v, %v", sessions, err)
	}
	// Both a replay of the original scan and a subsequent scan are no-ops.
	for _, snapshot := range []protocol.Session{original, saved} {
		got, changed, err := store.ArchiveDiscoveredSession(runtime, snapshot)
		if err != nil || changed || got != saved {
			t.Fatalf("repeated archive = %#v, changed=%t err=%v", got, changed, err)
		}
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 2 || events[0].ID != history.ID || string(events[0].Data) != string(history.Data) {
		t.Fatalf("retained event history = %#v, %v", events, err)
	}
	event := events[1]
	if event.Kind != "session_state_changed" || event.SessionID != original.ID || event.RuntimeID != runtime.ID ||
		event.RuntimeGeneration != runtime.Generation || event.WorkerID != original.WorkerID || event.Seq != history.Seq+1 {
		t.Fatalf("archive event = %#v", event)
	}
	var snapshot protocol.Session
	if err := json.Unmarshal(event.Data, &snapshot); err != nil || snapshot != saved {
		t.Fatalf("archive event snapshot = %#v, %v", snapshot, err)
	}
}

func TestArchiveDiscoveredSessionSkipsChangedOrMissingSnapshots(t *testing.T) {
	for _, kind := range []string{"timestamp", "metadata", "identity", "missing"} {
		t.Run(kind, func(t *testing.T) {
			store, runtime, saved := inventoryStoreFixture(t)
			expected := saved
			switch kind {
			case "timestamp":
				expected.UpdatedAt = expected.UpdatedAt.Add(-time.Second)
			case "metadata":
				expected.Name = "Old name before a concurrent rename"
			case "identity":
				expected.ID = uuid.NewString()
			case "missing":
				expected.ThreadID = "missing-thread"
			}
			_, changed, err := store.ArchiveDiscoveredSession(runtime, expected)
			if err != nil || changed {
				t.Fatalf("stale or missing archive changed=%t err=%v", changed, err)
			}
			sessions, err := store.ListSessions(runtime.ID)
			if err != nil || len(sessions) != 1 || sessions[0] != saved {
				t.Fatalf("concurrent session changed: %#v, %v", sessions, err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil || len(events) != 0 {
				t.Fatalf("unexpected archive events: %#v, %v", events, err)
			}
		})
	}
}

func TestArchiveDiscoveredSessionRejectsInvalidTargets(t *testing.T) {
	for _, kind := range []string{"runtime", "runtime_worker", "session_worker", "runtime_id", "session_id", "thread_id", "generation", "overflow_generation"} {
		t.Run(kind, func(t *testing.T) {
			store, runtime, session := inventoryStoreFixture(t)
			switch kind {
			case "runtime":
				runtime.ID = uuid.NewString()
			case "runtime_worker":
				runtime.WorkerID = uuid.NewString()
			case "session_worker":
				session.WorkerID = uuid.NewString()
			case "runtime_id":
				runtime.ID, session.RuntimeID = "invalid", "invalid"
			case "session_id":
				session.ID = "invalid"
			case "thread_id":
				session.ThreadID = ""
			case "generation":
				runtime.Generation = 0
			case "overflow_generation":
				runtime.Generation = uint64(math.MaxInt64) + 1
			}
			if _, changed, err := store.ArchiveDiscoveredSession(runtime, session); err == nil || changed {
				t.Fatalf("invalid target accepted: changed=%t err=%v", changed, err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil || len(events) != 0 {
				t.Fatalf("unexpected archive events: %#v, %v", events, err)
			}
		})
	}
}

func TestArchiveDiscoveredSessionRollsBackWhenEventCannotCommit(t *testing.T) {
	store, runtime, original := inventoryStoreFixture(t)
	if err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(math.MaxInt64))
	}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ArchiveDiscoveredSession(runtime, original); err == nil || changed {
		t.Fatalf("archive survived failed event append: changed=%t err=%v", changed, err)
	}
	sessions, err := store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0] != original {
		t.Fatalf("archive transaction did not roll back: %#v, %v", sessions, err)
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 0 {
		t.Fatalf("unexpected archive events: %#v, %v", events, err)
	}
}

func TestRestoreDiscoveredSessionPreservesIdentityAndPublishesCurrentState(t *testing.T) {
	store, runtime, original := inventoryStoreFixture(t)
	archived, changed, err := store.ArchiveDiscoveredSession(runtime, original)
	if err != nil || !changed {
		t.Fatalf("archive changed=%t err=%v", changed, err)
	}
	candidate := original
	candidate.Name, candidate.ActiveTurnID = "Reappeared session", "current-turn"
	restored, changed, err := store.RestoreDiscoveredSession(runtime, archived, candidate)
	if err != nil || !changed || restored.Archived || restored.ID != original.ID ||
		restored.Name != candidate.Name || restored.ActiveTurnID != "current-turn" || !restored.Loaded {
		t.Fatalf("restored = %#v, changed=%t err=%v", restored, changed, err)
	}
	if _, changed, err := store.RestoreDiscoveredSession(runtime, archived, candidate); err != nil || changed {
		t.Fatalf("repeated restore changed=%t err=%v", changed, err)
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 2 {
		t.Fatalf("restore events = %#v, %v", events, err)
	}
	var snapshot protocol.Session
	if err := json.Unmarshal(events[1].Data, &snapshot); err != nil || snapshot != restored ||
		events[1].Kind != "session_state_changed" || events[1].SessionID != original.ID {
		t.Fatalf("restore snapshot = %#v, %v", snapshot, err)
	}
	stored, err := store.ListSessions(runtime.ID)
	if err != nil || len(stored) != 1 || stored[0] != restored {
		t.Fatalf("stored restore = %#v, %v", stored, err)
	}
}

func TestRestoreDiscoveredSessionRequiresUnchangedArchivedSnapshot(t *testing.T) {
	for _, kind := range []string{"stale", "visible", "archived_candidate", "different_identity", "different_worker", "event_failure"} {
		t.Run(kind, func(t *testing.T) {
			store, runtime, original := inventoryStoreFixture(t)
			expected, changed, err := store.ArchiveDiscoveredSession(runtime, original)
			if err != nil || !changed {
				t.Fatalf("archive changed=%t err=%v", changed, err)
			}
			candidate := original
			wantError := false
			switch kind {
			case "stale":
				expected.UpdatedAt = expected.UpdatedAt.Add(-time.Second)
			case "visible":
				candidate, err = store.UpsertSession(original)
				if err != nil {
					t.Fatal(err)
				}
				expected = candidate
			case "archived_candidate":
				candidate.Archived, wantError = true, true
			case "different_identity":
				candidate.ID, wantError = uuid.NewString(), true
			case "different_worker":
				candidate.WorkerID, wantError = uuid.NewString(), true
			case "event_failure":
				wantError = true
				if err := store.db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(math.MaxInt64))
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.ListSessions(runtime.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, changed, err := store.RestoreDiscoveredSession(runtime, expected, candidate); changed || (err != nil) != wantError {
				t.Fatalf("invalid restore changed=%t err=%v, wantError=%t", changed, err, wantError)
			}
			after, err := store.ListSessions(runtime.ID)
			if err != nil || len(after) != 1 || after[0] != before[0] {
				t.Fatalf("failed restore changed session: %#v, %v", after, err)
			}
			events, err := store.OutboxAfter(0)
			if err != nil || len(events) != 1 {
				t.Fatalf("unexpected restore event: %#v, %v", events, err)
			}
		})
	}
}
