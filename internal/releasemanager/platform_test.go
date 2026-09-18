package releasemanager

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func platformWrite(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func platformWorker(t *testing.T, system string) *Layout {
	t.Helper()
	l, err := newLayout("worker", system, "amd64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPlatformLayoutsRejectUnsupportedServicePaths(t *testing.T) {
	for _, tc := range []struct{ component, system, architecture, home string }{
		{"gateway", "darwin", "arm64", "/Users/USER"},
		{"worker", "linux", "386", "/home/USER"},
		{"unknown", "linux", "amd64", "/home/USER"},
		{"worker", "windows", "amd64", "/home/USER"},
		{"worker", "linux", "amd64", "/home/user name"},
		{"worker", "linux", "amd64", "/home/user%name"},
		{"worker", "linux", "amd64", "/home/user\nname"},
		{"worker", "linux", "amd64", "relative/home"},
	} {
		if _, err := newLayout(tc.component, tc.system, tc.architecture, tc.home); err == nil {
			t.Errorf("accepted invalid layout %+v", tc)
		}
	}
	l, err := newLayout("gateway", "linux", "arm64", "/root")
	if err != nil {
		t.Fatal(err)
	}
	if l.Manager != "/usr/local/lib/codex-telegramgw/codex-telegramgw" || l.LegacyManager != "/usr/local/lib/codex-telegramgw/release-manager.py" {
		t.Fatalf("wrong manager paths: %+v", l)
	}
}

func TestPlatformGatewayLayoutDoesNotRequireHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("gateway installation requires Linux")
	}
	for _, home := range []string{"", "relative/home", "/home/user name"} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			if home == "" {
				if err := os.Unsetenv("HOME"); err != nil {
					t.Fatal(err)
				}
			}
			l, err := NewLayout("gateway")
			if err != nil {
				t.Fatalf("gateway depends on HOME: %v", err)
			}
			if l.Config != "/etc/codex-gateway/gateway.json" || l.Command != "/usr/local/bin/codex-telegramgw" || l.UnitDir != "/etc/systemd/system" || l.Home != "" {
				t.Fatalf("gateway did not use system paths: %+v", l)
			}
		})
	}
}

func TestPlatformWorkerLayoutStillRequiresHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux user home environment")
	}
	t.Setenv("HOME", "")
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLayout("worker"); err == nil {
		t.Fatal("worker accepted a missing home directory")
	}
}

func TestPlatformManagerMigratesLegacySymlink(t *testing.T) {
	l := platformWorker(t, "linux")
	platformWrite(t, l.LegacyManager, "old manager", 0755)
	if err := os.MkdirAll(l.Bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(l.LegacyManager, l.Command); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "native-manager")
	platformWrite(t, source, "native manager", 0755)
	m := &Manager{}
	if err := m.InstallManager(l, source); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(l.Command)
	if err != nil || link != l.Manager {
		t.Fatalf("manager symlink = %q, %v", link, err)
	}
	data, err := os.ReadFile(l.Command)
	if err != nil || string(data) != "native manager" {
		t.Fatalf("manager = %q, %v", data, err)
	}
	if !FileExists(l.LegacyManager) {
		t.Fatal("legacy manager removed before readiness succeeded")
	}
	if err := m.InstallManager(l, source); err != nil {
		t.Fatalf("repeat install failed: %v", err)
	}
}

func TestPlatformManagerRefusesOccupiedCommand(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "unrelated-symlink"}[symlink], func(t *testing.T) {
			l := platformWorker(t, "linux")
			source := filepath.Join(t.TempDir(), "native-manager")
			platformWrite(t, source, "native manager", 0755)
			if symlink {
				if err := os.MkdirAll(l.Bin, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(source, l.Command); err != nil {
					t.Fatal(err)
				}
			} else {
				platformWrite(t, l.Command, "another command", 0755)
			}
			if err := (&Manager{}).InstallManager(l, source); err == nil {
				t.Fatal("accepted occupied command")
			}
			if FileExists(l.Manager) {
				t.Fatal("wrote manager before rejecting occupied command")
			}
		})
	}
}

