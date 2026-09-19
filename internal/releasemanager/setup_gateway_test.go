package releasemanager

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

func gatewaySetupFixture(t *testing.T) (*Manager, *Layout, string) {
	t.Helper()
	root := t.TempDir()
	l := &Layout{
		Component: "gateway", System: "linux", Architecture: "arm64", Home: filepath.Join(root, "root"),
		Config: filepath.Join(root, "etc/codex-gateway/gateway.json"), Environment: filepath.Join(root, "etc/codex-gateway/secrets.env"),
		Unit: filepath.Join(root, "systemd/codex-gateway.service"), DataRoot: filepath.Join(root, "data"),
	}
	cwd := filepath.Join(root, "prepared")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE", "v0.5.2")
	m := New(nil)
	m.HTTP = &http.Client{Transport: gatewaySetupRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("HTTPS not configured")
	})}
	return m, l, cwd
}

func preparedGatewayConfig(t *testing.T, cwd string, l *Layout) (string, string) {
	t.Helper()
	path, env := filepath.Join(cwd, "gateway.json"), filepath.Join(cwd, "secrets.env")
	platformWrite(t, filepath.Join(cwd, ".botsecrets"), "BOTNAME=example_bot\nBOTTOKEN=123456:private-token\nWLNAME=owner\nWLID=12345\n", 0600)
	platformWrite(t, env, setupWebhookSecretEnv+"="+strings.Repeat("a", 64)+"\n", 0600)
	if err := WriteJSON(path, map[string]any{
		"public_base_url": "https://gateway.example.com", "database_path": filepath.Join(l.DataRoot, "gateway.db"),
		"bot_secrets_file": ".botsecrets", "webhook_secret_env": setupWebhookSecretEnv,
	}); err != nil {
		t.Fatal(err)
	}
	return path, env
}

func TestSetupGatewayRequiresSudoBeforeReadingConfiguration(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("requires an unprivileged Linux test account")
	}
	if err := New(nil).SetupGateway(context.Background()); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("non-root setup = %v", err)
	}
}

func TestSetupGatewayAdoptsWithoutTerminalAndPreservesConfiguration(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	platformWrite(t, l.Config, "existing gateway configuration", 0640)
	before, _ := os.ReadFile(l.Config)
	called := false
	err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
		return nil, errors.New("no terminal")
	}, func(_ context.Context, opts options) error {
		called = true
		if opts != (options{Action: "adopt", Component: "gateway", AutoUpdate: true}) {
			t.Fatalf("unexpected adoption options: %+v", opts)
		}
		return nil
	})
	after, _ := os.ReadFile(l.Config)
	if err != nil || !called || !bytes.Equal(before, after) {
		t.Fatal("existing installation was not preserved", err)
	}
}

func TestSetupGatewayAdoptsThenResumesCompletion(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	prepared, _ := preparedGatewayConfig(t, cwd, l)
	cfg, err := ReadJSON(prepared)
	if err != nil {
		t.Fatal(err)
	}
	cfg["bot_secrets_file"] = filepath.Join(cwd, ".botsecrets")
	if err := WriteJSON(l.Config, cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(l.Config)
	var output bytes.Buffer
	m.Out = &output
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"later"}}
	adopted := false
	err = m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
		if !adopted {
			t.Fatal("completion started before adoption")
		}
		return prompt, nil
	}, func(_ context.Context, opts options) error {
		adopted = true
		if opts != (options{Action: "adopt", Component: "gateway", AutoUpdate: true}) {
			t.Fatalf("unexpected adoption options: %+v", opts)
		}
		return nil
	})
	after, _ := os.ReadFile(l.Config)
	if err != nil || !adopted || !prompt.closed || !bytes.Equal(before, after) || !strings.Contains(output.String(), "sudo codex-telegramgw finish gateway") {
		t.Fatal("adoption did not preserve configuration and resume completion", err)
	}
}

func TestSetupGatewayUsesPreparedFilesWithoutPrompts(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	path, env := preparedGatewayConfig(t, cwd, l)
	configBefore, _ := os.ReadFile(path)
	envBefore, _ := os.ReadFile(env)
	called := false
	var output bytes.Buffer
	m.Out = &output
	err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
		t.Fatal("prepared configuration should not prompt")
		return nil, nil
	}, func(_ context.Context, opts options) error {
		called = true
		if opts != (options{Action: "install", Component: "gateway", Config: path, Environment: env, Version: "v0.5.2", AutoUpdate: true}) {
			t.Fatalf("unexpected install options: %+v", opts)
		}
		return nil
	})
	configAfter, _ := os.ReadFile(path)
	envAfter, _ := os.ReadFile(env)
	if err != nil || !called || FileExists(l.Config) || !bytes.Equal(configBefore, configAfter) || !bytes.Equal(envBefore, envAfter) {
		t.Fatal("prepared configuration flow failed", err)
	}
	if !strings.Contains(output.String(), "127.0.0.1:8080") || !strings.Contains(output.String(), "sudo codex-telegramgw finish gateway") {
		t.Fatal("gateway completion instructions missing")
	}
}

