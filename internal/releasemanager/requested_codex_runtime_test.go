package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
)

func requestedCodexSchedule(t *testing.T) (*codexScheduleFixture, *requestedWorkerPlan) {
	t.Helper()
	f := newCodexScheduleFixture(t)
	f.l.Lock = filepath.Join(f.l.Home, "update.lock")
	run := f.m.Run
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{f.l.Binary, "version"}) {
			return CommandResult{Output: []byte("1.2.3")}, nil
		}
		return run(ctx, args...)
	}
	return f, &requestedWorkerPlan{release: &Release{Tag: "1.2.3"}, codexCache: filepath.Join(t.TempDir(), "request.codex-check.json")}
}

func TestRequestedCodexFreshCheckIgnoresDailyCacheWithoutChangingSchedule(t *testing.T) {
	f, plan := requestedCodexSchedule(t)
	f.latest = "0.155.0"
	daily := codexUpdateState{Schema: 1, CheckedAt: f.now.Add(-time.Hour), LatestVersion: "0.154.0"}
	if err := WriteJSON(codexUpdateStatePath(f.l), daily); err != nil {
		t.Fatal(err)
	}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.State != "up_to_date" || result.Codex == nil || result.Codex.State != "up_to_date" || result.Codex.LatestVersion != f.latest || f.checks != 1 || f.prepares != 0 {
		t.Fatalf("explicit check: result=%+v codex=%+v checks=%d prepares=%d err=%v", result, result.Codex, f.checks, f.prepares, err)
	}
	if len(result.Codex.Profiles) != 2 || result.Codex.Profiles[0].InstalledVersion != "0.155.0" || result.Codex.Profiles[0].RunningVersion != "0.155.0" {
		t.Fatalf("configured/running versions missing: %+v", result.Codex)
	}
	after, err := loadCodexUpdateState(f.l)
	if err != nil || !reflect.DeepEqual(after, daily) {
		t.Fatalf("explicit check changed daily cache: %+v %v", after, err)
	}
}

func TestRequestedCodexBusyRetriesAndSupervisorRestartReuseFreshCheck(t *testing.T) {
	f, plan := requestedCodexSchedule(t)
	for i := 0; i < 3; i++ {
		_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
		var busy *BusyError
		if !errors.As(err, &busy) || f.checks != 1 || f.prepares != i+1 {
			t.Fatalf("retry %d: checks=%d prepares=%d err=%v", i, f.checks, f.prepares, err)
		}
	}
	// Reconstruct the plan as a restarted independently supervised updater would.
	plan = &requestedWorkerPlan{release: plan.release, codexCache: plan.codexCache}
	_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	var busy *BusyError
	if !errors.As(err, &busy) || f.checks != 1 || f.prepares != 4 {
		t.Fatalf("restart repeated discovery: checks=%d prepares=%d err=%v", f.checks, f.prepares, err)
	}
	info, err := os.Stat(plan.codexCache)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("request cache is not private: %v", err)
	}
}

func TestRequestedCodexFailedCheckPreservesWorkerSuccessAndCurrentVersions(t *testing.T) {
	f, plan := requestedCodexSchedule(t)
	f.failCheck = true
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.State != "up_to_date" || result.Version != "1.2.3" || result.Codex.State != "failed" || result.Codex.ErrorCode != "check_failed" || len(result.Codex.Profiles) != 2 || f.prepares != 0 || f.checks != 2 {
		t.Fatalf("worker success lost: result=%+v codex=%+v err=%v", result, result.Codex, err)
	}
	if _, err := f.m.requestedWorkerStep(t.Context(), f.l, plan); err != nil || f.checks != 2 {
		t.Fatalf("failed discovery repeated: checks=%d err=%v", f.checks, err)
	}
}

