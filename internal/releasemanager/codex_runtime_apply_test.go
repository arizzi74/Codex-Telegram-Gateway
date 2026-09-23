package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type codexApplyFixture struct {
	t                    *testing.T
	manager              *Manager
	layout               *Layout
	plan                 codexRuntimePlan
	now                  time.Time
	running              bool
	pid                  int
	version, nextVersion string
	events               []string
	busy, stopStaysLive  bool
	readinessFault       string
	nativeFault          string
	cancel               context.CancelFunc
	preflightFault       bool
	busyAfterStart       bool
}

func newCodexApplyFixture(t *testing.T) *codexApplyFixture {
	t.Helper()
	l, _, _ := updateFixture(t, "worker")
	f := &codexApplyFixture{t: t, layout: l, now: time.Now().UTC(), running: true, pid: 123, nextVersion: "0.157.0"}
	f.plan = codexRuntimePlan{
		Distribution:  &codexDistribution{Binary: filepath.Join(l.Bin, "codex"), Home: filepath.Join(l.Home, ".codex"), InstallDir: l.Bin, Version: "0.154.0"},
		TargetVersion: "0.156.0", NeedsInstall: true,
		Profiles: map[string]bool{"main": true, "manual": false},
	}
	f.installVersion("0.154.0")
	if err := os.Symlink(filepath.Join(f.plan.Distribution.Home, "packages", "standalone", "current", "bin", "codex"), f.plan.Distribution.Binary); err != nil {
		t.Fatal(err)
	}
	cfg, _ := ReadJSON(l.Config)
	cfg["runtimes"] = []any{map[string]any{"id": "main", "autostart": true}, map[string]any{"id": "manual", "autostart": false}}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	f.manager = &Manager{Run: f.run, CodexRun: f.update, Now: func() time.Time { return f.now }, ReadyTimeout: 10 * time.Millisecond, PollInterval: time.Millisecond}
	f.manager.codexPreflight = func(_ context.Context, _ *codexDistribution) error {
		f.events = append(f.events, "preflight")
		if f.preflightFault {
			return errors.New("candidate compatibility failure")
		}
		return nil
	}
	return f
}

func (f *codexApplyFixture) installVersion(version string) {
	f.t.Helper()
	standalone := filepath.Join(f.plan.Distribution.Home, "packages", "standalone")
	release := filepath.Join(standalone, "releases", version+"-aarch64-unknown-linux-musl")
	if err := os.MkdirAll(filepath.Join(release, "bin"), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(release, "bin", "codex"), []byte("fake native executable"), 0700); err != nil {
		f.t.Fatal(err)
	}
	metadata := map[string]any{"layoutVersion": 1, "version": version, "target": "aarch64-unknown-linux-musl", "variant": "codex", "entrypoint": "bin/codex"}
	if err := WriteJSON(filepath.Join(release, "codex-package.json"), metadata); err != nil {
		f.t.Fatal(err)
	}
	current := filepath.Join(standalone, "current")
	if err := os.Remove(current); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	if err := os.Symlink(release, current); err != nil {
		f.t.Fatal(err)
	}
	f.version = version
}

