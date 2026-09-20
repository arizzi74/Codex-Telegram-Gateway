package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
)

type scriptedWorkerSetup struct {
	t       *testing.T
	answers []string
	secrets []bool
	closed  bool
	endErr  error
}

func (p *scriptedWorkerSetup) Ask(_ context.Context, _ string, fallback string, secret bool) (string, error) {
	p.t.Helper()
	if len(p.answers) == 0 {
		if p.endErr != nil {
			return "", p.endErr
		}
		p.t.Fatal("unexpected extra prompt")
	}
	answer := p.answers[0]
	p.answers = p.answers[1:]
	p.secrets = append(p.secrets, secret)
	if answer == "" {
		answer = fallback
	}
	return answer, nil
}

func (p *scriptedWorkerSetup) Close() error { p.closed = true; return nil }

func setupFixture(t *testing.T) (*Manager, *Layout, string) {
	t.Helper()
	root := t.TempDir()
	l, err := newLayout("worker", "linux", "arm64", filepath.Join(root, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(l.Home, 0700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "project with 'quotes'")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := AtomicWrite(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0755, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE", "v0.5.1")
	m := New(nil)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if len(args) >= 2 && args[0] == "systemctl" && args[1] == "--user" {
			return CommandResult{}, nil
		}
		if len(args) >= 2 && args[0] == "loginctl" && args[1] == "show-user" {
			return CommandResult{Output: []byte("yes\n")}, nil
		}
		if len(args) == 3 && args[0] == filepath.Join(bin, "codex") && args[1] == "login" && args[2] == "status" {
			return CommandResult{Output: []byte("Logged in using ChatGPT\n")}, nil
		}
		t.Fatalf("unexpected setup command: %q", args)
		return CommandResult{}, nil
	}
	return m, l, cwd
}

func preparedWorkerConfig(t *testing.T, cwd string) string {
	t.Helper()
	token := filepath.Join(cwd, "worker.token")
	if err := os.WriteFile(token, []byte("test-enrollment-token"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "worker.json")
	cfg := config.WorkerConfig{
		WorkerID: "00000000-0000-4000-8000-000000000001", Name: "test-worker",
		StateFile: "state/worker.db", TokenFile: token,
		GatewayURL:            "wss://gateway.example.com/tgapi/v1/workers/connect",
		AllowedWorkspaceRoots: []string{cwd},
		Runtimes:              []config.RuntimeProfile{{ID: "primary", CodexBinary: "codex", WorkingDirectory: cwd, Autostart: true}},
	}
	if err := WriteJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetupWorkerAdoptsWithoutRestartOrConfigChanges(t *testing.T) {
	m, l := coreWorkerHome(t)
	configBefore, _ := os.ReadFile(l.Config)
	serviceBefore := "[Service]\nPrivateTmp=no\nPrivateUsers=no\nNoNewPrivileges=no\n"
	platformWrite(t, l.Unit, serviceBefore, 0644)
	workerState := filepath.Join(l.Home, ".local/state/codex-worker/worker.db")
	if err := AtomicWrite(workerState, []byte("existing worker state"), 0600, nil); err != nil {
		t.Fatal(err)
	}
	var commands []string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		command := strings.Join(args, " ")
		commands = append(commands, command)
		if len(args) == 2 && args[0] == l.Binary && args[1] == "version" {
			return CommandResult{Output: []byte("0.5.0\n")}, nil
		}
		if command == "systemctl --user daemon-reload" || command == "systemctl --user enable --now codex-worker-update.timer" ||
			(strings.HasPrefix(command, "launchctl ") && (strings.Contains(command, "codex-worker-update") || strings.Contains(command, "LaunchAgents"))) {
			return CommandResult{}, nil
		}
		t.Fatalf("unexpected service operation: %s", command)
		return CommandResult{}, nil
	}
	if err := m.SetupWorker(context.Background()); err != nil {
		t.Fatal(err)
	}
	configAfter, _ := os.ReadFile(l.Config)
	stateAfter, _ := os.ReadFile(workerState)
	if !bytes.Equal(configBefore, configAfter) || string(stateAfter) != "existing worker state" || updateRead(t, l.Unit) != serviceBefore {
		t.Fatal("adoption changed worker configuration, service access, or state")
	}
	if len(commands) < 3 {
		t.Fatal("automatic updates were not enabled")
	}
}

func TestSetupWorkerUsesPreparedPrivateConfigWithoutPrompts(t *testing.T) {
	m, l, cwd := setupFixture(t)
	path := preparedWorkerConfig(t, cwd)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
		t.Fatal("prepared configuration should not prompt")
		return nil, nil
	}, func(_ context.Context, opts options) error {
		called = true
		want := options{Action: "install", Component: "worker", Config: path, Version: "v0.5.1", AutoUpdate: true}
		if opts != want {
			t.Fatalf("options = %+v, want %+v", opts, want)
		}
		return nil
	})
	if err != nil || !called || FileExists(l.Config) {
		t.Fatal("prepared configuration flow failed", err, called)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("prepared worker configuration or custom workspace roots changed", err)
	}
}

func TestSetupWorkerGuidedConfigIsPrivateValidatedAndTemporary(t *testing.T) {
	for _, installFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[installFails], func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			var output bytes.Buffer
			m.Out = &output
			token := `private-token-"quotes"-$()-&=:`
			prompt := &scriptedWorkerSetup{t: t, answers: []string{
				"https://gateway.example.com:8443/", "00000000-0000-4000-8000-000000000001", `Worker "one"`, "", "", token,
			}}
			var stage string
			installErr := errors.New("test install failure")
			err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
				if opts.Action != "install" || opts.Component != "worker" || !opts.AutoUpdate || opts.Version != "v0.5.1" || opts.WorkerServiceAccess != workerServiceRestricted {
					t.Fatalf("unexpected install options: %+v", opts)
				}
				stage = filepath.Dir(opts.Config)
				for _, path := range []string{stage, opts.Config, filepath.Join(stage, "worker.token")} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm()&0077 != 0 {
						t.Fatal("setup files are not private", err)
					}
				}
				cfg, err := config.LoadWorker(opts.Config)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.GatewayURL != "wss://gateway.example.com:8443/tgapi/v1/workers/connect" || cfg.Name != `Worker "one"` || cfg.StateFile != filepath.Join(l.Home, ".local/state/codex-worker/worker.db") {
					t.Fatalf("bad generated worker config: %+v", cfg)
				}
				canonicalHome, _ := filepath.EvalSymlinks(l.Home)
				if !reflect.DeepEqual(cfg.AllowedWorkspaceRoots, []string{canonicalHome}) || len(cfg.Runtimes) != 1 || cfg.Runtimes[0].WorkingDirectory != canonicalHome || !cfg.Runtimes[0].Autostart || !filepath.IsAbs(cfg.Runtimes[0].CodexBinary) {
					t.Fatalf("bad generated runtime config: %+v", cfg.Runtimes)
				}
				data, _ := os.ReadFile(cfg.TokenFile)
				if string(data) != token+"\n" {
					t.Fatal("token was altered")
				}
				if FileExists(l.Config) {
					t.Fatal("installed config was written before installation")
				}
				if installFails {
					return installErr
				}
				return nil
			})
			if (installFails && !errors.Is(err, installErr)) || (!installFails && err != nil) {
				t.Fatal(err)
			}
			if stage == "" || FileExists(stage) || !prompt.closed || !reflect.DeepEqual(prompt.secrets, []bool{false, false, false, false, false, true}) {
				t.Fatal("setup did not hide token or clean temporary files")
			}
			if strings.Contains(output.String(), token) || (err != nil && strings.Contains(err.Error(), token)) {
				t.Fatal("setup leaked token")
			}
			if !strings.Contains(output.String(), "https://gateway.example.com:8443/tgadmin/") || !strings.Contains(output.String(), "Enroll worker") {
				t.Fatal("setup did not guide gateway enrollment")
			}
		})
	}
}