func TestRequestedCodexExternalProfilesReportVersionsWithoutInstalling(t *testing.T) {
	f, plan := requestedCodexSchedule(t)
	cfg, err := ReadJSON(f.l.Config)
	if err != nil {
		t.Fatal(err)
	}
	profiles := cfg["runtimes"].([]any)
	binary := profiles[0].(map[string]any)["codex_binary"].(string)
	pinned, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		profile.(map[string]any)["codex_binary"] = pinned
	}
	if err := WriteJSON(f.l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	run := f.m.Run
	const externalVersion = "0.155.0-alpha.1"
	f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{pinned, "--version"}) {
			return CommandResult{Output: []byte("codex-cli " + externalVersion)}, nil
		}
		result, err := run(ctx, args...)
		if reflect.DeepEqual(args, []string{f.l.Binary, "--config", f.l.Config, "status"}) && err == nil {
			var status codexObservedStatus
			if err := json.Unmarshal(result.Output, &status); err != nil {
				t.Fatal(err)
			}
			for i := range status.Runtimes {
				status.Runtimes[i].Version = "codex-cli " + externalVersion
			}
			result.Output, _ = json.Marshal(status)
		}
		return result, err
	}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.Codex.State != "unsupported" || result.Codex.LatestVersion != "0.156.0" || f.checks != 1 || f.prepares != 0 {
		t.Fatalf("external runtime behavior: %+v %v", result.Codex, err)
	}
	for _, profile := range result.Codex.Profiles {
		if profile.Support != "external" || profile.InstalledVersion != externalVersion || profile.RunningVersion != externalVersion {
			t.Fatalf("external version missing: %+v", profile)
		}
	}
}

func TestRequestedCodexWaitsForFreshAutostartRuntimeEvidence(t *testing.T) {
	for _, fault := range []string{"stale", "missing-autostart", "read-error"} {
		t.Run(fault, func(t *testing.T) {
			f, plan := requestedCodexSchedule(t)
			f.latest = "0.155.0"
			unready := true
			run := f.m.Run
			f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				result, err := run(ctx, args...)
				if unready && reflect.DeepEqual(args, []string{f.l.Binary, "--config", f.l.Config, "status"}) {
					if fault == "read-error" {
						return CommandResult{ExitCode: 1}, nil
					}
					var status codexObservedStatus
					if err := json.Unmarshal(result.Output, &status); err != nil {
						t.Fatal(err)
					}
					if fault == "stale" {
						status.UpdatedAt = f.now.Add(-time.Minute)
					} else {
						status.Runtimes = status.Runtimes[:1]
					}
					result.Output, _ = json.Marshal(status)
				}
				return result, err
			}
			_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
			var busy *BusyError
			if !errors.As(err, &busy) || f.prepares != 0 || f.checks != 1 {
				t.Fatalf("unready evidence became terminal: %v", err)
			}
			unready = false
			result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
			if err != nil || result.Codex.State != "up_to_date" || f.checks != 1 {
				t.Fatalf("fresh evidence did not settle request: %+v %v", result.Codex, err)
			}
		})
	}
}

func requestedCodexApply(t *testing.T) (*codexApplyFixture, *requestedWorkerPlan, *int) {
	t.Helper()
	f := newCodexApplyFixture(t)
	f.layout.Lock = filepath.Join(f.layout.Home, "update.lock")
	cfg, _ := ReadJSON(f.layout.Config)
	for _, profile := range cfg["runtimes"].([]any) {
		profile.(map[string]any)["codex_binary"] = f.plan.Distribution.Binary
	}
	if err := WriteJSON(f.layout.Config, cfg); err != nil {
		t.Fatal(err)
	}
	f.manager.Out = io.Discard
	checks := new(int)
	f.manager.HTTP = &http.Client{Transport: coreRoundTripper(func(request *http.Request) (*http.Response, error) {
		*checks++
		return coreResponse(request, []byte(`{"tag_name":"rust-v0.156.0"}`)), nil
	})}
	// Worker maintenance already succeeded. Runtime maintenance must still take
	// its own lease, and must retain this success if its separate update fails.
	plan := &requestedWorkerPlan{workerResult: &protocol.WorkerUpdateResult{State: "completed", Version: "0.5.61"}}
	f.events = append(f.events, "worker-completed")
	return f, plan, checks
}