func (f *codexApplyFixture) run(ctx context.Context, args ...string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	if args[0] == f.plan.Distribution.Binary {
		f.events = append(f.events, "inspect")
		f.version = f.installedVersion()
		return CommandResult{Output: []byte("codex-cli " + f.version)}, nil
	}
	if args[0] == "systemctl" {
		switch args[2] {
		case "show":
			if args[len(args)-1] == "--value" {
				return CommandResult{Output: []byte(strconv.Itoa(f.pid) + "\n")}, nil
			}
			if !f.running {
				return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
			}
			pid := "123"
			if f.pid == 456 {
				pid = "456"
			}
			return CommandResult{Output: []byte("MainPID=" + pid + "\nActiveState=active\n")}, nil
		case "stop":
			f.events = append(f.events, "stop")
			f.running = f.stopStaysLive
			return CommandResult{}, nil
		case "start":
			f.events = append(f.events, "start")
			f.running, f.pid = true, 456
			return CommandResult{}, nil
		}
	}
	if args[0] == f.layout.Binary {
		if len(args) == 2 && args[1] == "version" {
			return CommandResult{Output: []byte("0.5.43")}, nil
		}
		if len(args) >= 5 && args[3] == "update" {
			f.events = append(f.events, args[4])
			if args[4] == "abort" {
				return CommandResult{}, nil
			}
			if f.busy || (f.busyAfterStart && f.pid == 456) {
				return CommandResult{Output: []byte(`{"error":"worker update: session is busy"}`), ExitCode: 1}, nil
			}
			data, _ := json.Marshal(workerLease{WorkerID: "worker-id", PID: f.pid, Token: "private-token", ExpiresAt: f.now.Add(2 * time.Minute)})
			return CommandResult{Output: data}, nil
		}
		if args[3] == "status" {
			if !f.running {
				return CommandResult{ExitCode: 1}, nil
			}
			return CommandResult{Output: f.status()}, nil
		}
	}
	f.t.Fatalf("unexpected command: %v", args)
	return CommandResult{}, nil
}

func (f *codexApplyFixture) status() []byte {
	runtime := map[string]any{"profile_id": "main", "state": "running", "pid": 789, "codex_version": "codex-cli " + f.version}
	status := map[string]any{"worker_id": "worker-id", "pid": f.pid, "updated_at": f.now, "gateway_connected": true, "runtimes": []any{runtime}}
	fault := f.readinessFault
	if f.installedVersion() == "0.154.0" {
		fault = ""
	}
	switch fault {
	case "version":
		runtime["codex_version"] = "codex-cli 0.154.0"
	case "profile":
		runtime["profile_id"] = "other"
	case "runtime-pid":
		runtime["pid"] = 0
	case "worker-pid":
		status["pid"] = 999
	case "worker-id":
		status["worker_id"] = "other"
	case "stale":
		status["updated_at"] = f.now.Add(-time.Minute)
	case "degraded":
		runtime["state"] = "degraded"
	case "disconnected":
		status["gateway_connected"] = false
	case "duplicate":
		status["runtimes"] = []any{runtime, runtime}
	}
	data, _ := json.Marshal(status)
	return data
}

func (f *codexApplyFixture) update(ctx context.Context, _ ...string) (CommandResult, error) {
	f.events = append(f.events, "native-update")
	if f.running {
		f.t.Fatal("native updater ran while worker was running")
	}
	if strings.HasPrefix(f.nativeFault, "partial-") {
		f.installVersion(f.nextVersion)
		if f.nativeFault == "partial-cancel" {
			f.cancel()
			return CommandResult{}, ctx.Err()
		}
		return CommandResult{ExitCode: 1}, nil
	}
	if f.nativeFault == "removed-current" {
		if err := os.Remove(filepath.Join(f.plan.Distribution.Home, "packages", "standalone", "current")); err != nil {
			f.t.Fatal(err)
		}
		return CommandResult{ExitCode: 1}, nil
	}
	if f.nativeFault == "cancel" {
		f.cancel()
		return CommandResult{}, ctx.Err()
	}
	if f.nativeFault == "failure" {
		return CommandResult{ExitCode: 1}, nil
	}
	f.installVersion(f.nextVersion)
	return CommandResult{}, nil
}

func (f *codexApplyFixture) record() error {
	f.events = append(f.events, "record-restart")
	return nil
}

func TestCodexRuntimeApplyFencesWorkAndChecksReadiness(t *testing.T) {
	f := newCodexApplyFixture(t)
	if err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record); err != nil {
		t.Fatal(err)
	}
	want := []string{"inspect", "prepare", "record-restart", "stop", "inspect", "native-update", "inspect", "preflight", "start"}
	if !slices.Equal(f.events, want) || !f.running || f.version != "0.157.0" {
		t.Fatalf("events=%v running=%v version=%s", f.events, f.running, f.version)
	}
}

func TestCodexRuntimeApplyBusyDoesNotStopOrInstall(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.busy = true
	err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record)
	var busy *BusyError
	if !errors.As(err, &busy) || !slices.Equal(f.events, []string{"inspect", "prepare"}) || !f.running {
		t.Fatalf("events=%v running=%v err=%v", f.events, f.running, err)
	}
}

