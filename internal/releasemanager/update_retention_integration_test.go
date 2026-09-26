package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func retentionIntegrationHistory(t *testing.T, l *Layout) []string {
	t.Helper()
	var paths []string
	for index, version := range []string{"v0.6.0", "v0.7.0", "v0.8.0", "v0.9.0"} {
		path := filepath.Join(l.Backups, fmt.Sprintf("202609%02dT120000Z-%d", index+1, index+1000))
		if err := WriteJSON(filepath.Join(path, "00-update.json"), map[string]any{"schema": 1, "component": l.Component, "repo": DefaultRepo, "version": version}); err != nil {
			t.Fatal(err)
		}
		files := []string{"02-" + filepath.Base(l.Binary), "03-codex-telegramgw", "04-codex-local"}
		if l.Component == "gateway" {
			files[2] = "gateway.db"
		}
		for _, file := range files {
			if err := os.WriteFile(filepath.Join(path, file), []byte("previous snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		paths = append(paths, path)
	}
	return paths
}

func retentionIntegrationCheckPaths(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("history changed before successful update: %s: %v", path, err)
		}
	}
}

func retentionIntegrationVersions(t *testing.T, l *Layout) []string {
	t.Helper()
	entries, err := os.ReadDir(l.Backups)
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, entry := range entries {
		state, err := ReadJSON(filepath.Join(l.Backups, entry.Name(), "00-update.json"))
		if err != nil {
			t.Fatal(err)
		}
		version, _ := state["version"].(string)
		versions = append(versions, version)
	}
	sort.Strings(versions)
	return versions
}

func TestUpdateRetentionWaitsForReadinessAndSavedSettings(t *testing.T) {
	for _, failure := range []string{"", "readiness", "settings"} {
		t.Run(map[string]string{"": "success", "readiness": "readiness-failure", "settings": "settings-failure"}[failure], func(t *testing.T) {
			l, packages, release := updateFixture(t, "worker")
			if err := os.WriteFile(filepath.Join(l.Bin, "codex-local"), []byte("old local"), 0o755); err != nil {
				t.Fatal(err)
			}
			history := retentionIntegrationHistory(t, l)
			started, checkedReady := false, false
			m := &Manager{ReadyTimeout: 10 * time.Millisecond, PollInterval: time.Millisecond}
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "systemctl" {
					if args[2] == "show" {
						return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
					}
					if args[2] != "start" {
						t.Fatalf("unexpected service action: %v", args)
					}
					started = true
					return CommandResult{}, nil
				}
				if args[0] == l.Binary && args[3] == "status" {
					if !started {
						return CommandResult{ExitCode: 1}, nil
					}
					retentionIntegrationCheckPaths(t, history)
					if !checkedReady {
						state, err := SavedSettings(l)
						if err != nil || state["pending"] != true {
							t.Fatalf("update was committed before readiness: %v, %v", state, err)
						}
						if failure == "settings" {
							if err := os.Remove(l.State); err != nil {
								t.Fatal(err)
							}
							if err := os.Mkdir(l.State, 0o700); err != nil {
								t.Fatal(err)
							}
						}
					}
					checkedReady = true
					data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": failure != "readiness", "version": release.Tag, "runtimes": []any{}})
					return CommandResult{Output: data}, nil
				}
				t.Fatalf("unexpected command: %v", args)
				return CommandResult{}, nil
			}
			err := m.ApplyUpdate(t.Context(), l, packages, release, false)
			if !checkedReady || (err != nil) != (failure != "") {
				t.Fatalf("readiness checked=%v, update error=%v", checkedReady, err)
			}
			if failure != "" {
				retentionIntegrationCheckPaths(t, history)
				if versions := retentionIntegrationVersions(t, l); len(versions) != 5 {
					t.Fatalf("failed update lost recovery history: %v", versions)
				}
				return
			}
			state, err := SavedSettings(l)
			if err != nil || state["pending"] != nil || state["version"] != release.Tag {
				t.Fatalf("successful update did not save settings: %v, %v", state, err)
			}
			if got, want := retentionIntegrationVersions(t, l), []string{"v0.8.0", "v0.9.0", "v1.0.0"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("retained versions=%v, want %v", got, want)
			}
		})
	}
}

func TestUpdateRetentionPreservesHistoryAfterMigrationFailure(t *testing.T) {
	l, packages, release := updateFixture(t, "gateway")
	history := retentionIntegrationHistory(t, l)
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "runuser" {
			return CommandResult{ExitCode: 1}, nil
		}
		if args[0] != "systemctl" {
			t.Fatalf("unexpected command: %v", args)
		}
		return CommandResult{}, nil
	}}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err == nil {
		t.Fatal("migration failure was ignored")
	}
	retentionIntegrationCheckPaths(t, history)
	if versions := retentionIntegrationVersions(t, l); len(versions) != 5 {
		t.Fatalf("failed migration lost recovery history: %v", versions)
	}
}