func TestSetupGatewayGuidedConfigIsPrivateValidatedAndTemporary(t *testing.T) {
	var webhookSecrets []string
	for _, installFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[installFails], func(t *testing.T) {
			m, l, cwd := gatewaySetupFixture(t)
			var output bytes.Buffer
			m.Out = &output
			token := "123456789:private-bot-token_aZ-123"
			label := `Owner "quoted" $(not-evaluated) &=:`
			prompt := &scriptedWorkerSetup{t: t, answers: []string{
				"https://gateway.example.com:8443/", "@example_bot", "12345", label, "8090", token, "later",
			}}
			var stage string
			installErr := errors.New("test install failure")
			err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
				if opts.Action != "install" || opts.Component != "gateway" || !opts.AutoUpdate || opts.Version != "v0.5.2" {
					t.Fatalf("unexpected install options: %+v", opts)
				}
				stage = filepath.Dir(opts.Config)
				for _, path := range []string{stage, opts.Config, opts.Environment, filepath.Join(stage, ".botsecrets")} {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm()&0077 != 0 {
						t.Fatal("setup files are not private", err)
					}
				}
				cfg, err := config.LoadGateway(opts.Config)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Listen != "127.0.0.1:8090" || cfg.PublicBaseURL != "https://gateway.example.com:8443" || cfg.DatabasePath != filepath.Join(l.DataRoot, "gateway.db") || cfg.WebhookSecretEnv != setupWebhookSecretEnv {
					t.Fatal("incorrect generated gateway configuration")
				}
				if cfg.Secrets.BotToken != token || cfg.Secrets.BotName != "example_bot" || cfg.Secrets.WLID != 12345 || cfg.Secrets.WLName != label || !reflect.DeepEqual(cfg.AllowedUserIDs, []int64{12345}) {
					t.Fatal("incorrect generated bot secrets")
				}
				data, _ := os.ReadFile(opts.Environment)
				key, secret, ok := strings.Cut(strings.TrimSpace(string(data)), "=")
				decoded, err := hex.DecodeString(secret)
				if !ok || key != cfg.WebhookSecretEnv || err != nil || len(decoded) != 32 {
					t.Fatal("invalid generated webhook secret")
				}
				webhookSecrets = append(webhookSecrets, secret)
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
			wantSecrets := []bool{false, false, false, false, false, true}
			if !installFails {
				wantSecrets = append(wantSecrets, false)
			}
			if stage == "" || FileExists(stage) || !prompt.closed || !reflect.DeepEqual(prompt.secrets, wantSecrets) {
				t.Fatal("setup did not hide token or clean temporary files")
			}
			if strings.Contains(output.String(), token) || (err != nil && strings.Contains(err.Error(), token)) {
				t.Fatal("setup leaked token")
			}
			for _, secret := range webhookSecrets {
				if strings.Contains(output.String(), secret) {
					t.Fatal("setup leaked webhook secret")
				}
			}
			if installFails && strings.Contains(output.String(), "sudo codex-telegramgw finish gateway") {
				t.Fatal("failed install showed completion instructions")
			}
		})
	}
	if len(webhookSecrets) != 2 || webhookSecrets[0] == webhookSecrets[1] {
		t.Fatal("independent installations reused the same webhook secret")
	}
}

func TestSetupGatewayDefaults(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"gateway.example.com", "example_bot", "12345", "", "", "12345:token", "later"}}
	err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		cfg, err := config.LoadGateway(opts.Config)
		if err != nil || cfg.Listen != "127.0.0.1:8080" || cfg.Secrets.WLName != "owner" || cfg.PublicBaseURL != "https://gateway.example.com" {
			t.Fatal("default setup values failed", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetupGatewayCorrectsFieldsWithoutRestarting(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	var output bytes.Buffer
	m.Out = &output
	prompt := &scriptedWorkerSetup{t: t, answers: []string{
		"https://private-token@gateway.example.com", "gateway.example.com",
		"private-token\nWLID=2", "@example_bot",
		"-1", "12345",
		"private-token\nBOTTOKEN=bad", "Owner",
		"443", "8080",
		"private-token", "12345:valid-bot-token",
		"later",
	}}
	installed := false
	err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		installed = true
		cfg, err := config.LoadGateway(opts.Config)
		if err != nil {
			return err
		}
		if cfg.PublicBaseURL != "https://gateway.example.com" || cfg.Secrets.BotName != "example_bot" || cfg.Secrets.WLID != 12345 || cfg.Secrets.WLName != "Owner" || cfg.Listen != "127.0.0.1:8080" || cfg.Secrets.BotToken != "12345:valid-bot-token" {
			t.Fatal("corrected fields did not reach installation")
		}
		return nil
	})
	if err != nil || !installed || !prompt.closed || len(prompt.answers) != 0 || strings.Count(output.String(), "Please try again.") != 6 {
		t.Fatal("fields could not be corrected in place", err, output.String())
	}
	if strings.Contains(output.String(), "private-token") || strings.Contains(output.String(), "valid-bot-token") {
		t.Fatal("correction printed private input")
	}
	if !prompt.secrets[10] || !prompt.secrets[11] {
		t.Fatal("token retry did not retain hidden input")
	}
}

