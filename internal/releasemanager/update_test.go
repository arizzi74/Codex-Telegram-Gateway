package releasemanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func updateFixture(t *testing.T, component string) (*Layout, map[string]string, *Release) {
	t.Helper()
	root := t.TempDir()
	l := &Layout{
		Component: component, System: "linux", Home: root,
		Bin: filepath.Join(root, "bin"), Manager: filepath.Join(root, "lib", "codex-telegramgw"),
		LegacyManager: filepath.Join(root, "lib", "release-manager.py"),
		Command:       filepath.Join(root, "bin", "codex-telegramgw"),
		Config:        filepath.Join(root, "etc", "config.json"), State: filepath.Join(root, "etc", "update.json"),
		Backups: filepath.Join(root, "backups"), DataRoot: filepath.Join(root, "data"),
	}
	l.Binary = filepath.Join(l.Bin, "codex-"+component)
	for _, dir := range []string{l.Bin, filepath.Dir(l.Manager), filepath.Dir(l.Config), l.DataRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{l.Binary: "old binary", l.Manager: "old manager", l.LegacyManager: "old python manager"} {
		if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(l.LegacyManager, l.Command); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"database_path": filepath.Join(l.DataRoot, "gateway.db"), "worker_id": "worker-id", "state_file": filepath.Join(root, "worker.db")}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(l, DefaultRepo, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if component == "gateway" {
		db := openUpdateDB(t, filepath.Join(l.DataRoot, "gateway.db"))
		if _, err := db.Exec("CREATE TABLE data (value TEXT); INSERT INTO data VALUES ('original')"); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	packages := map[string]string{component: filepath.Join(root, "package"), "local": filepath.Join(root, "local")}
	for _, dir := range packages {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{
		filepath.Join(packages[component], "codex-"+component): "new binary",
		filepath.Join(packages[component], "codex-telegramgw"): "new manager",
		filepath.Join(packages["local"], "codex-local"):        "new local",
	} {
		if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return l, packages, &Release{Repo: DefaultRepo, Tag: "v1.2.3"}
}

func openUpdateDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func updateRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func updateValue(t *testing.T, l *Layout) string {
	t.Helper()
	db := openUpdateDB(t, filepath.Join(l.DataRoot, "gateway.db"))
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT value FROM data").Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func updateSetValue(t *testing.T, l *Layout, value string) {
	t.Helper()
	db := openUpdateDB(t, filepath.Join(l.DataRoot, "gateway.db"))
	defer db.Close()
	if _, err := db.Exec("UPDATE data SET value=?", value); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteBackupIncludesLiveWALAndPreservesRowIDs(t *testing.T) {
	dir := t.TempDir()
	path, backup := filepath.Join(dir, "source.db"), filepath.Join(dir, "backup.db")
	db := openUpdateDB(t, path)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE data(value TEXT); INSERT INTO data(rowid,value) VALUES (123,'committed in WAL')"); err != nil {
		t.Fatal(err)
	}
	walBefore := updateRead(t, path+"-wal")
	if len(walBefore) == 0 {
		t.Fatal("test requires a live nonempty WAL")
	}
	if err := sqliteBackup(t.Context(), path, backup); err != nil {
		t.Fatal(err)
	}
	copy := openUpdateDB(t, backup)
	var rowid int
	var value, integrity string
	if err := copy.QueryRow("SELECT rowid,value FROM data").Scan(&rowid, &value); err != nil {
		t.Fatal(err)
	}
	if rowid != 123 || value != "committed in WAL" {
		t.Fatalf("backup lost data: %d %q", rowid, value)
	}
	if err := copy.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity: %q %v", integrity, err)
	}
	if got := updateRead(t, path+"-wal"); got != walBefore {
		t.Fatal("backup modified source WAL")
	}
	info, err := os.Stat(backup)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions: %v %v", info, err)
	}
}

func TestUpdateFailedMigrationRestoresBeforeRestart(t *testing.T) {
	l, packages, release := updateFixture(t, "gateway")
	databasePath := filepath.Join(l.DataRoot, "gateway.db")
	if err := os.Chmod(databasePath, 0o640); err != nil {
		t.Fatal(err)
	}
	configBefore, stateBefore := updateRead(t, l.Config), updateRead(t, l.State)
	var actions []string
	m := &Manager{Run: func(ctx context.Context, args ...string) (CommandResult, error) {
		if args[0] == "systemctl" {
			actions = append(actions, args[1])
			if args[1] == "start" && (updateRead(t, l.Binary) != "old binary" || updateValue(t, l) != "original") {
				t.Fatal("old service started before rollback finished")
			}
			return CommandResult{}, nil
		}
		if args[0] == "runuser" {
			updateSetValue(t, l, "migrated")
			if err := os.Chmod(databasePath, 0o600); err != nil {
				t.Fatal(err)
			}
			return CommandResult{ExitCode: 1}, nil
		}
		t.Fatalf("unexpected command %q", args)
		return CommandResult{}, nil
	}}
	err := m.ApplyUpdate(t.Context(), l, packages, release, false)
	var apply *ApplyError
	if !errors.As(err, &apply) || apply.ServiceStarted {
		t.Fatalf("expected pre-start error, got %v", err)
	}
	if !reflect.DeepEqual(actions, []string{"stop", "start"}) {
		t.Fatalf("actions: %v", actions)
	}
	if updateRead(t, l.Binary) != "old binary" || updateRead(t, l.Manager) != "old manager" || updateValue(t, l) != "original" {
		t.Fatal("rollback lost original state")
	}
	if updateRead(t, l.Config) != configBefore || updateRead(t, l.State) != stateBefore {
		t.Fatal("configuration/settings changed")
	}
	if info, err := os.Stat(databasePath); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("database permissions were not restored: %v %v", info, err)
	}
	link, err := os.Readlink(l.Command)
	if err != nil || link != l.LegacyManager {
		t.Fatalf("legacy command not restored: %q %v", link, err)
	}
}

func TestUpdateFailureAfterStartPreservesAcceptedData(t *testing.T) {
	for _, failure := range []string{"start", "readiness"} {
		t.Run(failure, func(t *testing.T) {
			l, packages, release := updateFixture(t, "gateway")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
			defer server.Close()
			cfg, err := ReadJSON(l.Config)
			if err != nil {
				t.Fatal(err)
			}
			cfg["listen"] = strings.TrimPrefix(server.URL, "http://")
			if err := WriteJSON(l.Config, cfg); err != nil {
				t.Fatal(err)
			}
			var actions []string
			m := &Manager{ReadyTimeout: 15 * time.Millisecond, PollInterval: time.Millisecond, Run: func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "runuser" {
					updateSetValue(t, l, "migrated")
					return CommandResult{}, nil
				}
				if args[0] == "systemctl" {
					actions = append(actions, args[1])
					if args[1] == "start" {
						updateSetValue(t, l, "accepted user data")
						if failure == "start" {
							return CommandResult{ExitCode: 1}, nil
						}
					}
					return CommandResult{}, nil
				}
				t.Fatalf("unexpected command %q", args)
				return CommandResult{}, nil
			}}
			err = m.ApplyUpdate(t.Context(), l, packages, release, false)
			var apply *ApplyError
			if !errors.As(err, &apply) || !apply.ServiceStarted {
				t.Fatalf("expected post-start error, got %v", err)
			}
			if updateRead(t, l.Binary) != "new binary" || updateRead(t, l.Manager) != "new manager" || updateValue(t, l) != "accepted user data" {
				t.Fatal("unsafe rollback overwrote new state")
			}
			settings, err := ReadJSON(l.State)
			if err != nil || settings["pending"] != true || settings["version"] != release.Tag {
				t.Fatalf("pending settings: %v %v", settings, err)
			}
			if !reflect.DeepEqual(actions, []string{"stop", "start"}) {
				t.Fatalf("unexpected restart: %v", actions)
			}
		})
	}
}

func TestUpdateInvalidBackupNeverReplacesDatabaseOrBinaries(t *testing.T) {
	l, packages, release := updateFixture(t, "gateway")
	path := filepath.Join(l.DataRoot, "gateway.db")
	if err := os.WriteFile(path, []byte("damaged database fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	var actions []string
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] != "systemctl" {
			t.Fatalf("unexpected command: %v", args)
		}
		actions = append(actions, args[1])
		return CommandResult{}, nil
	}}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err == nil {
		t.Fatal("expected invalid backup failure")
	}
	if got := updateRead(t, path); got != "damaged database fixture" {
		t.Fatal("source database replaced after backup failure")
	}
	if updateRead(t, l.Binary) != "old binary" {
		t.Fatal("binary replaced before backup completed")
	}
	if !reflect.DeepEqual(actions, []string{"stop", "start"}) {
		t.Fatalf("old service not restarted: %v", actions)
	}
}