func TestUpdateRetentionBusyPreparationCreatesNoBackup(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "systemctl" && args[2] == "show" {
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		}
		if args[0] == l.Binary && args[3] == "update" && args[4] == "prepare" {
			return CommandResult{ExitCode: 75, Output: []byte(`{"error":"worker update: session is busy"}`)}, nil
		}
		t.Fatalf("busy preparation changed worker: %v", args)
		return CommandResult{}, nil
	}}
	for range 3 {
		var busy *BusyError
		if err := m.ApplyUpdate(t.Context(), l, packages, release, false); !errors.As(err, &busy) {
			t.Fatalf("expected busy update: %v", err)
		}
	}
	entries, err := os.ReadDir(l.Backups)
	if len(entries) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy preparation created backups: %v, %v", entries, err)
	}
}

func TestUpdateRetentionSnapshotFailureAbortsPreparedLease(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	if err := os.Remove(l.Manager); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(l.Manager, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, aborted := 0, 0
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "systemctl" && args[2] == "show" {
			if args[len(args)-1] == "--value" {
				return CommandResult{Output: []byte("123\n")}, nil
			}
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		}
		if args[0] == l.Binary && args[3] == "update" {
			if args[4] == "prepare" {
				prepared++
				data, _ := json.Marshal(workerLease{WorkerID: "worker-id", PID: 123, Token: "lease", ExpiresAt: time.Now().Add(2 * time.Minute)})
				return CommandResult{Output: data}, nil
			}
			if args[4] == "abort" {
				aborted++
				return CommandResult{}, nil
			}
		}
		t.Fatalf("snapshot failure changed worker: %v", args)
		return CommandResult{}, nil
	}}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err == nil {
		t.Fatal("snapshot failure was ignored")
	}
	if prepared != 1 || aborted != 1 {
		t.Fatalf("snapshot failure leaked preparation: prepare=%d abort=%d", prepared, aborted)
	}
	if got := updateRead(t, l.Binary); got != "old binary" {
		t.Fatalf("snapshot failure replaced binary: %s", got)
	}
}

func TestRequestedWorkerPendingRecoveryPrunesOnlyAfterReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			l, _, release := updateFixture(t, "worker")
			history := retentionIntegrationHistory(t, l)
			if err := WriteJSON(l.State, map[string]any{"schema": 1, "component": "worker", "repo": release.Repo, "version": release.Tag, "pending": true}); err != nil {
				t.Fatal(err)
			}
			m := &Manager{ReadyTimeout: 10 * time.Millisecond, PollInterval: time.Millisecond}
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				if reflect.DeepEqual(args, []string{l.Binary, "version"}) {
					return CommandResult{Output: []byte(release.Tag)}, nil
				}
				if reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}) {
					retentionIntegrationCheckPaths(t, history)
					data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": ready, "version": release.Tag, "runtimes": []any{}})
					return CommandResult{Output: data}, nil
				}
				t.Fatalf("pending recovery changed worker: %v", args)
				return CommandResult{}, nil
			}
			_, err := m.requestedWorkerReleaseStep(t.Context(), l, &requestedWorkerPlan{release: release})
			if (err == nil) != ready {
				t.Fatalf("ready=%v: %v", ready, err)
			}
			state, err := SavedSettings(l)
			if err != nil || (state["pending"] == true) == ready {
				t.Fatalf("pending recovery state=%v, err=%v", state, err)
			}
			if !ready {
				retentionIntegrationCheckPaths(t, history)
			} else if got, want := retentionIntegrationVersions(t, l), []string{"v0.7.0", "v0.8.0", "v0.9.0"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("recovered update retained=%v, want %v", got, want)
			}
		})
	}
}

func TestManagerPendingRecoveryPrunesAfterReadiness(t *testing.T) {
	f := newCodexWiringFixture(t)
	history := retentionIntegrationHistory(t, f.l)
	if err := WriteJSON(f.l.Config, map[string]any{"worker_id": "example-worker"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(f.l.State, map[string]any{"schema": 1, "component": "worker", "repo": DefaultRepo, "version": f.projectTag, "pending": true}); err != nil {
		t.Fatal(err)
	}
	oldRun := f.m.Run
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{f.l.Binary, "--config", f.l.Config, "status"}) {
			retentionIntegrationCheckPaths(t, history)
			data, _ := json.Marshal(map[string]any{"updated_at": f.m.Now(), "gateway_connected": true, "version": f.projectTag, "runtimes": []any{}})
			return CommandResult{Output: data}, nil
		}
		return oldRun(ctx, args...)
	}
	if err := f.m.execute(t.Context(), options{Action: "update", Component: "worker"}); err != nil {
		t.Fatal(err)
	}
	if got, want := retentionIntegrationVersions(t, f.l), []string{"v0.7.0", "v0.8.0", "v0.9.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered update retained=%v, want %v", got, want)
	}
}
