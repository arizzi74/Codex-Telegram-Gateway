package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestSessionActivitySettingsAreAuthoritativeRevisionAndGenerationFenced(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	discovered := env.discovery(t)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, discovered); err != nil {
		t.Fatal(err)
	}
	_, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	initial, err := env.store.LiveSessionActivity(ctx)
	if err != nil || initial.Sessions[0].SettingsRevision != "" {
		t.Fatalf("unconfirmed initial settings: %+v %v", initial, err)
	}
	var session protocol.Session
	_ = json.Unmarshal(discovered.Data, &session)
	sequence := uint64(1)
	emit := func(generation uint64, settings *protocol.SessionSettings) {
		t.Helper()
		sequence++
		session.Settings = settings
		// Saved usage can have a later observation time while still describing
		// an older turn; it is never the source of current preferences.
		session.Stats = &protocol.SessionStats{Model: "historical-model", ReasoningEffort: "ultra", LastMessage: "PRIVATE HISTORY", ObservedAt: time.Now().Add(time.Hour)}
		raw, _ := json.Marshal(session)
		event := protocol.Event{ID: uuid.NewString(), Seq: sequence, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: generation, SessionID: env.session.String(), Kind: "session_state_changed", OccurredAt: time.Now().UTC(), Data: raw}
		if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
			t.Fatal(err)
		}
	}
	emit(1, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 2, Model: "current-model", ReasoningEffort: "high"})
	changed, err := env.store.SessionActivitySince(ctx, initial.Revision)
	if err != nil || len(changed.Events) != 1 || changed.Events[0].Event != "session_settings_changed" || changed.Events[0].Session.Model != "current-model" || changed.Events[0].Session.ReasoningEffort != "high" || changed.Events[0].Session.SettingsRevision != "1:2" {
		t.Fatalf("confirmed settings missing: %+v %v", changed, err)
	}
	emit(1, nil)
	emit(1, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 1, Model: "older-model", ReasoningEffort: "low"})
	emit(1, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 2, Model: "same-revision-conflict", ReasoningEffort: "low"})
	stable, err := env.store.SessionActivitySince(ctx, changed.Snapshot.Revision)
	if err != nil || len(stable.Events) != 0 || stable.Snapshot.Sessions[0].Model != "current-model" || stable.Snapshot.Sessions[0].ReasoningEffort != "high" {
		t.Fatalf("old discovery/stats regressed preferences: %+v %v", stable, err)
	}
	emit(1, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 3, Model: "current-model", ReasoningEffort: ""})
	emit(1, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 4, Model: "next-model", ReasoningEffort: "low"})
	changed, err = env.store.SessionActivitySince(ctx, stable.Snapshot.Revision)
	if err != nil || len(changed.Events) != 2 || changed.Events[0].Session.ReasoningEffort != "" || changed.Events[0].Session.SettingsRevision != "1:3" || changed.Events[1].Session.Model != "next-model" || changed.Events[1].Session.SettingsRevision != "1:4" {
		t.Fatalf("coalesced settings/default effort transition lost: %+v %v", changed, err)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	emit(1, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 999, Model: "stale-runtime-model"})
	emit(2, &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 1000, Model: "carried-stale-model"})
	unknown, err := env.store.LiveSessionActivity(ctx)
	if err != nil || unknown.Sessions[0].SettingsRevision != "" || unknown.Sessions[0].Model != "" || unknown.Sessions[0].ReasoningEffort != "" {
		t.Fatalf("runtime restart retained stale settings: %+v %v", unknown, err)
	}
	emit(2, &protocol.SessionSettings{RuntimeGeneration: 2, Revision: 1, Model: "restarted-model"})
	refreshed, err := env.store.SessionActivitySince(ctx, unknown.Revision)
	if err != nil || len(refreshed.Events) != 1 || refreshed.Events[0].Session.SettingsRevision != "2:1" || refreshed.Events[0].Session.Model != "restarted-model" || refreshed.Events[0].Session.ReasoningEffort != "" {
		t.Fatalf("new generation did not reset revision: %+v %v", refreshed, err)
	}
}

func TestCurrentSessionSettingsPersistenceIsNarrowAndAtomic(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	event := env.discovery(t)
	var session protocol.Session
	_ = json.Unmarshal(event.Data, &session)
	session.Settings = &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 1, Model: "confirmed-model"}
	event.Data, _ = json.Marshal(session)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	// Initial browser connection obtains persisted settings even when no browser
	// was open to retain an in-memory notification when they were recorded.
	snapshot, err := env.store.LiveSessionActivity(ctx)
	if err != nil || snapshot.Sessions[0].Model != "confirmed-model" || snapshot.Sessions[0].SettingsRevision != "1:1" {
		t.Fatalf("persisted initial settings: %+v %v", snapshot, err)
	}
	event.ID, event.Seq, event.Kind = uuid.NewString(), 2, "session_state_changed"
	session.Name = "must rollback"
	session.Settings.Revision = 0
	event.Data, _ = json.Marshal(session)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err == nil {
		t.Fatal("invalid current settings accepted")
	}
	var name string
	if err := env.store.pool.QueryRow(ctx, `SELECT name FROM sessions WHERE session_id=$1`, env.session).Scan(&name); err != nil || name == "must rollback" {
		t.Fatalf("invalid settings partially applied: %q %v", name, err)
	}
}
