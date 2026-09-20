package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWorkerServiceAccessWizard(t *testing.T) {
	for _, tc := range []struct {
		name, system, want string
		answers            []string
	}{
		{name: "default restricted", system: "linux", answers: []string{""}, want: workerServiceRestricted},
		{name: "explicit restricted", system: "linux", answers: []string{"yes"}, want: workerServiceRestricted},
		{name: "full", system: "linux", answers: []string{"no"}, want: workerServiceFull},
		{name: "retry invalid", system: "linux", answers: []string{"invalid", "n"}, want: workerServiceFull},
		{name: "macOS account access", system: "darwin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			m := New(&output)
			m.Run = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("choosing access must not invoke sudo or modify the service")
				return CommandResult{}, nil
			}
			prompt := &scriptedWorkerSetup{t: t, answers: tc.answers}
			got, err := m.setupWorkerServiceAccess(t.Context(), platformWorker(t, tc.system), prompt)
			if err != nil || got != tc.want || len(prompt.answers) != 0 {
				t.Fatalf("access=%q err=%v remaining=%v", got, err, prompt.answers)
			}
			if tc.system == "linux" {
				for _, detail := range []string{"sudo apt update", "existing sudo policy", "Codex session permissions", "allowed working directories"} {
					if !strings.Contains(output.String(), detail) {
						t.Fatalf("missing access explanation %q: %s", detail, output.String())
					}
				}
			} else if !strings.Contains(output.String(), "profiles are unavailable") || !strings.Contains(output.String(), "macOS privacy controls") {
				t.Fatalf("macOS restriction promise is misleading: %s", output.String())
			}
		})
	}
}

func TestWorkerServiceAccessCancellationDoesNotChooseFullAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := New(nil).setupWorkerServiceAccess(ctx, platformWorker(t, "linux"), &scriptedWorkerSetup{t: t})
	if got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled access choice: %q, %v", got, err)
	}
}

func TestWorkerServiceAccessOptionsRequireFreshLinuxWorker(t *testing.T) {
	for _, access := range []string{"", workerServiceRestricted, workerServiceFull} {
		args := []string{"install", "worker", "--config", "worker.json"}
		if access != "" {
			args = append(args, "--service-access", access)
		}
		opts, err := parseOptions(args)
		if err != nil || opts.WorkerServiceAccess != access {
			t.Fatalf("parse %v: %+v, %v", args, opts, err)
		}
		l := platformWorker(t, "linux")
		if err := configureWorkerServiceAccess(l, opts); err != nil || l.WorkerServiceAccess != access {
			t.Fatalf("configure access %q: %+v, %v", access, l, err)
		}
	}
	for _, args := range [][]string{
		{"install", "worker", "--config", "worker.json", "--service-access", "unknown"},
		{"install", "gateway", "--config", "gateway.json", "--service-access", "full"},
		{"update", "worker", "--service-access", "full"},
		{"adopt", "worker", "--service-access", "full"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted unsupported access option: %v", args)
		}
	}
	for _, tc := range []struct{ action, system string }{{"install", "darwin"}, {"adopt", "linux"}, {"update", "linux"}} {
		l := platformWorker(t, tc.system)
		if err := configureWorkerServiceAccess(l, options{Action: tc.action, Component: "worker", WorkerServiceAccess: "full"}); err == nil || l.WorkerServiceAccess != "" {
			t.Fatalf("accepted incompatible access option: %+v", tc)
		}
	}
}

func TestInstalledWorkerServiceUsesChosenAccessAndPreservesOtherSettings(t *testing.T) {
	template, err := os.ReadFile("../../deploy/systemd/codex-worker.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range []string{workerServiceRestricted, workerServiceFull} {
		t.Run(access, func(t *testing.T) {
			l := platformWorker(t, "linux")
			l.WorkerServiceAccess = access
			config := platformExport(t, l)
			platformWrite(t, l.Config, string(config), 0600)
			pkg := t.TempDir()
			platformWrite(t, filepath.Join(pkg, "deploy/systemd/codex-worker.service"), string(template), 0644)
			var calls [][]string
			m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
				calls = append(calls, append([]string(nil), args...))
				return CommandResult{}, nil
			}}
			if err := m.installService(t.Context(), l, pkg); err != nil {
				t.Fatal(err)
			}
			installed := updateRead(t, l.Unit)
			value := "yes"
			if access == workerServiceFull {
				value = "no"
			}
			for _, key := range []string{"PrivateTmp", "PrivateUsers", "NoNewPrivileges"} {
				if strings.Count(installed, key+"=") != 1 || !strings.Contains(installed, "\n"+key+"="+value+"\n") {
					t.Fatalf("incorrect %s for %s: %s", key, access, installed)
				}
			}
			for _, setting := range []string{"WorkingDirectory=%h", "Environment=HOME=%h", "ExecStart=%h/.local/bin/codex-worker", "TimeoutStopSec=20s", "UMask=0077"} {
				if !strings.Contains(installed, setting) {
					t.Fatalf("access choice lost existing setting %q: %s", setting, installed)
				}
			}
			if !reflect.DeepEqual(calls, [][]string{{"systemctl", "--user", "daemon-reload"}}) || updateRead(t, l.Config) != string(config) {
				t.Fatalf("access setup changed runtime configuration or invoked extra commands: %v", calls)
			}
		})
	}
}

func TestWorkerServiceChoiceSurvivesUpdate(t *testing.T) {
	for _, access := range []string{workerServiceRestricted, workerServiceFull} {
		t.Run(access, func(t *testing.T) {
			l, packages, release := updateFixture(t, "worker")
			l.Unit = filepath.Join(l.Home, "systemd", "codex-worker.service")
			unit, err := renderWorkerServiceAccess([]byte("[Service]\nExecStart=custom-worker-command\n"), access)
			if err != nil {
				t.Fatal(err)
			}
			platformWrite(t, l.Unit, string(unit), 0644)
			dropin := filepath.Join(l.Unit+".d", "custom.conf")
			platformWrite(t, dropin, "[Service]\nEnvironment=EXAMPLE=retained\n", 0644)
			started := false
			m := &Manager{ReadyTimeout: time.Second, Run: func(_ context.Context, args ...string) (CommandResult, error) {
				switch {
				case reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState"}):
					return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
				case reflect.DeepEqual(args, []string{"systemctl", "--user", "start", "codex-worker.service"}):
					started = true
					return CommandResult{}, nil
				case len(args) > 3 && args[0] == l.Binary && args[3] == "status":
					if !started {
						return CommandResult{ExitCode: 1}, nil
					}
					status, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": true, "version": "1.2.3", "runtimes": []any{}})
					return CommandResult{Output: status}, nil
				default:
					t.Fatalf("unexpected update service mutation: %v", args)
					return CommandResult{}, nil
				}
			}}
			if err := m.ApplyUpdate(t.Context(), l, packages, release, false); err != nil {
				t.Fatal(err)
			}
			if updateRead(t, l.Unit) != string(unit) || updateRead(t, dropin) != "[Service]\nEnvironment=EXAMPLE=retained\n" {
				t.Fatal("update changed installed access choice or administrator settings")
			}
		})
	}
}