func TestSetupWorkerGuidedRootsCoverHomeAndOnlyExplicitOutsideWorkspaces(t *testing.T) {
	for _, choice := range []string{"default", "home project", "outside", "explicit symlink"} {
		t.Run(choice, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			m.Out = io.Discard
			project := filepath.Join(l.Home, "projects", "first")
			secondProject := filepath.Join(l.Home, "projects", "second", "nested")
			hiddenProject := filepath.Join(l.Home, ".private-project")
			lookalike := l.Home + "-other-user"
			external := t.TempDir()
			for _, directory := range []string{project, secondProject, hiddenProject, lookalike} {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(external, filepath.Join(l.Home, "external-link")); err != nil {
				t.Fatal(err)
			}
			answer, runtime := "", l.Home
			wantRoots := []string{l.Home}
			switch choice {
			case "home project":
				answer, runtime = "~/projects/first", project
			case "outside":
				answer, runtime = cwd, cwd
				wantRoots = append(wantRoots, cwd)
			case "explicit symlink":
				answer, runtime = "~/external-link", external
				wantRoots = append(wantRoots, external)
			}
			wantRoots, _ = auth.CanonicalWorkspaceRoots(wantRoots)
			runtime, _ = filepath.EvalSymlinks(runtime)
			prompt := &scriptedWorkerSetup{t: t, answers: []string{
				"https://gateway.example.com", "00000000-0000-4000-8000-000000000001", "", answer, "", "enrollment-token",
			}}
			called := false
			err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
				called = true
				cfg, err := config.LoadWorker(opts.Config)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(cfg.AllowedWorkspaceRoots, wantRoots) || cfg.Runtimes[0].WorkingDirectory != runtime {
					t.Fatalf("roots = %q, runtime = %q; want roots %q, runtime %q", cfg.AllowedWorkspaceRoots, cfg.Runtimes[0].WorkingDirectory, wantRoots, runtime)
				}
				newProject := filepath.Join(l.Home, "created-after-setup")
				if err := os.Mkdir(newProject, 0700); err != nil {
					t.Fatal(err)
				}
				for _, directory := range []string{l.Home, project, secondProject, hiddenProject, newProject} {
					if _, err := auth.CanonicalWorkspace(directory, cfg.AllowedWorkspaceRoots); err != nil {
						t.Fatalf("home subfolder %q is not available: %v", directory, err)
					}
				}
				for _, test := range []struct {
					directory string
					allowed   bool
				}{
					{cwd, choice == "outside"},
					{external, choice == "explicit symlink"},
					{filepath.Join(l.Home, "external-link"), choice == "explicit symlink"},
					{lookalike, false},
				} {
					_, err := auth.CanonicalWorkspace(test.directory, cfg.AllowedWorkspaceRoots)
					if (err == nil) != test.allowed {
						t.Fatalf("outside directory %q allowed = %v, want %v", test.directory, err == nil, test.allowed)
					}
				}
				return nil
			})
			if err != nil || !called || !prompt.closed || len(prompt.answers) != 0 {
				t.Fatal("guided worker setup failed", err)
			}
		})
	}
}

