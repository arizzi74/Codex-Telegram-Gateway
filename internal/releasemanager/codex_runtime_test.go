package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type codexScheduleFixture struct {
	m         *Manager
	l         *Layout
	now       time.Time
	latest    string
	running   string
	failCheck bool
	checks    int
	prepares  int
	output    bytes.Buffer
}

func newCodexScheduleFixture(t *testing.T) *codexScheduleFixture {
	t.Helper()
	m, binary, _, _ := codexDistributionFixture(t)
	l, _, _ := updateFixture(t, "worker")
	cfg := map[string]any{"worker_id": "worker-id", "runtimes": []any{
		map[string]any{"id": "first", "codex_binary": binary, "autostart": true},
		map[string]any{"id": "second", "codex_binary": binary, "autostart": true},
	}}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	f := &codexScheduleFixture{m: m, l: l, now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), latest: "0.156.0", running: "0.155.0"}
	m.Now = func() time.Time { return f.now }
	m.Out = &f.output
	m.codexPreflight = func(context.Context, *codexDistribution) error { return nil }
	m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
		f.checks++
		if request.URL.String() != codexLatestURL && request.URL.String() != codexLatestFallbackURL {
			t.Fatalf("unexpected network request: %s", request.URL)
		}
		if f.failCheck {
			return nil, errors.New("unavailable")
		}
		return coreResponse(request, []byte(`{"tag_name":"rust-v`+f.latest+`"}`)), nil
	})
	m.CodexRun = func(context.Context, ...string) (CommandResult, error) {
		t.Fatal("a scheduler check or busy worker invoked the native installer")
		return CommandResult{}, nil
	}
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		switch {
		case reflect.DeepEqual(args, []string{binary, "--version"}):
			return CommandResult{Output: []byte("codex-cli 0.155.0")}, nil
		case reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState"}):
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}):
			data, _ := json.Marshal(map[string]any{"worker_id": "worker-id", "pid": 123, "updated_at": f.now, "runtimes": []any{
				map[string]any{"profile_id": "first", "codex_version": "codex-cli " + f.running, "state": "running"},
				map[string]any{"profile_id": "second", "codex_version": "codex-cli " + f.running, "state": "running"},
			}})
			return CommandResult{Output: data}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "update", "prepare"}):
			f.prepares++
			return CommandResult{ExitCode: 75, Output: []byte(`{"error":"worker update: a session has an active turn or pending response"}`)}, nil
		default:
			t.Fatalf("unexpected command or service mutation: %v", args)
			return CommandResult{}, nil
		}
	}
	return f
}

func TestCodexChecksDailyButRetriesPendingUpdateOnEveryWorkerTick(t *testing.T) {
	f := newCodexScheduleFixture(t)
	for i := 0; i < 3; i++ {
		if i > 0 {
			f.now = f.now.Add(5 * time.Minute)
		}
		err := f.m.UpdateCodexRuntime(t.Context(), f.l, false)
		var busy *BusyError
		if !errors.As(err, &busy) {
			t.Fatalf("tick %d did not preserve busy deferral: %v", i, err)
		}
		if f.checks != 1 || f.prepares != i+1 {
			t.Fatalf("tick %d: checks=%d prepares=%d", i, f.checks, f.prepares)
		}
	}
	state, err := loadCodexUpdateState(f.l)
	if err != nil || state.LatestVersion != "0.156.0" || state.CheckFailed || state.RestartPending {
		t.Fatalf("pending cached release lost: %+v %v", state, err)
	}
	info, err := os.Stat(codexUpdateStatePath(f.l))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("runtime updater state permissions: %v", err)
	}
	f.now = state.CheckedAt.Add(24 * time.Hour)
	_ = f.m.UpdateCodexRuntime(t.Context(), f.l, false)
	if f.checks != 2 || f.prepares != 4 {
		t.Fatalf("next daily check did not run: checks=%d prepares=%d", f.checks, f.prepares)
	}
}

func TestCodexCheckOnlyDoesNotWriteStateOrPauseWorker(t *testing.T) {
	f := newCodexScheduleFixture(t)
	if err := f.m.UpdateCodexRuntime(t.Context(), f.l, true); err != nil {
		t.Fatal(err)
	}
	if f.checks != 1 || f.prepares != 0 || !strings.Contains(f.output.String(), "0.155.0 -> 0.156.0") {
		t.Fatalf("check-only behavior: checks=%d prepares=%d output=%q", f.checks, f.prepares, f.output.String())
	}
	if _, err := os.Stat(codexUpdateStatePath(f.l)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check-only wrote daily state")
	}
}

func TestCodexFailedDailyCheckIsNotRepeatedEveryFiveMinutes(t *testing.T) {
	f := newCodexScheduleFixture(t)
	f.failCheck = true
	if err := f.m.UpdateCodexRuntime(t.Context(), f.l, false); err == nil {
		t.Fatal("metadata failure was ignored")
	}
	if f.checks != 2 { // Primary channel and its single GitHub fallback.
		t.Fatalf("unexpected metadata attempts: %d", f.checks)
	}
	f.now = f.now.Add(5 * time.Minute)
	if err := f.m.UpdateCodexRuntime(t.Context(), f.l, false); err != nil {
		t.Fatal(err)
	}
	if f.checks != 2 || f.prepares != 0 || strings.Contains(f.output.String(), "runtime is current") {
		t.Fatalf("failed check repeated or claimed current: checks=%d output=%q", f.checks, f.output.String())
	}
	state, err := loadCodexUpdateState(f.l)
	if err != nil || !state.CheckFailed || state.LatestVersion != "" {
		t.Fatalf("failed check state: %+v %v", state, err)
	}
}

