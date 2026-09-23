package worker

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func storedSettingsSession(t *testing.T, store *Store, id string) protocol.Session {
	t.Helper()
	sessions, err := store.ListSessions("")
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("session %s missing", id)
	return protocol.Session{}
}

func TestNativeSettingsObserverPublishesWithoutHistoryPolling(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "settings-thread", "active-turn")
	other := installSession(a, runtime, "other-thread", "")
	a.onSession(runtime, session) // Wait for initial actor recovery.
	client, _, _ := a.manager.Client(runtime.ID)
	stop := a.manager.forwardAdapter(a.ctx, runtime, client)
	defer stop()
	before := len(server.Calls())
	emit := func(thread, model string, effort any) {
		t.Helper()
		if err := server.Emit("thread/settings/updated", map[string]any{"threadId": thread, "threadSettings": map[string]any{"model": model, "effort": effort, "cwd": "/private-not-projected", "private": "not-projected"}}); err != nil {
			t.Fatal(err)
		}
	}
	emit(session.ThreadID, "current-model", "high")
	emit(session.ThreadID, "current-model", "high") // Permission-only updates are duplicates here.
	emit("untracked-thread", "unknown-model", "low")
	emit(session.ThreadID, "current-model", nil)
	waitFor(t, func() bool {
		current := storedSettingsSession(t, a.store, session.ID)
		return current.Settings != nil && current.Settings.Revision == 2
	})
	current := storedSettingsSession(t, a.store, session.ID)
	if current.Settings.Model != "current-model" || current.Settings.ReasoningEffort != "" || current.Settings.RuntimeGeneration != runtime.Generation || current.ActiveTurnID != "active-turn" {
		t.Fatalf("incorrect current preferences: %+v", current)
	}
	if storedSettingsSession(t, a.store, other.ID).Settings != nil {
		t.Fatal("settings changed another session")
	}
	if len(server.Calls()) != before {
		t.Fatalf("native preference notifications triggered extra RPCs: %+v", server.Calls()[before:])
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 2 {
		t.Fatalf("duplicate settings emitted another event: %d %v", len(events), err)
	}
	for index, event := range events {
		var snapshot protocol.Session
		if json.Unmarshal(event.Data, &snapshot) != nil || event.Kind != "session_state_changed" || snapshot.Settings == nil || snapshot.Settings.Revision != uint64(index+1) || snapshot.ActiveTurnID != "active-turn" {
			t.Fatalf("settings event %d: %s", index, event.Data)
		}
		if strings.Contains(string(event.Data), "not-projected") {
			t.Fatal("unrelated native settings leaked into the gateway event")
		}
	}
	// An older discovery result includes last turn statistics and cannot undo
	// the current choice, even when it arrives after the settings notification.
	session.Stats = &protocol.SessionStats{Model: "historical-model", ReasoningEffort: "low"}
	a.onSession(runtime, session)
	current = storedSettingsSession(t, a.store, session.ID)
	if current.Settings == nil || current.Settings.Revision != 2 || current.Settings.Model != "current-model" || current.Settings.ReasoningEffort != "" {
		t.Fatalf("history rolled current preferences back: %+v", current.Settings)
	}
	nextRuntime := runtime
	nextRuntime.Generation++
	a.onSession(nextRuntime, current)
	if got := storedSettingsSession(t, a.store, session.ID).Settings; got != nil {
		t.Fatalf("new runtime kept old generation settings: %+v", got)
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "thread_settings_updated", ThreadID: session.ThreadID, Settings: &codexadapter.CurrentThreadSettings{Model: "stale-runtime", ReasoningEffort: "low"}})
	a.onEvent(nextRuntime, codexadapter.Event{Kind: "thread_settings_updated", ThreadID: session.ThreadID, Settings: &codexadapter.CurrentThreadSettings{Model: "next-runtime", ReasoningEffort: "high"}})
	waitFor(t, func() bool {
		settings := storedSettingsSession(t, a.store, session.ID).Settings
		return settings != nil && settings.Model == "next-runtime"
	})
	if got := storedSettingsSession(t, a.store, session.ID).Settings; got.Revision != 1 || got.RuntimeGeneration != nextRuntime.Generation {
		t.Fatalf("runtime revision fence: %+v", got)
	}
}