func TestUpdateRollbackUsesUncancelledContext(t *testing.T) {
	l, packages, release := updateFixture(t, "gateway")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	restarted := false
	m := &Manager{Run: func(ctx context.Context, args ...string) (CommandResult, error) {
		if args[0] == "runuser" {
			cancel()
			return CommandResult{}, context.Canceled
		}
		if args[0] == "systemctl" && args[1] == "start" {
			restarted = true
			return CommandResult{}, ctx.Err()
		}
		return CommandResult{}, nil
	}}
	if err := m.ApplyUpdate(ctx, l, packages, release, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if !restarted || updateRead(t, l.Binary) != "old binary" {
		t.Fatal("cancelled update was not recovered")
	}
}

func TestWorkerBusyDoesNotStopOrReplace(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "systemctl" && args[2] == "show" {
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		}
		if args[0] == l.Binary && args[3] == "update" && args[4] == "prepare" {
			return CommandResult{ExitCode: 75, Output: []byte(`{"error":"worker update: native CLI approvals or input are pending"}`)}, nil
		}
		t.Fatalf("busy worker was acted on: %v", args)
		return CommandResult{}, nil
	}}
	err := m.ApplyUpdate(t.Context(), l, packages, release, false)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("wanted busy error: %v", err)
	}
	if busy.Reason != "worker update deferred: native CLI approvals or input are pending" {
		t.Fatalf("lost worker deferral reason: %v", err)
	}
	if updateRead(t, l.Binary) != "old binary" || updateRead(t, l.Manager) != "old manager" {
		t.Fatal("busy worker binary replaced")
	}
}

