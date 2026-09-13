package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestRuntimeManagerRejectsWorkspaceOutsideLocalAllowlist(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = NewRuntimeManager(config.WorkerConfig{AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "primary", WorkingDirectory: outside}}}, store, nil, RuntimeHooks{})
	if err == nil {
		t.Fatal("outside runtime workspace accepted")
	}
}

func TestRuntimeManagerPersistsGenerationBeforeFailedSpawnAndSignalsReady(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.WorkerConfig{AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "primary", Name: "Primary", CodexBinary: "missing", WorkingDirectory: root, Autostart: true, RestartPolicy: "never"}}}
	m, err := NewRuntimeManager(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), RuntimeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	m.start = func(context.Context, codexadapter.Config) (*codexadapter.Client, error) {
		return nil, errors.New("fake start failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	select {
	case <-m.Ready():
	case <-time.After(time.Second):
		t.Fatal("manager did not finish initial startup attempt")
	}
	runtime, found, err := store.RuntimeForProfile("primary")
	if err != nil || !found || runtime.Generation != 1 || runtime.State != "starting" {
		t.Fatalf("generation was not committed before spawn: %#v found=%t err=%v", runtime, found, err)
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "runtime_failed" {
		t.Fatalf("failed spawn was not durably reported: %#v, %v", events, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRestartPolicyCapsFailures(t *testing.T) {
	var attempts []time.Time
	now := time.Now()
	for range 5 {
		if !shouldRestart("on-failure", &attempts, now) {
			t.Fatal("restart unexpectedly denied")
		}
	}
	if shouldRestart("on-failure", &attempts, now) {
		t.Fatal("sixth restart within window allowed")
	}
	if shouldRestart("never", &attempts, now) {
		t.Fatal("never policy restarted")
	}
}

func TestRuntimeDiscoverySubscribesOnlyLoadedWorkspaceThreadsOnce(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewRuntimeManager(config.WorkerConfig{AllowedWorkspaceRoots: []string{root}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)), RuntimeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.BeginRuntime("main", "Main", root)
	if err != nil {
		t.Fatal(err)
	}
	client, fixture, err := codextest.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	fixture.SetThreads([]map[string]any{
		{"id": "loaded-a", "cwd": root, "name": "A", "status": "idle"},
		{"id": "loaded-b", "cwd": root, "name": "B", "status": "active", "turns": []map[string]any{{"id": "turn-b", "status": "inProgress"}}},
		{"id": "loaded-c", "cwd": root, "name": "C", "status": "idle"},
		{"id": "persisted-cold", "cwd": root, "name": "Cold", "status": "idle"},
	}, []string{"loaded-a", "loaded-b", "loaded-c"})
	m.install(runtime, client)
	if err := m.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 4 {
		t.Fatalf("reconciled sessions = %#v, %v", sessions, err)
	}
	var active protocol.Session
	for _, session := range sessions {
		if session.ThreadID == "loaded-b" {
			active = session
		}
	}
	if active.State != "running" || active.ActiveTurnID != "turn-b" || !active.Loaded {
		t.Fatalf("active loaded session = %#v", active)
	}
	assertResumeCalls(t, fixture.Calls(), 3)
	if err := m.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	assertResumeCalls(t, fixture.Calls(), 3)
}

func TestRuntimeDiscoveryUnavailableResumeDegradesWithoutFatalHook(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fatal := make(chan error, 1)
	m, err := NewRuntimeManager(config.WorkerConfig{AllowedWorkspaceRoots: []string{root}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)), RuntimeHooks{OnError: func(err error) { fatal <- err }})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.BeginRuntime("main", "Main", root)
	if err != nil {
		t.Fatal(err)
	}
	client, fixture, err := codextest.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	fixture.SetThreads([]map[string]any{{"id": "loaded", "cwd": root, "name": "Loaded", "status": "idle"}}, []string{"loaded"})
	fixture.SetMethodUnavailable("thread/resume", true)
	m.install(runtime, client)
	if err := m.discover(context.Background(), runtime, client); err != nil {
		t.Fatalf("unavailable resume should degrade, not fail discovery: %v", err)
	}
	if snapshot := m.Snapshot(); len(snapshot) != 1 || snapshot[0].State != "degraded" {
		t.Fatalf("runtime did not degrade: %#v", snapshot)
	}
	select {
	case err := <-fatal:
		t.Fatalf("discovery RPC incorrectly called fatal hook: %v", err)
	default:
	}
	events, err := store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	degraded := 0
	for _, event := range events {
		if event.Kind == "runtime_degraded" {
			degraded++
		}
	}
	if degraded != 1 {
		t.Fatalf("runtime_degraded events = %d", degraded)
	}
	assertResumeCalls(t, fixture.Calls(), 1)
	if err := m.discover(context.Background(), runtime, client); err != nil {
		t.Fatal(err)
	}
	assertResumeCalls(t, fixture.Calls(), 1)
}

func TestRuntimeClientCrashStartsNextGeneration(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.WorkerConfig{AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "main", Name: "Main", WorkingDirectory: root, Autostart: true, RestartPolicy: "on-failure"}}}
	m, err := NewRuntimeManager(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)), RuntimeHooks{})
	if err != nil {
		t.Fatal(err)
	}
	clients := make(chan *codexadapter.Client, 2)
	m.start = func(ctx context.Context, _ codexadapter.Config) (*codexadapter.Client, error) {
		client, fixture, err := codextest.New(ctx)
		if err != nil {
			return nil, err
		}
		fixture.SetThreads(nil, nil)
		clients <- client
		return client, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	first := <-clients
	waitRuntime(t, m, 1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	<-clients
	waitRuntime(t, m, 2)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime manager did not stop after client crash")
	}
}

func assertResumeCalls(t *testing.T, calls []codextest.Call, want int) {
	t.Helper()
	got := 0
	for _, call := range calls {
		if call.Method != "thread/resume" {
			continue
		}
		got++
		var params map[string]any
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatal(err)
		}
		if len(params) != 1 || params["threadId"] == "" {
			t.Fatalf("resume received non-empty options: %s", call.Params)
		}
		if params["threadId"] == "persisted-cold" {
			t.Fatal("cold persisted thread was resumed")
		}
	}
	if got != want {
		t.Fatalf("thread/resume calls = %d, want %d; calls=%#v", got, want, calls)
	}
}

func waitRuntime(t *testing.T, manager *RuntimeManager, generation uint64) {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		snapshot := manager.Snapshot()
		if len(snapshot) == 1 && snapshot[0].Generation == generation && snapshot[0].State == "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime generation %d did not become running: %#v", generation, manager.Snapshot())
}