func TestSessionSettingsPersistRevisionAndDurableEventTogether(t *testing.T) {
	file, workerID := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(file, workerID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.BeginRuntime("settings", "Settings", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "persisted", CWD: runtime.DefaultCWD, State: "idle"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.recordSessionSettings(runtime, session, codexadapter.CurrentThreadSettings{Model: "first", ReasoningEffort: "high"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(file, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.recordSessionSettings(runtime, session, codexadapter.CurrentThreadSettings{Model: "second"}); err != nil {
		t.Fatal(err)
	}
	saved := storedSettingsSession(t, store, session.ID)
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 2 || saved.Settings.Revision != 2 || saved.Settings.Model != "second" {
		t.Fatalf("settings/event persistence mismatch: %+v events=%d err=%v", saved.Settings, len(events), err)
	}
	archived, changed, err := store.ArchiveDiscoveredSession(runtime, saved)
	if err != nil || !changed {
		t.Fatalf("archive settings fixture: changed=%v err=%v", changed, err)
	}
	candidate := archived
	candidate.Archived, candidate.Settings = false, nil
	saved, changed, err = store.RestoreDiscoveredSession(runtime, archived, candidate)
	if err != nil || !changed || saved.Settings == nil || saved.Settings.Revision != 2 || saved.Settings.Model != "second" {
		t.Fatalf("restoring discovery metadata erased current preferences: %+v changed=%v err=%v", saved.Settings, changed, err)
	}
	wrong := session
	wrong.ID = uuid.NewString()
	if _, err := store.recordSessionSettings(runtime, wrong, codexadapter.CurrentThreadSettings{Model: "bad-target"}); err == nil {
		t.Fatal("settings accepted a changed session identity")
	}
	saved.Settings.Revision = math.MaxInt64
	if _, err := store.UpsertSession(saved); err != nil {
		t.Fatal(err)
	}
	if _, err := store.recordSessionSettings(runtime, saved, codexadapter.CurrentThreadSettings{Model: "overflow"}); err == nil {
		t.Fatal("settings revision overflow was accepted")
	}
	events, err = store.OutboxAfter(0)
	if err != nil || len(events) != 4 || storedSettingsSession(t, store, session.ID).Settings.Model != "second" {
		t.Fatal("rejected settings changed persistent state or emitted an event")
	}
}

func TestCodexStatusAndReasoningUseConfirmedPreferences(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{{"id": "settings-status", "cwd": runtime.DefaultCWD, "model": "old-model", "reasoningEffort": "low", "status": "idle"}}, nil)
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "settings-status", CWD: runtime.DefaultCWD, State: "idle", Loaded: true})
	if err != nil {
		t.Fatal(err)
	}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	actor.observeThreadSettings(codexadapter.Event{Kind: "thread_settings_updated", ThreadID: session.ThreadID, Settings: &codexadapter.CurrentThreadSettings{Model: "current-model"}})
	client, _, _ := a.manager.Client(runtime.ID)
	text, err := actor.codexStatus(t.Context(), client)
	if err != nil || !strings.Contains(text, "Model: current-model") || !strings.Contains(text, "Reasoning: default") {
		t.Fatalf("stale status: %s %v", text, err)
	}
	if err := server.SetMethodResult("model/list", map[string]any{"data": []map[string]any{{"id": "current-model", "supportedReasoningEfforts": []map[string]string{{"reasoningEffort": "high"}}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.codexReasoning(t.Context(), client, "high"); err != nil {
		t.Fatalf("reasoning validated the old model: %v", err)
	}
	if actor.session.Settings.ReasoningEffort != "" || actor.session.Settings.Revision != 1 {
		t.Fatal("command acknowledgment synthesized a settings event before the ordered native notification")
	}
}

func TestRuntimeMetadataUpsertPreservesSettingsRevision(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	metadata := protocol.Session{RuntimeID: runtime.ID, ThreadID: "creation-race", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}
	session, err := a.store.UpsertRuntimeSession(runtime, metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"first", "second"} {
		if _, err := a.store.recordSessionSettings(runtime, session, codexadapter.CurrentThreadSettings{Model: model}); err != nil {
			t.Fatal(err)
		}
	}
	// A late start/fork response has no preference fields and may arrive after
	// thread/started plus two settings broadcasts have already been persisted.
	session, err = a.store.UpsertRuntimeSession(runtime, metadata)
	if err != nil || session.Settings == nil || session.Settings.Revision != 2 || session.Settings.Model != "second" {
		t.Fatalf("metadata erased a concurrent preference revision: %+v %v", session.Settings, err)
	}
	settings, err := a.store.recordSessionSettings(runtime, session, codexadapter.CurrentThreadSettings{Model: "third", ReasoningEffort: "high"})
	if err != nil || settings.Revision != 3 {
		t.Fatalf("metadata write caused preference revision reuse: %+v %v", settings, err)
	}
	stale := session
	stale.Settings = &protocol.SessionSettings{RuntimeGeneration: runtime.Generation, Revision: 1, Model: "old"}
	updated, err := a.store.UpsertRuntimeSession(runtime, stale)
	if err != nil || updated.Settings.Revision != 3 || updated.Settings.Model != "third" {
		t.Fatalf("stale metadata overwrote a newer settings record: %+v %v", updated.Settings, err)
	}
	next := runtime
	next.Generation++
	updated, err = a.store.UpsertRuntimeSession(next, metadata)
	if err != nil || updated.Settings != nil {
		t.Fatalf("new runtime inherited previous generation settings: %+v %v", updated.Settings, err)
	}
	settings, err = a.store.recordSessionSettings(next, updated, codexadapter.CurrentThreadSettings{Model: "next-generation"})
	if err != nil || settings.Revision != 1 || settings.RuntimeGeneration != next.Generation {
		t.Fatalf("new runtime did not start its own settings revision: %+v %v", settings, err)
	}
	updated, err = a.store.UpsertRuntimeSession(runtime, metadata)
	if err != nil || updated.Settings == nil || updated.Settings.RuntimeGeneration != next.Generation || updated.Settings.Model != "next-generation" {
		t.Fatalf("late old-runtime metadata erased current preferences: %+v %v", updated.Settings, err)
	}
	archived, changed, err := a.store.ArchiveDiscoveredSession(runtime, updated)
	if err != nil || changed || archived.Archived || archived.Settings == nil || archived.Settings.RuntimeGeneration != next.Generation {
		t.Fatalf("old runtime archived a session with current settings: %+v changed=%v err=%v", archived, changed, err)
	}
}
