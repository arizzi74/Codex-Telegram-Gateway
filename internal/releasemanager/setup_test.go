package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

type scriptedWorkerSetup struct {
	t       *testing.T
	answers []string
	secrets []bool
	closed  bool
}

func (p *scriptedWorkerSetup) Ask(_ context.Context, _ string, fallback string, secret bool) (string, error) {
	p.t.Helper()
	if len(p.answers) == 0 {
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
	return New(nil), l, cwd
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
		GatewayURL:            "wss://gateway.example.com/api/v1/workers/connect",
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
	if !bytes.Equal(configBefore, configAfter) || string(stateAfter) != "existing worker state" {
		t.Fatal("adoption changed worker configuration or state")
	}
	if len(commands) < 3 {
		t.Fatal("automatic updates were not enabled")
	}
}

func TestSetupWorkerUsesPreparedPrivateConfigWithoutPrompts(t *testing.T) {
	m, l, cwd := setupFixture(t)
	path := preparedWorkerConfig(t, cwd)
	called := false
	err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
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
}

func TestSetupWorkerGuidedConfigIsPrivateValidatedAndTemporary(t *testing.T) {
	for _, installFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[installFails], func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			var output bytes.Buffer
			m.Out = &output
			token := `private-token-"quotes"-$()-&=:`
			prompt := &scriptedWorkerSetup{t: t, answers: []string{
				"https://gateway.example.com:8443/", "00000000-0000-4000-8000-000000000001", `Worker "one"`, "", token,
			}}
			var stage string
			installErr := errors.New("test install failure")
			err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
				if opts.Action != "install" || opts.Component != "worker" || !opts.AutoUpdate || opts.Version != "v0.5.1" {
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
				if cfg.GatewayURL != "wss://gateway.example.com:8443/api/v1/workers/connect" || cfg.Name != `Worker "one"` || cfg.StateFile != filepath.Join(l.Home, ".local/state/codex-worker/worker.db") {
					t.Fatalf("bad generated worker config: %+v", cfg)
				}
				canonicalWorkspace, _ := filepath.EvalSymlinks(cwd)
				if !reflect.DeepEqual(cfg.AllowedWorkspaceRoots, []string{canonicalWorkspace}) || len(cfg.Runtimes) != 1 || cfg.Runtimes[0].WorkingDirectory != canonicalWorkspace || !cfg.Runtimes[0].Autostart || !filepath.IsAbs(cfg.Runtimes[0].CodexBinary) {
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
			if stage == "" || FileExists(stage) || !prompt.closed || !reflect.DeepEqual(prompt.secrets, []bool{false, false, false, false, true}) {
				t.Fatal("setup did not hide token or clean temporary files")
			}
			if strings.Contains(output.String(), token) || (err != nil && strings.Contains(err.Error(), token)) {
				t.Fatal("setup leaked token")
			}
		})
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
	if err == nil || !strings.Contains(err.Error(), "terminal or a private worker.json") || FileExists(l.Home) {
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
		{"token", []string{"https://gateway.example.com", "00000000-0000-4000-8000-000000000001", "", "", "private-token\nsecond-line"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			var output bytes.Buffer
			m.Out = &output
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			prompt := &scriptedWorkerSetup{t: t, answers: test.answers}
			err := m.setupWorker(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error {
				t.Fatal("invalid setup attempted an installation")
				return nil
			})
			if err == nil || !prompt.closed || FileExists(l.Home) || strings.Contains(err.Error()+output.String(), "private-token") {
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
			if err == nil || FileExists(l.Home) {
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
	if err == nil || FileExists(l.Home) {
		t.Fatal(err)
	}
}

func TestSetupGatewayURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://gateway.example.com", "wss://gateway.example.com/api/v1/workers/connect"},
		{"https://[::1]:8443/", "wss://[::1]:8443/api/v1/workers/connect"},
		{"wss://gateway.example.com", "wss://gateway.example.com/api/v1/workers/connect"},
		{"wss://gateway.example.com/custom/connect", "wss://gateway.example.com/custom/connect"},
		{"http://gateway.example.com", ""},
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