func TestWorkerPrepareReportsKnownDeferralReasons(t *testing.T) {
	for _, test := range []struct {
		name, reason, want string
	}{
		{"native-request", "worker update: native CLI requests are still in flight", "worker update deferred: native CLI requests are still in flight"},
		{"native-approval", "worker update: native CLI approvals or input are pending", "worker update deferred: native CLI approvals or input are pending"},
		{"native-unverified", "worker update: native CLI activity could not be verified", "worker update deferred: native CLI activity could not be verified"},
		{"native-initializing", "worker update: native CLI connection is still initializing", "worker update deferred: native CLI connection is still initializing"},
		{"active-thread", "worker update: a native CLI thread is active or its idle state cannot be verified", "worker update deferred: a native CLI thread is active or its idle state cannot be verified"},
		{"pending-runtime-request", "worker update: runtime has pending or unconfirmed RPCs; finish work and stop the worker service before updating", "worker update deferred: runtime has pending or unconfirmed requests"},
		{"runtime-busy", "worker update: runtime startup or discovery is in progress", "worker update deferred: runtime startup or discovery is in progress"},
		{"session-queued", "worker update: session has active or queued work", "worker update deferred: commands are queued or executing"},
		{"queued-command", "worker update: commands are queued or executing", "worker update deferred: commands are queued or executing"},
		{"session-response", "worker update: a session has an active turn or pending response", "worker update deferred: a session has an active turn or pending response"},
		{"unacknowledged-events", "worker update: durable events are awaiting gateway acknowledgement", "worker update deferred: events are awaiting gateway acknowledgement"},
		{"legacy-native-guard", "worker update: this runtime was used by a native CLI; finish native work and stop the worker service before updating", "worker update deferred: the installed worker cannot verify a runtime previously used by a native CLI"},
		{"unavailable-coordination", "worker update coordination unavailable; the running worker must support update prepare", "worker update deferred: the running worker does not provide update coordination"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := json.Marshal(map[string]string{"error": test.reason})
			if err != nil {
				t.Fatal(err)
			}
			l := &Layout{Binary: "codex-worker", Config: "config.json"}
			calls := 0
			m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
				calls++
				if !reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "update", "prepare"}) {
					t.Fatalf("failed prepare triggered another command: %v", args)
				}
				return CommandResult{ExitCode: 1, Output: output}, nil
			}}
			lease, err := m.workerPrepare(t.Context(), l)
			var busy *BusyError
			if lease != nil || !errors.As(err, &busy) || busy.Reason != test.want || calls != 1 {
				t.Fatalf("lease=%v error=%v calls=%d, want %q", lease, err, calls, test.want)
			}
		})
	}
}