func TestPlatformWorkerResolvesCodexAndNodeForService(t *testing.T) {
	l := platformWorker(t, "linux")
	tools := filepath.Join(t.TempDir(), "toolchain")
	for _, name := range []string{"codex", "node"} {
		platformWrite(t, filepath.Join(tools, name), "#!/bin/sh\nexit 0\n", 0755)
	}
	t.Setenv("PATH", tools)
	profile := map[string]any{"codex_binary": "codex"}
	cfg := map[string]any{"runtimes": []any{profile}}
	path, err := workerExecutablePaths(cfg, l.Home)
	if err != nil {
		t.Fatal(err)
	}
	if profile["codex_binary"] != filepath.Join(tools, "codex") {
		t.Fatalf("executable not resolved: %v", profile)
	}
	if strings.Count(path, tools) != 1 || !strings.HasPrefix(path, tools+":") {
		t.Fatalf("incorrect service PATH: %s", path)
	}
	if err := os.Chmod(filepath.Join(tools, "codex"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := workerExecutablePaths(cfg, l.Home); err == nil {
		t.Fatal("accepted non-executable Codex")
	}
}

func TestPlatformWorkerRejectsServicePathInjection(t *testing.T) {
	for _, bad := range []string{"percent%name", "colon:name", "newline\nname", "quote\"name", "back\\slash"} {
		t.Run(bad, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), bad, "codex")
			platformWrite(t, binary, "#!/bin/sh\nexit 0\n", 0755)
			cfg := map[string]any{"runtimes": []any{map[string]any{"codex_binary": binary}}}
			if _, err := workerExecutablePaths(cfg, t.TempDir()); err == nil {
				t.Fatal("accepted unsafe PATH directory")
			}
		})
	}
}