func TestCodexRuntimeApplyRecoversNativeFailureAndCancellation(t *testing.T) {
	for _, fault := range []string{"failure", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.cancel, f.nativeFault = cancel, fault
			err := f.manager.applyCodexRuntimeUpdates(ctx, f.layout, []codexRuntimePlan{f.plan}, false, f.record)
			if err == nil || !f.running || f.events[len(f.events)-1] != "start" || slices.Contains(f.events, "abort") {
				t.Fatalf("events=%v running=%v err=%v", f.events, f.running, err)
			}
		})
	}
}

func TestCodexRuntimeApplyRestartOnlyDoesNotDowngrade(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.installVersion("0.158.0")
	f.plan.NeedsInstall = false
	if err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.events, "native-update") || !slices.Contains(f.events, "stop") || !slices.Contains(f.events, "start") || f.version != "0.158.0" {
		t.Fatalf("events=%v version=%s", f.events, f.version)
	}
}

func TestCodexRuntimeApplyPreservesStoppedService(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "leave-stopped", true: "recover-pending-restart"}[resume], func(t *testing.T) {
			f := newCodexApplyFixture(t)
			f.running = false
			if err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, resume, f.record); err != nil {
				t.Fatal(err)
			}
			if f.running != resume || slices.Contains(f.events, "prepare") || slices.Contains(f.events, "stop") || slices.Contains(f.events, "record-restart") != resume {
				t.Fatalf("events=%v running=%v", f.events, f.running)
			}
		})
	}
}

func TestCodexRuntimeApplyAbortsUnusedLease(t *testing.T) {
	for _, fault := range []string{"record", "expired", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			record := func() error {
				if fault == "record" {
					return errors.New("state write failed")
				}
				if fault == "expired" {
					f.now = f.now.Add(90 * time.Second)
				} else {
					cancel()
				}
				return nil
			}
			err := f.manager.applyCodexRuntimeUpdates(ctx, f.layout, []codexRuntimePlan{f.plan}, false, record)
			if err == nil || !slices.Contains(f.events, "abort") || slices.Contains(f.events, "stop") || !f.running {
				t.Fatalf("events=%v running=%v err=%v", f.events, f.running, err)
			}
		})
	}
}

func TestCodexRuntimeApplyRequiresConfirmedStop(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.stopStaysLive = true
	err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record)
	if err == nil || slices.Contains(f.events, "native-update") || !slices.Contains(f.events, "abort") || !f.running {
		t.Fatalf("events=%v running=%v err=%v", f.events, f.running, err)
	}
}

func TestCodexRuntimeApplyValidatesInstallationBeforeStopping(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.plan.Distribution.Home += "-changed"
	err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record)
	if err == nil || slices.Contains(f.events, "prepare") || slices.Contains(f.events, "stop") {
		t.Fatalf("events=%v err=%v", f.events, err)
	}
}

func TestCodexRuntimeApplyRetainsReadinessFailure(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.readinessFault = "version"
	err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") || !f.running {
		t.Fatalf("events=%v running=%v err=%v", f.events, f.running, err)
	}
}

func TestCodexRuntimeReadyRequiresFreshManagedProfiles(t *testing.T) {
	for _, fault := range []string{"", "version", "profile", "runtime-pid", "worker-pid", "worker-id", "stale", "degraded", "disconnected", "duplicate"} {
		t.Run(fault, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			f.installVersion("0.157.0")
			f.readinessFault = fault
			err := f.manager.codexRuntimeReady(t.Context(), f.layout, f.now, map[string]string{"main": "0.157.0"})
			if (err != nil) != (fault != "") {
				t.Fatalf("fault=%s err=%v", fault, err)
			}
		})
	}
}

func (f *codexApplyFixture) installedVersion() string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.plan.Distribution.Home, "packages", "standalone", "current", "codex-package.json"))
	if err != nil {
		return "unavailable"
	}
	var value struct {
		Version string `json:"version"`
	}
	if err = json.Unmarshal(data, &value); err != nil {
		f.t.Fatal(err)
	}
	return value.Version
}