func TestSetupWorkerRetriesInvalidAnswersWithoutRestartingSetup(t *testing.T) {
	m, l, cwd := setupFixture(t)
	var output bytes.Buffer
	m.Out = &output
	prompt := &scriptedWorkerSetup{t: t, answers: []string{
		"https://private-token@example.com", "https://gateway.example.com",
		"private-token", "00000000-0000-4000-8000-000000000001",
		"   ", "Worker one",
		"missing-directory", "~",
		"invalid-access", "no",
		"private-token\nsecond-line", "valid-enrollment-token",
	}}
	called := false
	err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		called = true
		cfg, err := config.LoadWorker(opts.Config)
		if err != nil || cfg.Name != "Worker one" || opts.WorkerServiceAccess != workerServiceFull {
			t.Fatal("corrected answers did not produce a valid configuration", err)
		}
		return nil
	})
	if err != nil || !called || !prompt.closed || len(prompt.answers) != 0 {
		t.Fatal("worker setup did not recover from invalid answers", err)
	}
	if !reflect.DeepEqual(prompt.secrets, []bool{false, false, false, false, false, false, false, false, false, false, true, true}) {
		t.Fatal("enrollment token was not hidden on every attempt")
	}
	if strings.Contains(output.String(), "private-token") || strings.Contains(output.String(), "valid-enrollment-token") {
		t.Fatal("invalid or valid secret input leaked")
	}
}

func TestSetupWorkerRejectsUnattendedFreshSetupWithoutMutation(t *testing.T) {
	m, l, cwd := setupFixture(t)
	err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
		return nil, errors.New("no tty")
	}, func(context.Context, options) error {
		t.Fatal("unattended setup attempted an installation")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "terminal or a private worker.json") || FileExists(l.Config) {
		t.Fatal(err)
	}
}