func TestWorkerPrepareHidesUntrustedOutput(t *testing.T) {
	const known = `{"error":"worker update: native CLI approvals or input are pending"}`
	const secret = "private-token-value"
	for _, test := range []struct{ name, output string }{
		{"empty-legacy-output", ""},
		{"plain-text", "worker update: native CLI approvals or input are pending"},
		{"arbitrary-stderr", "configuration /private/config.json contains " + secret},
		{"private-error", `{"error":"worker update: cannot verify native thread: /private/session.json?token=` + secret + `"}`},
		{"decorated-reason", `{"error":"worker update: native CLI approvals or input are pending: ` + secret + `"}`},
		{"reason-with-newline", `{"error":"worker update: native CLI approvals or input are pending\n` + secret + `"}`},
		{"reason-with-control-byte", `{"error":"worker update: native CLI approvals or input are pending\u0000` + secret + `"}`},
		{"extra-fields", `{"error":"worker update: native CLI approvals or input are pending","token":"` + secret + `"}`},
		{"log-object", `{"msg":"worker stopped","error":"worker update: native CLI approvals or input are pending"}`},
		{"leading-output", secret + "\n" + known},
		{"trailing-output", known + "\n" + secret},
		{"multiple-objects", known + "\n" + known},
		{"array", "[" + known + "]"},
		{"nested-object", `{"error":` + known + `}`},
		{"null", "null"},
		{"wrong-field", `{"message":"worker update: native CLI approvals or input are pending"}`},
		{"wrong-error-type", `{"error":true}`},
		{"malformed", `{"error":"` + secret},
		{"oversize", strings.Repeat(" ", 1025) + known},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := &Manager{Run: func(context.Context, ...string) (CommandResult, error) {
				return CommandResult{ExitCode: 1, Output: []byte(test.output)}, nil
			}}
			lease, err := m.workerPrepare(t.Context(), &Layout{})
			var busy *BusyError
			if lease != nil || !errors.As(err, &busy) || busy.Reason != workerPrepareUnknownReason {
				t.Fatalf("untrusted worker output escaped the generic deferral: lease=%v error=%v", lease, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/private/") {
				t.Fatal("worker output exposed a token or private path")
			}
		})
	}
}

func TestWorkerStoppedLegacyUpdateSucceedsWithoutPreparation(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	started := false
	var actions []string
	m := &Manager{ReadyTimeout: time.Second, Run: func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] == "systemctl" {
			if args[2] == "show" {
				return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
			}
			actions = append(actions, args[2])
			if args[2] == "start" {
				started = true
			}
			return CommandResult{}, nil
		}
		if args[0] == l.Binary && args[3] == "status" {
			if !started {
				return CommandResult{ExitCode: 1}, nil
			}
			data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": true, "version": "1.2.3", "runtimes": []any{}})
			return CommandResult{Output: data}, nil
		}
		t.Fatalf("unexpected command: %v", args)
		return CommandResult{}, nil
	}}
	if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actions, []string{"start"}) {
		t.Fatalf("stopped worker actions: %v", actions)
	}
	if updateRead(t, l.Binary) != "new binary" || updateRead(t, filepath.Join(l.Bin, "codex-local")) != "new local" {
		t.Fatal("worker/helper not installed")
	}
	link, err := os.Readlink(l.Command)
	if err != nil || link != l.Manager {
		t.Fatalf("native manager command: %q %v", link, err)
	}
	settings, err := ReadJSON(l.State)
	if err != nil || settings["pending"] != nil || settings["version"] != release.Tag {
		t.Fatalf("settings not committed: %v %v", settings, err)
	}
}

