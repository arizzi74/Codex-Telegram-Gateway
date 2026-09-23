package releasemanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCodexFailureRestoresActualReleaseAndVerifiesWorker(t *testing.T) {
	for _, fault := range []string{"partial-failure", "partial-cancel", "removed-current", "preflight", "readiness"} {
		t.Run(fault, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.cancel = cancel
			switch fault {
			case "preflight":
				f.preflightFault = true
			case "readiness":
				f.readinessFault = "version"
			default:
				f.nativeFault = fault
			}
			err := f.manager.applyCodexRuntimeUpdates(ctx, f.layout, []codexRuntimePlan{f.plan}, false, f.record)
			if err == nil || !f.running || f.installedVersion() != "0.154.0" {
				t.Fatalf("rollback failed: version=%s running=%v events=%v err=%v", f.installedVersion(), f.running, f.events, err)
			}
			if _, err := os.Stat(codexRecoveryPath(f.layout)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completed rollback retained journal: %v", err)
			}
			state, err := loadCodexUpdateState(f.layout)
			if err != nil || state.RestartPending || !slices.Contains(state.RejectedVersions, "0.156.0") || state.RejectedByWorkerVersion != "0.5.43" {
				t.Fatalf("rollback/quarantine state: %+v %v", state, err)
			}
			if fault == "readiness" && countCodexEvent(f.events, "prepare") != 2 {
				t.Fatalf("restarted worker was not freshly fenced: %v", f.events)
			}
			if (fault == "partial-failure" || fault == "partial-cancel" || fault == "preflight" || fault == "readiness") && !slices.Contains(state.RejectedVersions, "0.157.0") {
				t.Fatal("actual newer installed release was not quarantined")
			}
		})
	}
}
func TestCodexRollbackDefersWhenRestartedWorkerAcceptedWork(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.readinessFault = "version"
	f.busyAfterStart = true
	err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record)
	if err == nil || !strings.Contains(err.Error(), "rollback is pending") || !f.running || f.installedVersion() != "0.157.0" || countCodexEvent(f.events, "stop") != 1 {
		t.Fatalf("rollback interrupted accepted work: events=%v err=%v", f.events, err)
	}
	journal, err := loadCodexRecovery(f.layout)
	if err != nil || journal == nil || journal.Phase != "rollback" {
		t.Fatalf("recovery intent lost: %+v %v", journal, err)
	}
	f.readinessFault = ""
	f.busyAfterStart = false
	if err = f.manager.UpdateCodexRuntime(t.Context(), f.layout, false); err != nil {
		t.Fatal(err)
	}
	if !f.running || f.installedVersion() != "0.154.0" || countCodexEvent(f.events, "native-update") != 1 || countCodexEvent(f.events, "stop") != 2 {
		t.Fatalf("deferred rollback failed: %v", f.events)
	}
}
func TestCodexInterruptedTransactionRecoversBeforePlanningOrNetworking(t *testing.T) {
	for _, phase := range []string{"prepared", "installing", "starting", "rollback", "committed"} {
		t.Run(phase, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			snapshot, err := codexRecoveryFixtureSnapshot(f)
			if err != nil {
				t.Fatal(err)
			}
			journal := &codexRecovery{Schema: 1, Phase: phase, Restart: true, Snapshots: []codexRuntimeSnapshot{snapshot}, RejectedVersions: []string{"0.157.0"}}
			if phase != "prepared" {
				f.installVersion("0.157.0")
			}
			f.running = phase == "starting" || phase == "committed"
			if err = WriteJSON(codexRecoveryPath(f.layout), journal); err != nil {
				t.Fatal(err)
			}
			if err = WriteJSON(codexUpdateStatePath(f.layout), codexUpdateState{Schema: 1, RestartPending: true}); err != nil {
				t.Fatal(err)
			}
			// The ordinary fixture omits codex_binary, deliberately making planning
			// impossible. A committed journal no longer needs the old release files.
			if phase == "committed" {
				if err = os.RemoveAll(snapshot.ReleaseDir); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.manager.UpdateCodexRuntime(t.Context(), f.layout, false); err != nil {
				t.Fatal(err)
			}
			want := "0.154.0"
			if phase == "committed" {
				want = "0.157.0"
			}
			if !f.running || f.installedVersion() != want || slices.Contains(f.events, "native-update") {
				t.Fatalf("interrupted recovery failed: %v", f.events)
			}
			state, err := loadCodexUpdateState(f.layout)
			if err != nil || state.RestartPending {
				t.Fatalf("restart intent not cleared: %+v %v", state, err)
			}
		})
	}
}
func TestCodexMultiInstallationFailureRollsBackEveryChangedRelease(t *testing.T) {
	first, second := newCodexApplyFixture(t), newCodexApplyFixture(t)
	second.running = false
	second.nativeFault = "partial-failure"
	second.plan.Profiles = map[string]bool{"secondary": false}
	firstRun := first.manager.Run
	first.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if args[0] == second.plan.Distribution.Binary {
			return second.run(ctx, args...)
		}
		return firstRun(ctx, args...)
	}
	first.manager.CodexRun = func(ctx context.Context, args ...string) (CommandResult, error) {
		if args[len(args)-2] == second.plan.Distribution.Binary {
			return second.update(ctx, args...)
		}
		return first.update(ctx, args...)
	}
	err := first.manager.applyCodexRuntimeUpdates(t.Context(), first.layout, []codexRuntimePlan{first.plan, second.plan}, false, first.record)
	if err == nil || !first.running || first.installedVersion() != "0.154.0" || second.installedVersion() != "0.154.0" {
		t.Fatalf("multi installation rollback failed: %v", err)
	}
}
func TestCodexRollbackRefusesTamperedPreviousReleaseAndPreservesJournal(t *testing.T) {
	for _, kind := range []string{"binary", "metadata", "public-binary", "public-release", "escaped-release", "launcher-file"} {
		t.Run(kind, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			snapshot, err := codexRecoveryFixtureSnapshot(f)
			if err != nil {
				t.Fatal(err)
			}
			f.installVersion("0.157.0")
			switch kind {
			case "binary":
				err = os.WriteFile(filepath.Join(snapshot.ReleaseDir, "bin", "codex"), []byte("changed"), 0700)
			case "metadata":
				err = os.WriteFile(filepath.Join(snapshot.ReleaseDir, "codex-package.json"), []byte(`{}`), 0600)
			case "public-binary":
				err = os.Chmod(filepath.Join(snapshot.ReleaseDir, "bin", "codex"), 0775)
			case "public-release":
				err = os.Chmod(snapshot.ReleaseDir, 0775)
			case "escaped-release":
				snapshot.ReleaseDir = filepath.Dir(snapshot.ReleaseDir)
			case "launcher-file":
				err = os.Remove(f.plan.Distribution.Binary)
				if err == nil {
					err = os.WriteFile(f.plan.Distribution.Binary, []byte("foreign file"), 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = restoreCodexRuntimeSnapshot(snapshot); err == nil {
				t.Fatal("untrusted restoration was accepted")
			}
			if f.installedVersion() != "0.157.0" {
				t.Fatal("rejected rollback changed current release")
			}
		})
	}
}
func TestCodexRollbackRepairsMissingAndBrokenOfficialLinks(t *testing.T) {
	for _, kind := range []string{"missing-current", "broken-current", "missing-launcher"} {
		t.Run(kind, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			snapshot, err := codexRecoveryFixtureSnapshot(f)
			if err != nil {
				t.Fatal(err)
			}
			f.installVersion("0.157.0")
			current := filepath.Join(f.plan.Distribution.Home, "packages", "standalone", "current")
			if kind == "missing-launcher" {
				err = os.Remove(f.plan.Distribution.Binary)
			} else {
				err = os.Remove(current)
				if err == nil && kind == "broken-current" {
					err = os.Symlink(snapshot.CandidateReleaseDir, current)
					if err == nil {
						err = os.RemoveAll(snapshot.CandidateReleaseDir)
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = restoreCodexRuntimeSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
			if f.installedVersion() != "0.154.0" {
				t.Fatal("prior release was not restored")
			}
			if _, err = filepath.EvalSymlinks(f.plan.Distribution.Binary); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCodexRejectedReleaseWaitsForNewWorkerOrNewCodexVersion(t *testing.T) {
	for _, change := range []string{"none", "worker", "older-worker", "codex"} {
		t.Run(change, func(t *testing.T) {
			f := newCodexScheduleFixture(t)
			state := codexUpdateState{Schema: 1, CheckedAt: f.now, LatestVersion: "0.156.0", RejectedVersions: []string{"0.156.0"}, RejectedByWorkerVersion: "0.5.43"}
			if change == "codex" {
				state.LatestVersion = "0.157.0"
			}
			if err := WriteJSON(codexUpdateStatePath(f.l), state); err != nil {
				t.Fatal(err)
			}
			oldRun := f.m.Run
			f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if len(args) == 2 && args[0] == f.l.Binary && args[1] == "version" {
					version := "0.5.43"
					if change == "worker" {
						version = "0.5.44"
					}
					if change == "older-worker" {
						version = "0.5.42"
					}
					return CommandResult{Output: []byte(version)}, nil
				}
				return oldRun(ctx, args...)
			}
			err := f.m.UpdateCodexRuntime(t.Context(), f.l, false)
			if change == "none" || change == "older-worker" {
				if err != nil || f.prepares != 0 {
					t.Fatalf("rejected release retried: %v", err)
				}
			} else {
				var busy *BusyError
				if !errors.As(err, &busy) || f.prepares != 1 {
					t.Fatalf("new version not reconsidered: %v", err)
				}
			}
		})
	}
}
func TestCodexRecoveryReadinessFailureRemainsPending(t *testing.T) {
	f := newCodexApplyFixture(t)
	snapshot, err := codexRecoveryFixtureSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	f.running = false
	journal := &codexRecovery{Schema: 1, Phase: "rollback", Restart: true, Snapshots: []codexRuntimeSnapshot{snapshot}, RejectedVersions: []string{"0.156.0"}}
	if err = WriteJSON(codexRecoveryPath(f.layout), journal); err != nil {
		t.Fatal(err)
	}
	original := f.manager.Run
	f.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if len(args) > 3 && args[0] == f.layout.Binary && args[3] == "status" && f.running {
			return CommandResult{ExitCode: 1}, nil
		}
		return original(ctx, args...)
	}
	err = f.manager.recoverCodexRuntime(t.Context(), f.layout, journal)
	if err == nil || !strings.Contains(err.Error(), "readiness failed") {
		t.Fatalf("rollback start was mistaken for recovery: %v", err)
	}
	if saved, err := loadCodexRecovery(f.layout); err != nil || saved == nil {
		t.Fatalf("unverified recovery lost durable intent: %v", err)
	}
}
func TestCodexPreflightEnvironmentDoesNotExposeProductionCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("CODEX_HOME", "production")
	t.Setenv("CODEX_INTERNAL_RUNTIME", "production")
	env := codexPreflightEnvironment("isolated")
	for _, entry := range env {
		if strings.Contains(entry, "secret") || strings.Contains(entry, "production") {
			t.Fatalf("preflight inherited private environment: %s", strings.Split(entry, "=")[0])
		}
	}
	if !slices.Contains(env, "HOME=isolated") || !slices.Contains(env, "CODEX_HOME=isolated") {
		t.Fatal("isolated home missing")
	}
}
func countCodexEvent(events []string, want string) int {
	n := 0
	for _, event := range events {
		if event == want {
			n++
		}
	}
	return n
}

func TestCodexRecoveryJournalDeduplicatesSharedTargets(t *testing.T) {
	f := newCodexApplyFixture(t)
	snapshot, err := codexRecoveryFixtureSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	journal := &codexRecovery{Schema: 1, Phase: "rollback", Snapshots: []codexRuntimeSnapshot{snapshot}}
	for i := 0; i < 64; i++ {
		journal.rejectVersion("0.157.0")
	}
	if err = WriteJSON(codexRecoveryPath(f.layout), journal); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadCodexRecovery(f.layout)
	if err != nil || len(loaded.RejectedVersions) != 1 {
		t.Fatalf("journal cannot read its deduplicated targets: %v", err)
	}
}

func TestCodexRecoveryNeverExpiresPendingJournalIntoUnsafeStop(t *testing.T) {
	f := newCodexApplyFixture(t)
	snapshot, err := codexRecoveryFixtureSnapshot(f)
	if err != nil {
		t.Fatal(err)
	}
	f.installVersion("0.157.0")
	f.busy = true
	journal := &codexRecovery{Schema: 1, Phase: "rollback", Restart: true, Snapshots: []codexRuntimeSnapshot{snapshot}, RejectedVersions: []string{"0.157.0"}}
	if err = WriteJSON(codexRecoveryPath(f.layout), journal); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = f.manager.UpdateCodexRuntime(t.Context(), f.layout, false); err == nil {
			t.Fatal("busy pending rollback succeeded")
		}
		f.now = f.now.Add(24 * time.Hour)
	}
	if slices.Contains(f.events, "stop") || f.installedVersion() != "0.157.0" {
		t.Fatal("pending rollback interrupted active work")
	}
}

func TestCodexRuntimeNativePreflight(t *testing.T) {
	if os.Getenv("CODEX_PREFLIGHT_REAL_TEST") != "1" {
		t.Skip("set CODEX_PREFLIGHT_REAL_TEST=1 and CODEX_REAL_BINARY for an isolated native check")
	}
	binary := os.Getenv("CODEX_REAL_BINARY")
	if binary == "" {
		t.Fatal("CODEX_REAL_BINARY is required")
	}
	manager := New(nil)
	if err := manager.preflightCodexRuntime(t.Context(), &codexDistribution{Binary: binary}); err != nil {
		t.Fatal(err)
	}
}
func TestCodexRecoverySizeRejectedBeforeWorkerPreparation(t *testing.T) {
	for _, kind := range []string{"profiles", "profile-name", "duplicate-installation"} {
		t.Run(kind, func(t *testing.T) {
			f := newCodexApplyFixture(t)
			plans := []codexRuntimePlan{f.plan}
			switch kind {
			case "profiles":
				for i := 0; i < 129; i++ {
					f.plan.Profiles[strings.Repeat("x", i+1)] = false
				}
			case "profile-name":
				f.plan.Profiles[strings.Repeat("x", 257)] = false
			case "duplicate-installation":
				plans = append(plans, f.plan)
			}
			if err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, plans, false, f.record); err == nil || slices.Contains(f.events, "prepare") || slices.Contains(f.events, "stop") {
				t.Fatalf("unsupported recovery shape reached maintenance: %v %v", f.events, err)
			}
		})
	}
}

func codexRecoveryFixtureSnapshot(f *codexApplyFixture) (codexRuntimeSnapshot, error) {
	snapshot, err := snapshotCodexRuntime(f.plan.Distribution, f.plan.Profiles)
	snapshot.CandidateReleaseDir = filepath.Join(filepath.Dir(snapshot.ReleaseDir), "0.157.0-aarch64-unknown-linux-musl")
	snapshot.CandidateObserved = true
	return snapshot, err
}
func TestCodexRollbackDoesNotOverwriteReleaseChangedAfterCheckpoint(t *testing.T) {
	f := newCodexApplyFixture(t)
	f.readinessFault = "version"
	original := f.manager.Run
	changed := false
	f.manager.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		// Simulate an independent installation after the candidate was recorded
		// and started, while readiness is being inspected.
		if len(args) > 3 && args[0] == f.layout.Binary && args[3] == "status" && f.pid == 456 && !changed {
			changed = true
			f.installVersion("0.158.0")
		}
		return original(ctx, args...)
	}
	err := f.manager.applyCodexRuntimeUpdates(t.Context(), f.layout, []codexRuntimePlan{f.plan}, false, f.record)
	if err == nil || !strings.Contains(err.Error(), "changed after its update checkpoint") || !f.running || f.installedVersion() != "0.158.0" || countCodexEvent(f.events, "stop") != 1 {
		t.Fatalf("independent release was clobbered: %s %v %v", f.installedVersion(), f.events, err)
	}
	journal, err := loadCodexRecovery(f.layout)
	if err != nil || journal == nil || journal.Snapshots[0].CandidateReleaseDir == filepath.Join(filepath.Dir(journal.Snapshots[0].ReleaseDir), "0.158.0-aarch64-unknown-linux-musl") {
		t.Fatalf("candidate identity was replaced: %+v %v", journal, err)
	}
}