func TestPlatformGatewayDatabaseStaysWithinServiceData(t *testing.T) {
	root := t.TempDir()
	l := &Layout{DataRoot: filepath.Join(root, "data")}
	if err := os.Mkdir(l.DataRoot, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "config.json")
	for _, path := range []string{"data/nested/gateway.db", filepath.Join(l.DataRoot, "gateway.db")} {
		actual, err := gatewayDatabase(l, map[string]any{"database_path": path}, source)
		if err != nil || !withinDirectory(l.DataRoot, actual) {
			t.Fatalf("valid database rejected: %s, %v", actual, err)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(l.DataRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []map[string]any{
		{"database_path": "data-other/gateway.db"},
		{"database_path": "data/../gateway.db"},
		{"database_path": "data/escape/missing/gateway.db"},
		{"database_path": l.DataRoot},
		{"database_path": "data/gateway.db", "database_url_env": "DATABASE_URL"},
		{},
	} {
		if _, err := gatewayDatabase(l, cfg, source); err == nil {
			t.Fatalf("accepted unsafe database config: %v", cfg)
		}
	}
}

func platformExport(t *testing.T, l *Layout) []byte {
	t.Helper()
	token := filepath.Join(t.TempDir(), "enrollment.token")
	platformWrite(t, token, "private-token", 0600)
	codex := filepath.Join(t.TempDir(), "codex")
	platformWrite(t, codex, "#!/bin/sh\nexit 0\n", 0755)
	cfg := map[string]any{
		"worker_id": "test-worker", "token_file": token, "state_file": filepath.Join(l.Home, ".local/state/codex-worker/state.db"),
		"runtimes": []any{map[string]any{"codex_binary": codex}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPlatformWorkerCopiesPrivateConfigurationOnce(t *testing.T) {
	l := platformWorker(t, "linux")
	exported := platformExport(t, l)
	// A retry may encounter a leftover file from a previous manual setup.
	platformWrite(t, filepath.Join(filepath.Dir(l.Config), "worker.token"), "old-token", 0644)
	calls := 0
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		calls++
		if !reflect.DeepEqual(args[1:], []string{"--config", "/configuration/worker.json", "config", "export"}) {
			t.Fatalf("unexpected export args: %v", args)
		}
		return CommandResult{Output: exported}, nil
	}}
	if err := m.installConfiguration(context.Background(), l, "/configuration/worker.json", "", "/package"); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(l.Config), "worker.token")
	if cfg["token_file"] != path {
		t.Fatalf("token_file = %v", cfg["token_file"])
	}
	for _, file := range []string{path, l.Config} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file mode for %s: %v, %v", file, info, err)
		}
	}
	if err := m.installConfiguration(context.Background(), l, "/configuration/worker.json", "", "/package"); err == nil {
		t.Fatal("overwrote existing configuration")
	}
	if calls != 1 {
		t.Fatalf("called exporter after seeing existing configuration: %d", calls)
	}
}

func TestPlatformPrivateSecretRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	platformWrite(t, target, "unchanged", 0644)
	link := filepath.Join(root, "secret")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateSecret(link, []byte("new-secret"), nil); err == nil {
		t.Fatal("overwrote symlinked secret")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("modified symlink target: %q, %v", data, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("changed target permissions: %v, %v", info, err)
	}
}

func TestPlatformFreshInstallCleansUpBeforeStartFailure(t *testing.T) {
	l := platformWorker(t, "linux")
	exported := platformExport(t, l)
	pkg := t.TempDir()
	platformWrite(t, filepath.Join(pkg, "deploy/systemd/codex-worker.service"), "[Service]\nEnvironment=PATH=old\n", 0644)
	platformWrite(t, l.Manager, "existing-manager", 0755)
	var calls []string
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		calls = append(calls, strings.Join(args, " "))
		if strings.HasSuffix(args[0], "codex-worker") {
			return CommandResult{Output: exported}, nil
		}
		if reflect.DeepEqual(args, []string{"systemctl", "--user", "enable", "codex-worker.service"}) {
			return CommandResult{ExitCode: 1}, nil
		}
		return CommandResult{}, nil
	}}
	err := m.FreshInstall(context.Background(), l, "/source.json", "", map[string]string{"worker": pkg}, &Release{Tag: "v1.0.0"})
	if err == nil {
		t.Fatal("expected service enable failure")
	}
	for _, path := range []string{l.Config, l.Unit, filepath.Join(filepath.Dir(l.Config), "worker.token")} {
		if FileExists(path) {
			t.Errorf("left failed-install file: %s", path)
		}
	}
	data, err := os.ReadFile(l.Manager)
	if err != nil || string(data) != "existing-manager" {
		t.Fatalf("failed install modified preexisting manager: %s, %v", data, err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "systemctl --user disable --now codex-worker.service") {
		t.Fatalf("failed installation did not disable service: %v", calls)
	}
}

func TestPlatformServiceUsesResolvedPATH(t *testing.T) {
	l := platformWorker(t, "linux")
	exported := platformExport(t, l)
	platformWrite(t, l.Config, string(exported), 0600)
	pkg := t.TempDir()
	platformWrite(t, filepath.Join(pkg, "deploy/systemd/codex-worker.service"), "[Service]\nEnvironment=PATH=old\nExecStart=%h/.local/bin/codex-worker run\n", 0644)
	var calls [][]string
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		calls = append(calls, append([]string(nil), args...))
		return CommandResult{}, nil
	}}
	if err := m.installService(context.Background(), l, pkg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.Unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `Environment="PATH=`) || strings.Contains(string(data), "PATH=old") {
		t.Fatalf("wrong service contents: %s", data)
	}
	if !reflect.DeepEqual(calls, [][]string{{"systemctl", "--user", "daemon-reload"}}) {
		t.Fatalf("unexpected service commands: %v", calls)
	}
}

