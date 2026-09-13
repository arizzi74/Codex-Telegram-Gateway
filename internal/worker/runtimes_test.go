package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
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