func TestSetupGatewayOrigin(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"gateway.example.com", "https://gateway.example.com"},
		{" gateway.example.com:8443/ ", "https://gateway.example.com:8443"},
		{"https://gateway.example.com/", "https://gateway.example.com"},
		{"http://gateway.example.com", ""},
		{"", ""},
		{"//gateway.example.com", ""},
		{"private-token@gateway.example.com", ""},
		{"gateway.example.com/path", ""},
		{"gateway.example.com?token=private-token", ""},
		{"gateway.example.com#private-token", ""},
	} {
		got, err := setupGatewayOrigin(tc.input)
		if got != tc.want || (err == nil) != (tc.want != "") || (err != nil && strings.Contains(err.Error(), "private-token")) {
			t.Errorf("origin normalization = %q, %v; want %q", got, err, tc.want)
		}
	}
}

type endedGatewaySetup struct{ *scriptedWorkerSetup }

func (p *endedGatewaySetup) Ask(ctx context.Context, label, fallback string, secret bool) (string, error) {
	if len(p.answers) == 0 {
		return "", io.EOF
	}
	return p.scriptedWorkerSetup.Ask(ctx, label, fallback, secret)
}

func TestSetupGatewayRepromptsInvalidInputBeforeInstallation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
	}{
		{"http", []string{"http://gateway.example.com"}},
		{"credential-origin", []string{"https://private-token@gateway.example.com"}},
		{"path-origin", []string{"https://gateway.example.com/extra"}},
		{"bot-injection", []string{"https://gateway.example.com", "private-token\nWLID=2"}},
		{"bot-shell", []string{"https://gateway.example.com", "$(private-token)"}},
		{"negative-user", []string{"https://gateway.example.com", "example_bot", "-1"}},
		{"zero-user", []string{"https://gateway.example.com", "example_bot", "0"}},
		{"overflow-user", []string{"https://gateway.example.com", "example_bot", "999999999999999999999999"}},
		{"label-injection", []string{"https://gateway.example.com", "example_bot", "12345", "private-token\nBOTTOKEN=bad"}},
		{"privileged-port", []string{"https://gateway.example.com", "example_bot", "12345", "", "443"}},
		{"large-port", []string{"https://gateway.example.com", "example_bot", "12345", "", "65536"}},
		{"token", []string{"https://gateway.example.com", "example_bot", "12345", "", "", "private-token"}},
		{"token-injection", []string{"https://gateway.example.com", "example_bot", "12345", "", "", "12345:private-token\nWLID=2"}},
		{"token-control", []string{"https://gateway.example.com", "example_bot", "12345", "", "", "12345:private-token\x00"}},
		{"token-trailing-newline", []string{"https://gateway.example.com", "example_bot", "12345", "", "", "12345:private-token\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l, cwd := gatewaySetupFixture(t)
			var output bytes.Buffer
			m.Out = &output
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			prompt := &endedGatewaySetup{&scriptedWorkerSetup{t: t, answers: tc.answers}}
			err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error {
				t.Fatal("invalid setup attempted an installation")
				return nil
			})
			if !errors.Is(err, io.EOF) || !prompt.closed || FileExists(l.Config) || !strings.Contains(output.String(), "Please try again.") || strings.Contains(err.Error()+output.String(), "private-token") {
				t.Fatal("invalid setup was not rejected privately", err)
			}
			files, _ := os.ReadDir(temp)
			if len(files) != 0 {
				t.Fatal("invalid setup left temporary files")
			}
		})
	}
}

func TestSetupGatewayRejectsUnattendedFreshSetup(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) { return nil, errors.New("no tty") }, func(context.Context, options) error {
		t.Fatal("unattended setup attempted installation")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "terminal or private gateway.json") || FileExists(l.Config) {
		t.Fatal(err)
	}
}