func TestSetupWorkerRejectsInvalidInputBeforeInstallation(t *testing.T) {
	for _, test := range []struct {
		name    string
		answers []string
	}{
		{"gateway", []string{"https://private-token@example.com"}},
		{"identity", []string{"https://gateway.example.com", "private-token"}},
		{"workspace", []string{"https://gateway.example.com", "00000000-0000-4000-8000-000000000001", "", "missing-directory"}},
		{"access", []string{"https://gateway.example.com", "00000000-0000-4000-8000-000000000001", "", "", "invalid-access"}},
		{"token", []string{"https://gateway.example.com", "00000000-0000-4000-8000-000000000001", "", "", "", "private-token\nsecond-line"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			var output bytes.Buffer
			m.Out = &output
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			prompt := &scriptedWorkerSetup{t: t, answers: test.answers, endErr: io.EOF}
			err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error {
				t.Fatal("invalid setup attempted an installation")
				return nil
			})
			if !errors.Is(err, io.EOF) || !prompt.closed || FileExists(l.Config) || strings.Contains(err.Error()+output.String(), "private-token") {
				t.Fatal("invalid setup was not rejected privately", err)
			}
			files, _ := os.ReadDir(temp)
			if len(files) != 0 {
				t.Fatal("invalid setup left temporary files")
			}
		})
	}
}

func TestSetupWorkerRejectsPublicOrLinkedPreparedConfig(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "linked"}[linked], func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			path := preparedWorkerConfig(t, cwd)
			if linked {
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
				t.Fatal("unsafe prepared configuration opened prompts")
				return nil, nil
			}, func(context.Context, options) error {
				t.Fatal("unsafe prepared configuration installed")
				return nil
			})
			if err == nil || FileExists(l.Config) {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupWorkerRejectsInvalidBootstrapRelease(t *testing.T) {
	m, l, cwd := setupFixture(t)
	preparedWorkerConfig(t, cwd)
	t.Setenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE", "../invalid")
	err := m.setupWorker(context.Background(), l, cwd, nil, func(context.Context, options) error {
		t.Fatal("invalid bootstrap version reached installation")
		return nil
	})
	if err == nil || FileExists(l.Config) {
		t.Fatal(err)
	}
}

func TestSetupGatewayURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"gateway.example.com", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"gateway.example.com:8443", "wss://gateway.example.com:8443/tgapi/v1/workers/connect"},
		{" gateway.example.com/tgadmin/ ", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"https://gateway.example.com/tgadmin", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"https://gateway.example.com:8443/tgadmin/", "wss://gateway.example.com:8443/tgapi/v1/workers/connect"},
		{"https://gateway.example.com", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"https://[::1]:8443/", "wss://[::1]:8443/tgapi/v1/workers/connect"},
		{"wss://gateway.example.com", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"wss://gateway.example.com/", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"wss://gateway.example.com/api/v1/workers/connect", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"wss://gateway.example.com:8443/api/v1/workers/connect/", "wss://gateway.example.com:8443/tgapi/v1/workers/connect"},
		{"wss://gateway.example.com/tgapi/v1/workers/connect", "wss://gateway.example.com/tgapi/v1/workers/connect"},
		{"wss://gateway.example.com/custom/connect", "wss://gateway.example.com/custom/connect"},
		{"http://gateway.example.com", ""},
		{"https://user:token@gateway.example.com/tgadmin/", ""},
		{"https://gateway.example.com/tgadmin/?token=private", ""},
		{"https://gateway.example.com/tgadmin/?", ""},
		{"https://gateway.example.com/tgadmin/#", ""},
		{"https://gateway.example.com/tgadmin/#private", ""},
		{"https://gateway.example.com/tg%61dmin/", ""},
		{"user:token@gateway.example.com/tgadmin/", ""},
		{"gateway.example.com/tgadmin/?token=private", ""},
		{"gateway.example.com/tgadmin/#", ""},
		{"", ""},
		{"//gateway.example.com", ""},
		{"https://gateway.example.com/path", ""},
		{"wss://user:token@gateway.example.com/path", ""},
		{"wss://gateway.example.com/path?token=private", ""},
		{"wss://gateway.example.com/path?", ""},
		{"wss://gateway.example.com/path#", ""},
		{"wss://gateway.example.com:0/path", ""},
		{"wss://gateway.example.com:65536/path", ""},
		{"wss://gateway.example.com:/path", ""},
		{"wss:///path", ""},
		{"%invalid", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := setupGatewayURL(test.input)
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatal(got, err)
			}
		})
	}
}