func TestPlatformCanceledFreshInstallStillDisablesService(t *testing.T) {
	l := platformWorker(t, "linux")
	exported := platformExport(t, l)
	pkg := t.TempDir()
	platformWrite(t, filepath.Join(pkg, "deploy/systemd/codex-worker.service"), "[Service]\nEnvironment=PATH=old\n", 0644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	disabled := false
	m := &Manager{Run: func(commandCtx context.Context, args ...string) (CommandResult, error) {
		if strings.HasSuffix(args[0], "codex-worker") {
			return CommandResult{Output: exported}, nil
		}
		if reflect.DeepEqual(args, []string{"systemctl", "--user", "enable", "codex-worker.service"}) {
			cancel()
			return CommandResult{}, context.Canceled
		}
		if commandCtx.Err() != nil {
			t.Error("cleanup reused the canceled installation context")
			return CommandResult{}, commandCtx.Err()
		}
		if reflect.DeepEqual(args, []string{"systemctl", "--user", "disable", "--now", "codex-worker.service"}) {
			disabled = true
		}
		return CommandResult{}, nil
	}}
	err := m.FreshInstall(ctx, l, "/source.json", "", map[string]string{"worker": pkg}, &Release{Tag: "v1.0.0"})
	if !errors.Is(err, context.Canceled) || !disabled {
		t.Fatalf("cancel did not clean up the new service: disabled=%v err=%v", disabled, err)
	}
}

func TestPlatformAutoUpdateLinuxCallsNativeManager(t *testing.T) {
	l := platformWorker(t, "linux")
	var calls [][]string
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		calls = append(calls, append([]string(nil), args...))
		return CommandResult{}, nil
	}}
	if err := m.AutoUpdate(context.Background(), l, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(l.UnitDir, "codex-worker-update.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ExecStart="+l.Command+" update worker\n") || strings.Contains(string(data), "python") {
		t.Fatalf("incorrect update command: %s", data)
	}
	if !strings.Contains(string(data), "SuccessExitStatus=75") {
		t.Fatal("busy deferrals would appear as failed updates")
	}
	if err := m.AutoUpdate(context.Background(), l, false); err != nil {
		t.Fatal(err)
	}
	expected := [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", "codex-worker-update.timer"}, {"systemctl", "--user", "disable", "--now", "codex-worker-update.timer"}}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("unexpected update commands: %v", calls)
	}
}

func TestPlatformDarwinPlistsEscapePathsAndScheduleNativeManager(t *testing.T) {
	l, err := newLayout("worker", "darwin", "arm64", filepath.Join(t.TempDir(), "home&name"))
	if err != nil {
		t.Fatal(err)
	}
	platformWrite(t, l.Config, string(platformExport(t, l)), 0600)
	var calls [][]string
	m := &Manager{Run: func(_ context.Context, args ...string) (CommandResult, error) {
		calls = append(calls, append([]string(nil), args...))
		return CommandResult{}, nil
	}}
	if err := m.installService(context.Background(), l, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.AutoUpdate(context.Background(), l, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{workerPlist(l), filepath.Join(l.Home, "Library/LaunchAgents/com.iaia.codex-worker-update.plist")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "home&amp;name") {
			t.Fatalf("plist did not XML-escape paths: %s", data)
		}
		decoder := xml.NewDecoder(strings.NewReader(string(data)))
		for {
			_, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("invalid XML plist: %v", err)
			}
		}
	}
	if len(calls) != 2 || calls[0][1] != "bootout" || calls[1][1] != "bootstrap" {
		t.Fatalf("unexpected launchd update commands: %v", calls)
	}
	if err := m.AutoUpdate(context.Background(), l, false); err != nil {
		t.Fatal(err)
	}
	if FileExists(filepath.Join(l.Home, "Library/LaunchAgents/com.iaia.codex-worker-update.plist")) {
		t.Fatal("disabled launchd timer can still load on login")
	}
}