func TestCodexDetectsAlreadyUpdatedLauncherWithoutAnotherRemoteCheck(t *testing.T) {
	f := newCodexScheduleFixture(t)
	f.running = "0.154.0"
	state := codexUpdateState{Schema: 1, CheckedAt: f.now, LatestVersion: "0.155.0"}
	if err := WriteJSON(codexUpdateStatePath(f.l), state); err != nil {
		t.Fatal(err)
	}
	err := f.m.UpdateCodexRuntime(t.Context(), f.l, false)
	var busy *BusyError
	if !errors.As(err, &busy) || f.checks != 0 || f.prepares != 1 || !strings.Contains(f.output.String(), "restart pending: 0.154.0 -> 0.155.0") {
		t.Fatalf("launcher drift was not safely deferred: %v checks=%d prepares=%d", err, f.checks, f.prepares)
	}
}

func TestCodexRuntimePlansDeduplicateSharedInstallations(t *testing.T) {
	f := newCodexScheduleFixture(t)
	plans, worker, err := f.m.codexRuntimePlans(t.Context(), f.l)
	if err != nil || worker != "worker-id" || len(plans) != 1 || len(plans[0].Profiles) != 2 {
		t.Fatalf("shared runtime plans: %+v %q %v", plans, worker, err)
	}
}

func TestCodexDailyStateRejectsInvalidVersionsAndHandlesClockChanges(t *testing.T) {
	f := newCodexScheduleFixture(t)
	for _, version := range []string{"0.156.0-beta.1", "invalid"} {
		if err := WriteJSON(codexUpdateStatePath(f.l), codexUpdateState{Schema: 1, LatestVersion: version}); err != nil {
			t.Fatal(err)
		}
		if err := f.m.UpdateCodexRuntime(t.Context(), f.l, false); err == nil {
			t.Fatal("invalid cached version accepted")
		}
	}
	if f.checks != 0 || f.prepares != 0 {
		t.Fatal("invalid local state triggered networking or worker preparation")
	}
	if !codexCheckDue(codexUpdateState{}, f.now) || codexCheckDue(codexUpdateState{CheckedAt: f.now.Add(-11 * time.Hour)}, f.now) || !codexCheckDue(codexUpdateState{CheckedAt: f.now.Add(-23 * time.Hour)}, f.now) || !codexCheckDue(codexUpdateState{CheckedAt: f.now.Add(time.Hour)}, f.now) {
		t.Fatal("daily cadence or clock-reset handling is wrong")
	}
}

func TestCodexRuntimeCLIOptionsAndGatewayIsolation(t *testing.T) {
	opts, err := parseOptions([]string{"update", "codex", "--check"})
	if err != nil || opts.Component != "codex" || !opts.Check {
		t.Fatalf("runtime check options: %+v %v", opts, err)
	}
	for _, args := range [][]string{{"install", "codex", "--config", "worker.json"}, {"update", "codex", "--repo", "example/repo"}, {"update", "codex", "--version", "0.155.0"}, {"auto-update", "enable", "codex"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted unsupported runtime options: %v", args)
		}
	}
	f := newCodexScheduleFixture(t)
	f.l.Component = "gateway"
	f.l.Config = filepath.Join(t.TempDir(), "must-not-be-read.json")
	if err := f.m.UpdateCodexRuntime(t.Context(), f.l, false); err == nil || f.checks != 0 || f.prepares != 0 {
		t.Fatalf("gateway was accepted for Codex updates: %v", err)
	}
}

func TestCodexPendingRestartRecoversEvenWhenInstallationInspectionFails(t *testing.T) {
	for _, running := range []bool{false, true} {
		f := newCodexApplyFixture(t)
		f.manager.Out = io.Discard
		f.running = running
		// This fixture's missing codex_binary prevents planning, as can happen
		// if an interrupted installation or configuration edit broke a launcher.
		state := codexUpdateState{Schema: 1, CheckedAt: f.now, LatestVersion: "0.156.0", RestartPending: true}
		if err := WriteJSON(codexUpdateStatePath(f.layout), state); err != nil {
			t.Fatal(err)
		}
		if err := f.manager.UpdateCodexRuntime(t.Context(), f.layout, false); err == nil {
			t.Fatal("unverified installation was reported as repaired")
		}
		starts := 0
		for _, event := range f.events {
			if event == "start" {
				starts++
			}
			if event == "stop" || event == "prepare" || event == "install" {
				t.Fatalf("recovery interrupted work or installed an unverified runtime: %v", f.events)
			}
		}
		if (!running && starts != 1) || (running && starts != 0) || !f.running {
			t.Fatalf("pending recovery did not preserve/restore service: initial=%v starts=%d running=%v", running, starts, f.running)
		}
		saved, err := loadCodexUpdateState(f.layout)
		if err != nil || !saved.RestartPending {
			t.Fatalf("unverified restart intent was lost: %+v %v", saved, err)
		}
	}
}