func TestWorkerLeaseValidatesIdentityAndAbortsInvalidReservations(t *testing.T) {
	for _, fault := range []string{"", "worker", "pid", "expiry", "malformed-expiry"} {
		t.Run(fault, func(t *testing.T) {
			l, _, _ := updateFixture(t, "worker")
			lease := map[string]any{"worker_id": "worker-id", "pid": 123, "token": "private-lease", "expires_at": time.Now().Add(2 * time.Minute)}
			switch fault {
			case "worker":
				lease["worker_id"] = "other-worker"
			case "pid":
				lease["pid"] = 456
			case "expiry":
				lease["expires_at"] = time.Now().Add(20 * time.Second)
			case "malformed-expiry":
				lease["expires_at"] = "not a timestamp"
			}
			data, _ := json.Marshal(lease)
			aborted := false
			m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "systemctl" {
					return CommandResult{Output: []byte("123\n")}, nil
				}
				if args[4] == "abort" {
					aborted = true
					if args[6] != "private-lease" {
						t.Fatal("wrong abort token")
					}
					return CommandResult{}, nil
				}
				return CommandResult{Output: data}, nil
			}}
			_, err := m.workerPrepare(t.Context(), l)
			if (err != nil) != (fault != "") || aborted != (fault != "") {
				t.Fatalf("fault %q: err=%v aborted=%v", fault, err, aborted)
			}
		})
	}
}

func TestWorkerRunningRejectsUnmanagedLiveProcess(t *testing.T) {
	for _, process := range []string{"live", "dead", "unreadable", "never-started", "transition"} {
		t.Run(process, func(t *testing.T) {
			l, _, _ := updateFixture(t, "worker")
			if process == "unreadable" {
				cfg, _ := ReadJSON(l.Config)
				if err := os.WriteFile(cfg["state_file"].(string)+".status.json", []byte("invalid status"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "systemctl" {
					if process == "transition" {
						return CommandResult{Output: []byte("MainPID=0\nActiveState=activating\n")}, nil
					}
					return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
				}
				if process == "unreadable" || process == "never-started" {
					return CommandResult{ExitCode: 1}, nil
				}
				pid := os.Getpid()
				if process == "dead" {
					pid = 2147483647
				}
				return CommandResult{Output: []byte(fmt.Sprintf(`{"pid":%d}`, pid))}, nil
			}}
			running, err := m.workerRunning(t.Context(), l)
			wantBusy := process == "live" || process == "unreadable" || process == "transition"
			var busy *BusyError
			if running || errors.As(err, &busy) != wantBusy || (!wantBusy && err != nil) {
				t.Fatalf("running=%v error=%v", running, err)
			}
		})
	}
}

func TestMacWorkerLeaseMustMatchManagedProcess(t *testing.T) {
	for _, status := range []string{
		"gui/501/com.iaia.codex-worker = {\n\tstate = running\n\tpid = 123\n}\n",
		"state = running\npid = 456\n",
		"state = waiting\n",
		"pid = invalid\n",
		"pid = 123\npid = 123\n",
	} {
		t.Run(status, func(t *testing.T) {
			l, _, _ := updateFixture(t, "worker")
			l.System = "darwin"
			lease, _ := json.Marshal(workerLease{WorkerID: "worker-id", PID: 123, Token: "lease-token", ExpiresAt: time.Now().Add(2 * time.Minute)})
			aborted := false
			m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "launchctl" {
					return CommandResult{Output: []byte(status)}, nil
				}
				if args[4] == "abort" {
					aborted = true
					return CommandResult{}, nil
				}
				return CommandResult{Output: lease}, nil
			}}
			_, err := m.workerPrepare(t.Context(), l)
			valid := strings.Contains(status, "state = running\n\tpid = 123")
			if (err == nil) != valid || aborted == valid {
				t.Fatalf("valid=%v err=%v aborted=%v", valid, err, aborted)
			}
		})
	}
}

func TestWorkerReadinessRequiresFreshVersionAndAllRuntimes(t *testing.T) {
	for _, fault := range []string{"", "missing-runtime", "stopped-runtime", "wrong-version", "stale", "disconnected"} {
		t.Run(fault, func(t *testing.T) {
			l, _, _ := updateFixture(t, "worker")
			cfg, _ := ReadJSON(l.Config)
			cfg["runtimes"] = []any{map[string]any{"id": "enabled", "autostart": true}, map[string]any{"id": "disabled", "autostart": false}}
			if err := WriteJSON(l.Config, cfg); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			status := map[string]any{"updated_at": now.Add(time.Second), "gateway_connected": true, "version": "1.2.3", "runtimes": []any{map[string]any{"state": "running"}}}
			switch fault {
			case "missing-runtime":
				status["runtimes"] = []any{}
			case "stopped-runtime":
				status["runtimes"] = []any{map[string]any{"state": "stopped"}}
			case "wrong-version":
				status["version"] = "1.2.2"
			case "stale":
				status["updated_at"] = now.Add(-time.Second)
			case "disconnected":
				status["gateway_connected"] = false
			}
			data, _ := json.Marshal(status)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probes := 0
			m := &Manager{ReadyTimeout: 5 * time.Second, PollInterval: time.Millisecond, Run: func(context.Context, ...string) (CommandResult, error) {
				probes++
				if fault != "" {
					// End the negative case after a status response is examined,
					// rather than racing a tiny deadline under concurrent CI load.
					cancel()
				}
				return CommandResult{Output: data}, nil
			}}
			err := m.WorkerReady(ctx, l, now, "v1.2.3")
			if probes == 0 {
				t.Fatal("readiness ended before checking worker status")
			}
			if (err != nil) != (fault != "") {
				t.Fatalf("fault %q: %v", fault, err)
			}
		})
	}
}

