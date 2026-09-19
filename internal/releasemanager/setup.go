package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
)

type workerSetupPrompt interface {
	Ask(context.Context, string, string, bool) (string, error)
	Close() error
}

// SetupWorker uses an existing installation or a local worker.json when present.
// A fresh interactive setup reads the controlling terminal, since the bootstrap
// script may occupy standard input when installed with curl | sh.
func (m *Manager) SetupWorker(ctx context.Context) error {
	l, err := NewLayout("worker")
	if err != nil {
		return err
	}
	if err := l.RequireUser(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return m.setupWorker(ctx, l, cwd, openWorkerSetupTerminal, m.execute)
}

func (m *Manager) setupWorker(ctx context.Context, l *Layout, cwd string, openPrompt func() (workerSetupPrompt, error), execute func(context.Context, options) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	version := os.Getenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE")
	if version != "" {
		if _, err := ParseVersion(version); err != nil {
			return errors.New("invalid bootstrap release version")
		}
	}
	if _, err := os.Lstat(l.Config); err == nil {
		return execute(ctx, options{Action: "adopt", Component: "worker", AutoUpdate: true})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	prepared := filepath.Join(cwd, "worker.json")
	if info, err := os.Lstat(prepared); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("worker.json must be a regular private file; set its permissions to 0600")
		}
		if _, err := config.LoadWorker(prepared); err != nil {
			return errors.New("worker.json is not a valid private worker configuration; check its settings and token file")
		}
		return execute(ctx, options{Action: "install", Component: "worker", Config: prepared, Version: version, AutoUpdate: true})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	prompt, err := openPrompt()
	if err != nil {
		return errors.New("worker setup needs a terminal or a private worker.json in the current directory; run this command in an interactive terminal")
	}
	defer prompt.Close()
	codex, err := exec.LookPath("codex")
	if err != nil {
		return errors.New("Codex CLI is not available in PATH; install and authenticate Codex, then run this installer again")
	}
	codex, err = filepath.Abs(codex)
	if err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Set up a worker using the worker ID and enrollment token from your gateway. Daily automatic updates will be enabled.")
	gateway, err := prompt.Ask(ctx, "Gateway HTTPS address (or full WSS URL)", "", false)
	if err != nil {
		return err
	}
	gateway, err = setupGatewayURL(gateway)
	if err != nil {
		return err
	}
	workerID, err := prompt.Ask(ctx, "Enrolled worker ID (UUID)", "", false)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(strings.TrimSpace(workerID))
	if err != nil {
		return errors.New("worker ID must be the UUID assigned by your gateway during enrollment")
	}
	name, err := prompt.Ask(ctx, "Worker name", "my-worker", false)
	if err != nil {
		return err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = "my-worker"
	}
	workspace, err := prompt.Ask(ctx, "Workspace directory", cwd, false)
	if err != nil {
		return err
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = cwd
	}
	if workspace == "~" {
		workspace = l.Home
	} else if strings.HasPrefix(workspace, "~/") {
		workspace = filepath.Join(l.Home, workspace[2:])
	} else if !filepath.IsAbs(workspace) {
		workspace = filepath.Join(cwd, workspace)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return errors.New("workspace must be an existing directory")
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return errors.New("workspace must be an existing directory")
	}
	// Ask for the secret last, after validating the non-secret settings.
	token, err := prompt.Ask(ctx, "Worker enrollment token (hidden)", "", true)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("worker enrollment token must be a nonempty single line")
	}
	stage, err := os.MkdirTemp("", "codex-telegramgw-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	tokenPath := filepath.Join(stage, "worker.token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
		return err
	}
	cfg := config.WorkerConfig{
		WorkerID: id.String(), Name: name,
		StateFile:  filepath.Join(l.Home, ".local/state/codex-worker/worker.db"),
		GatewayURL: gateway, TokenFile: tokenPath,
		AllowedWorkspaceRoots: []string{workspace}, MaxQueuedTurns: 20,
		Runtimes: []config.RuntimeProfile{{
			ID: "primary", Name: "Primary Codex", CodexBinary: codex,
			WorkingDirectory: workspace, Autostart: true, RestartPolicy: "on-failure",
		}},
	}
	configPath := filepath.Join(stage, "worker.json")
	if err := WriteJSON(configPath, cfg); err != nil {
		return err
	}
	if _, err := config.LoadWorker(configPath); err != nil {
		return errors.New("worker setup configuration failed validation; check the workspace and enrollment settings")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return execute(ctx, options{Action: "install", Component: "worker", Config: configPath, Version: version, AutoUpdate: true})
}

func setupGatewayURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	u, err := url.Parse(value)
	if err == nil && u.Scheme == "https" {
		u, err = config.ParseHTTPSOrigin(value)
		if err == nil {
			u.Scheme = "wss"
			u.Path = config.WorkerConnectPath
			return u.String(), nil
		}
	}
	if err == nil && u.Scheme == "wss" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(value, "#") {
		if _, err := config.ParseHTTPSOrigin((&url.URL{Scheme: "https", Host: u.Host}).String()); err == nil {
			if u.Path == "" || u.Path == "/" {
				u.Path = config.WorkerConnectPath
			}
			return config.NormalizeGatewayURL(u.String()), nil
		}
	}
	return "", errors.New("gateway address must be an HTTPS origin or WSS URL without credentials, query, or fragment")
}