func TestSetupGatewayRejectsUnsafeOrIncompletePreparedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, file, action string
	}{
		{"missing-config", "gateway.json", "remove"}, {"missing-env", "secrets.env", "remove"}, {"missing-bot", ".botsecrets", "remove"},
		{"public-config", "gateway.json", "public"}, {"public-env", "secrets.env", "public"}, {"public-bot", ".botsecrets", "public"},
		{"linked-config", "gateway.json", "symlink"}, {"linked-env", "secrets.env", "symlink"}, {"linked-bot", ".botsecrets", "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l, cwd := gatewaySetupFixture(t)
			preparedGatewayConfig(t, cwd, l)
			path := filepath.Join(cwd, tc.file)
			switch tc.action {
			case "remove":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
			}
			err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
				t.Fatal("unsafe prepared configuration prompted")
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

func TestSetupGatewayRejectsIncompleteInstalledFiles(t *testing.T) {
	for _, filename := range []string{"environment", "bot", "service"} {
		t.Run(filename, func(t *testing.T) {
			m, l, cwd := gatewaySetupFixture(t)
			path := map[string]string{"environment": l.Environment, "bot": filepath.Join(filepath.Dir(l.Config), ".botsecrets"), "service": l.Unit}[filename]
			platformWrite(t, path, "existing private settings", 0600)
			err := m.setupGateway(context.Background(), l, cwd, func() (workerSetupPrompt, error) {
				t.Fatal("incomplete installation should not prompt")
				return nil, nil
			}, func(context.Context, options) error {
				t.Fatal("incomplete installed configuration was overwritten")
				return nil
			})
			data, _ := os.ReadFile(path)
			if err == nil || string(data) != "existing private settings" {
				t.Fatal("incomplete installation was not preserved", err)
			}
		})
	}
}

type cancellingGatewaySetup struct {
	*scriptedWorkerSetup
	cancel context.CancelFunc
}

func (p *cancellingGatewaySetup) Ask(ctx context.Context, label, fallback string, secret bool) (string, error) {
	value, err := p.scriptedWorkerSetup.Ask(ctx, label, fallback, secret)
	if secret {
		p.cancel()
	}
	return value, err
}

func TestSetupGatewayCancellationStopsBeforeWritingSecrets(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prompt := &cancellingGatewaySetup{
		scriptedWorkerSetup: &scriptedWorkerSetup{t: t, answers: []string{"https://gateway.example.com", "example_bot", "12345", "", "", "12345:token"}},
		cancel:              cancel,
	}
	err := m.setupGateway(ctx, l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error {
		t.Fatal("cancelled setup attempted installation")
		return nil
	})
	files, _ := os.ReadDir(temp)
	if !errors.Is(err, context.Canceled) || !prompt.closed || len(files) != 0 {
		t.Fatal("cancelled setup did not close and clean up", err)
	}
}

func TestSetupGatewayRejectsInvalidBootstrapRelease(t *testing.T) {
	m, l, cwd := gatewaySetupFixture(t)
	preparedGatewayConfig(t, cwd, l)
	t.Setenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE", "../invalid")
	err := m.setupGateway(context.Background(), l, cwd, nil, func(context.Context, options) error {
		t.Fatal("invalid bootstrap version reached installation")
		return nil
	})
	if err == nil || FileExists(l.Config) {
		t.Fatal(err)
	}
}

func TestSetupGatewayValidatesPreparedWebhookSecret(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		valid          bool
	}{
		{"single-quoted", setupWebhookSecretEnv + "='" + strings.Repeat("a", 32) + "'\n", true},
		{"double-quoted", setupWebhookSecretEnv + "=\"" + strings.Repeat("a", 32) + "\"\n", true},
		{"missing", "OTHER_SECRET=" + strings.Repeat("a", 32) + "\n", false},
		{"short", setupWebhookSecretEnv + "=private-token\n", false},
		{"duplicate", strings.Repeat(setupWebhookSecretEnv+"="+strings.Repeat("a", 32)+"\n", 2), false},
		{"shell", "export " + setupWebhookSecretEnv + "=" + strings.Repeat("a", 32) + "\n", false},
		{"multiline", setupWebhookSecretEnv + "=\"private-token\nsecond-line\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, l, cwd := gatewaySetupFixture(t)
			path, env := preparedGatewayConfig(t, cwd, l)
			platformWrite(t, env, tc.contents, 0600)
			err := validatePreparedGateway(path, env)
			if (err == nil) != tc.valid || (err != nil && strings.Contains(err.Error(), "private-token")) {
				t.Fatal("incorrect prepared webhook validation", err)
			}
		})
	}
}