func TestGatewayReadinessRequiresManagedExecutable(t *testing.T) {
	for _, correct := range []bool{true, false} {
		t.Run(strconv.FormatBool(correct), func(t *testing.T) {
			l, _, _ := updateFixture(t, "gateway")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tgreadyz" {
					t.Errorf("wrong URL: %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			cfg, _ := ReadJSON(l.Config)
			cfg["listen"] = strings.TrimPrefix(server.URL, "http://")
			if err := WriteJSON(l.Config, cfg); err != nil {
				t.Fatal(err)
			}
			if correct {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				l.Binary = executable
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probes := 0
			m := &Manager{ReadyTimeout: 5 * time.Second, PollInterval: time.Millisecond, Run: func(context.Context, ...string) (CommandResult, error) {
				probes++
				if !correct {
					// The HTTP request succeeded; cancel only once the managed
					// executable check has a process to inspect. Real HTTP and
					// /proc lookups cannot reliably finish within 8ms under -race.
					cancel()
				}
				return CommandResult{Output: []byte(fmt.Sprintf("MainPID=%d\nActiveState=active\n", os.Getpid()))}, nil
			}}
			err := m.GatewayReady(ctx, l)
			if probes == 0 {
				t.Fatal("readiness ended before checking the managed executable")
			}
			if (err == nil) != correct {
				t.Fatalf("correct=%v error=%v", correct, err)
			}
		})
	}
}

func TestSQLiteRollbackCannotEscapeReplacedDataDirectory(t *testing.T) {
	l, _, _ := updateFixture(t, "gateway")
	nested := filepath.Join(l.DataRoot, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "gateway.db")
	if err := os.Rename(filepath.Join(l.DataRoot, "gateway.db"), path); err != nil {
		t.Fatal(err)
	}
	cfg, _ := ReadJSON(l.Config)
	cfg["database_path"] = path
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	database, err := openGatewayDatabase(l)
	if err != nil {
		t.Fatal(err)
	}
	defer database.root.Close()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := sqliteBackup(t.Context(), path, backup); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "gateway.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(outsidePath+suffix, []byte("must survive"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(nested, filepath.Join(l.DataRoot, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, nested); err != nil {
		t.Fatal(err)
	}
	if err := restoreSQLite(database, backup); err == nil {
		t.Fatal("rollback followed a symlink outside data root")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if updateRead(t, outsidePath+suffix) != "must survive" {
			t.Fatal("rollback modified an outside file")
		}
	}
}

func TestGatewayDatabaseRejectsSymlinkDataRoot(t *testing.T) {
	l, _, _ := updateFixture(t, "gateway")
	original := l.DataRoot + "-original"
	if err := os.Rename(l.DataRoot, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, l.DataRoot); err != nil {
		t.Fatal(err)
	}
	if database, err := openGatewayDatabase(l); err == nil {
		database.root.Close()
		t.Fatal("accepted a symlink as the trusted data root")
	}
}

func TestGatewayDatabaseRejectsSymlinkSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			l, _, _ := updateFixture(t, "gateway")
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(l.DataRoot, "gateway.db")+suffix); err != nil {
				t.Fatal(err)
			}
			if database, err := openGatewayDatabase(l); err == nil {
				database.root.Close()
				t.Fatal("accepted a symlink SQLite sidecar")
			}
			if updateRead(t, outside) != "original" {
				t.Fatal("outside file changed")
			}
		})
	}
}
