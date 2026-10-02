package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func savedWorkerSetupFixture(t *testing.T) (*Manager, *Layout, string, *workerSetupRecovery) {
	t.Helper()
	m, l, cwd := setupFixture(t)
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
		writeSetupEnrollmentResponse(w, r, workerServiceRestricted)
	})
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"", value}}
	stop := errors.New("stop after preparing installation")
	var prepared string
	err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		prepared = opts.Config
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	recovery, err := loadWorkerSetupRecovery(l)
	if err != nil || recovery == nil {
		t.Fatal("missing saved recovery", err)
	}
	return m, l, prepared, recovery
}

func installMatchingRecoveryConfig(t *testing.T, l *Layout, recovery *workerSetupRecovery) {
	t.Helper()
	cfg := recovery.Config
	cfg.TokenFile = filepath.Join(filepath.Dir(l.Config), "worker.token")
	if err := writePrivateSecret(cfg.TokenFile, []byte(recovery.Token+"\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSetupRecoveryVerifiesActiveWorkerWithoutRestartOrBinaryReplacement(t *testing.T) {
	m, l, source, recovery := savedWorkerSetupFixture(t)
	installMatchingRecoveryConfig(t, l, recovery)
	platformWrite(t, l.Unit, "existing service access and overrides\n", 0644)
	platformWrite(t, l.Binary, "existing active worker executable\n", 0755)
	m.Self = filepath.Join(t.TempDir(), "manager")
	platformWrite(t, m.Self, "new manager executable\n", 0755)
	var commands [][]string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		commands = append(commands, args)
		if reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState"}) {
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		}
		if reflect.DeepEqual(args, []string{l.Binary, "version"}) {
			return CommandResult{Output: []byte("0.5.1\n")}, nil
		}
		if reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}) {
			data, _ := json.Marshal(map[string]any{"updated_at": m.Now(), "gateway_connected": true, "version": "0.5.1", "runtimes": []map[string]string{{"state": "running"}}})
			return CommandResult{Output: data}, nil
		}
		t.Fatalf("active recovery attempted service mutation: %v", args)
		return CommandResult{}, nil
	}
	err := m.resumeEnrolledWorkerInstall(t.Context(), l, source, nil, &Release{Repo: DefaultRepo, Tag: "v0.5.1"})
	if err != nil || len(commands) != 3 || updateRead(t, l.Binary) != "existing active worker executable\n" || updateRead(t, l.Unit) != "existing service access and overrides\n" {
		t.Fatal("active recovery restarted or changed the worker", err)
	}
	settings, err := SavedSettings(l)
	if err != nil || settings["version"] != "v0.5.1" || settings["pending"] == true {
		t.Fatal("verified recovery did not complete installer state", err)
	}
}