func TestRequestedCodexWaitsForFreshLeaseThenInstallsAndVerifies(t *testing.T) {
	f, plan, checks := requestedCodexApply(t)
	f.busy = true
	_, err := f.manager.requestedWorkerStep(t.Context(), f.layout, plan)
	var busy *BusyError
	if !errors.As(err, &busy) || countCodexEvent(f.events, "stop") != 0 {
		t.Fatalf("runtime ignored active work: %v %v", f.events, err)
	}
	f.busy = false
	result, err := f.manager.requestedWorkerStep(t.Context(), f.layout, plan)
	if err != nil || result.State != "completed" || result.Codex.State != "completed" || *checks != 1 || f.installedVersion() != "0.157.0" || !f.running {
		t.Fatalf("runtime update failed: result=%+v codex=%+v checks=%d events=%v err=%v", result, result.Codex, *checks, f.events, err)
	}
	if countCodexEvent(f.events, "prepare") != 2 || slices.Index(f.events, "prepare") < slices.Index(f.events, "worker-completed") || result.Codex.Profiles[0].RunningVersion != "0.157.0" {
		t.Fatalf("fresh lease or runtime readiness missing: %v %+v", f.events, result.Codex)
	}
}

func TestRequestedCodexInstallFailurePreservesWorkerSuccessAfterRollback(t *testing.T) {
	f, plan, _ := requestedCodexApply(t)
	f.nativeFault = "partial-failure"
	result, err := f.manager.requestedWorkerStep(t.Context(), f.layout, plan)
	if err != nil || result.State != "completed" || result.Version != "0.5.61" || result.Codex.State != "failed" || result.Codex.ErrorCode != "update_failed" || !f.running || f.installedVersion() != "0.154.0" {
		t.Fatalf("runtime failure erased worker result or failed rollback: result=%+v codex=%+v err=%v", result, result.Codex, err)
	}
}

func TestRequestedCodexQuarantinedReleaseRemainsWithheld(t *testing.T) {
	f, plan := requestedCodexSchedule(t)
	state := codexUpdateState{Schema: 1, CheckedAt: f.now, LatestVersion: f.latest, RejectedVersions: []string{f.latest}}
	if err := WriteJSON(codexUpdateStatePath(f.l), state); err != nil {
		t.Fatal(err)
	}
	result, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
	if err != nil || result.Codex.State != "withheld" || result.Codex.ErrorCode != "rejected_release" || f.prepares != 0 || f.checks != 1 {
		t.Fatalf("quarantine bypassed: %+v %v", result.Codex, err)
	}
}

func TestRequestedCodexRecoversBrokenLauncherBeforePendingWorkerReadiness(t *testing.T) {
	f, plan, checks := requestedCodexApply(t)
	plan.workerResult = nil
	plan.release = &Release{Tag: "0.5.43"}
	settings, err := SavedSettings(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	settings["pending"] = true
	if err := WriteJSON(f.layout.State, settings); err != nil {
		t.Fatal(err)
	}
	run := f.manager.Run
	f.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		result, err := run(ctx, args...)
		if reflect.DeepEqual(args, []string{f.layout.Binary, "--config", f.layout.Config, "status"}) && err == nil && result.ExitCode == 0 {
			f.events = append(f.events, "worker-readiness")
			var status map[string]any
			if json.Unmarshal(result.Output, &status) != nil {
				t.Fatal("invalid fixture status")
			}
			status["version"] = "0.5.43"
			result.Output, _ = json.Marshal(status)
		}
		return result, err
	}
	snapshot, err := codexRecoveryFixtureSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	journal := &codexRecovery{Schema: 1, Phase: "rollback", Restart: true, Snapshots: []codexRuntimeSnapshot{snapshot}, RejectedVersions: []string{"0.156.0"}}
	if err := writeCodexRecovery(f.layout, journal); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.plan.Distribution.Home, "packages", "standalone", "current")); err != nil {
		t.Fatal(err)
	}
	f.running = false
	result, err := f.manager.requestedWorkerStep(t.Context(), f.layout, plan)
	if err != nil || result.State != "up_to_date" || result.Codex.State != "withheld" || !f.running || f.installedVersion() != "0.154.0" || *checks != 1 || countCodexEvent(f.events, "native-update") != 0 {
		t.Fatalf("recovery blocked by ordinary inspection: result=%+v codex=%+v events=%v err=%v", result, result.Codex, f.events, err)
	}
	settings, err = SavedSettings(f.layout)
	if err != nil || settings["pending"] == true || slices.Index(f.events, "worker-readiness") < slices.Index(f.events, "start") {
		t.Fatalf("worker readiness did not settle after runtime recovery: settings=%v events=%v err=%v", settings, f.events, err)
	}
	if _, err := os.Stat(codexRecoveryPath(f.layout)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered transaction retained journal: %v", err)
	}
}