func TestWorkerSetupRecoveryRefusesDifferentConfigTokenOrJournal(t *testing.T) {
	for _, kind := range []string{"worker-id", "token", "workspace", "service-profile", "public-journal", "linked-journal", "tampered-source-token"} {
		t.Run(kind, func(t *testing.T) {
			m, l, source, recovery := savedWorkerSetupFixture(t)
			installMatchingRecoveryConfig(t, l, recovery)
			beforeConfig, _ := os.ReadFile(l.Config)
			switch kind {
			case "worker-id":
				cfg, _ := ReadJSON(l.Config)
				cfg["worker_id"] = "00000000-0000-4000-8000-000000000002"
				if err := WriteJSON(l.Config, cfg); err != nil {
					t.Fatal(err)
				}
			case "token":
				if err := os.WriteFile(filepath.Join(filepath.Dir(l.Config), "worker.token"), []byte("different private token\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "workspace":
				recovery.Config.AllowedWorkspaceRoots = []string{t.TempDir()}
				if err := WriteJSON(workerSetupRecoveryPath(l), recovery); err != nil {
					t.Fatal(err)
				}
			case "service-profile":
				recovery.ServiceAccess = "unrecognized"
				if err := WriteJSON(workerSetupRecoveryPath(l), recovery); err != nil {
					t.Fatal(err)
				}
			case "public-journal":
				if err := os.Chmod(workerSetupRecoveryPath(l), 0644); err != nil {
					t.Fatal(err)
				}
			case "linked-journal":
				if err := os.Rename(workerSetupRecoveryPath(l), workerSetupRecoveryPath(l)+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(workerSetupRecoveryPath(l)+".real", workerSetupRecoveryPath(l)); err != nil {
					t.Fatal(err)
				}
			case "tampered-source-token":
				if err := os.WriteFile(recovery.Config.TokenFile, []byte("tampered saved token\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			beforeConfig, _ = os.ReadFile(l.Config)
			m.Run = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("unverified recovery ran a command")
				return CommandResult{}, nil
			}
			err := m.resumeEnrolledWorkerInstall(t.Context(), l, source, nil, &Release{Repo: DefaultRepo, Tag: "v0.5.1"})
			afterConfig, _ := os.ReadFile(l.Config)
			if err == nil || !bytes.Equal(beforeConfig, afterConfig) || strings.Contains(err.Error(), testWorkerEnrollmentToken) {
				t.Fatal("unsafe recovery was accepted or mutated config", err)
			}
		})
	}
}

func TestWorkerSetupRecoveryWithoutConfigRefusesManualWorkerStatusAndOrphanService(t *testing.T) {
	for _, kind := range []string{"manual-status", "orphan-service"} {
		t.Run(kind, func(t *testing.T) {
			m, l, source, recovery := savedWorkerSetupFixture(t)
			if kind == "manual-status" {
				platformWrite(t, recovery.Config.StateFile+".status.json", "existing manual worker status", 0600)
			} else {
				platformWrite(t, l.Unit, "orphan service", 0644)
			}
			m.Run = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("unverified manual worker recovery ran a command")
				return CommandResult{}, nil
			}
			err := m.resumeEnrolledWorkerInstall(t.Context(), l, source, nil, &Release{Repo: DefaultRepo, Tag: "v0.5.1"})
			if err == nil || FileExists(l.Config) || FileExists(l.Binary) || !FileExists(workerSetupRecoveryPath(l)) {
				t.Fatal("manual worker recovery mutated the installation", err)
			}
		})
	}
}

func TestWorkerSetupRecoveryMacOSDistinguishesMissingServiceFromInspectionFailure(t *testing.T) {
	for _, tc := range []struct {
		code                 int
		wantRunning, wantErr bool
	}{{0, true, false}, {113, false, false}, {1, false, true}} {
		m := New(nil)
		m.Run = func(context.Context, ...string) (CommandResult, error) { return CommandResult{ExitCode: tc.code}, nil }
		running, err := m.workerSetupRecoveryServiceRunning(t.Context(), platformWorker(t, "darwin"))
		if running != tc.wantRunning || (err != nil) != tc.wantErr {
			t.Fatal("incorrect launchd service classification", tc, err)
		}
	}
}

func TestWorkerSetupRecoveryMacOSRunCommandRecognizesServiceNotFound(t *testing.T) {
	bin := t.TempDir()
	platformWrite(t, filepath.Join(bin, "launchctl"), "#!/bin/sh\nexit 113\n", 0755)
	t.Setenv("PATH", bin)
	m := New(nil)
	running, err := m.workerSetupRecoveryServiceRunning(t.Context(), platformWorker(t, "darwin"))
	if err != nil || running {
		t.Fatal("production command runner did not recognize missing launchd service", err)
	}
}

func TestWorkerSetupRecoveryCompletesStoppedPartialInstallation(t *testing.T) {
	m, l, source, recovery := savedWorkerSetupFixture(t)
	installMatchingRecoveryConfig(t, l, recovery)
	platformWrite(t, recovery.Config.StateFile, "existing worker database", 0600)
	_, packages, _ := updateFixture(t, "worker")
	platformWrite(t, filepath.Join(packages["worker"], "deploy/systemd/codex-worker.service"), "[Service]\nEnvironment=PATH=/usr/bin\nExecStart=/worker\n", 0644)
	l.WorkerServiceAccess = recovery.ServiceAccess
	started := false
	var serviceActions []string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState"}) {
			return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
		}
		if len(args) >= 3 && args[0] == "systemctl" && args[1] == "--user" {
			serviceActions = append(serviceActions, args[2])
			if args[2] == "start" {
				started = true
			}
			return CommandResult{}, nil
		}
		if reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}) && started {
			data, _ := json.Marshal(map[string]any{"updated_at": m.Now(), "gateway_connected": true, "version": "0.5.1", "runtimes": []map[string]string{{"state": "running"}}})
			return CommandResult{Output: data}, nil
		}
		t.Fatalf("unexpected partial recovery command: %v", args)
		return CommandResult{}, nil
	}
	err := m.resumeEnrolledWorkerInstall(t.Context(), l, source, packages, &Release{Repo: DefaultRepo, Tag: "v0.5.1"})
	if err != nil || !reflect.DeepEqual(serviceActions, []string{"daemon-reload", "enable", "start"}) || !FileExists(l.Binary) || !FileExists(l.Unit) || updateRead(t, recovery.Config.StateFile) != "existing worker database" {
		t.Fatal("stopped partial installation did not complete safely", err, serviceActions)
	}
	installed, err := ReadJSON(l.Config)
	if err != nil || installed["worker_id"] != recovery.Config.WorkerID || updateRead(t, filepath.Join(filepath.Dir(l.Config), "worker.token")) != recovery.Token+"\n" {
		t.Fatal("partial recovery changed worker identity", err)
	}
}

func TestWorkerSetupRecoveryRefusesDowngradingStoppedExistingWorker(t *testing.T) {
	m, l, source, recovery := savedWorkerSetupFixture(t)
	installMatchingRecoveryConfig(t, l, recovery)
	platformWrite(t, l.Unit, "existing worker service", 0644)
	platformWrite(t, l.Binary, "newer existing binary", 0755)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState"}) {
			return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
		}
		if reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}) {
			return CommandResult{ExitCode: 1}, nil
		}
		if reflect.DeepEqual(args, []string{l.Binary, "version"}) {
			return CommandResult{Output: []byte("0.6.0\n")}, nil
		}
		t.Fatalf("downgrade recovery attempted mutation: %v", args)
		return CommandResult{}, nil
	}
	err := m.resumeEnrolledWorkerInstall(t.Context(), l, source, nil, &Release{Repo: DefaultRepo, Tag: "v0.5.1"})
	if err == nil || !strings.Contains(err.Error(), "without downgrading") || updateRead(t, l.Binary) != "newer existing binary" || updateRead(t, l.Unit) != "existing worker service" {
		t.Fatal("recovery downgraded existing worker", err)
	}
}
