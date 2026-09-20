package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
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
	if err := m.prepareWorkerSetupService(ctx, l); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Set up this machine as a worker. Press Enter to accept each default. Daily automatic updates will be enabled.")
	gateway, err := askSetupValue(ctx, prompt, m.Out, "Gateway address (hostname or HTTPS URL)", "", false, setupGatewayURL)
	if err != nil {
		return err
	}
	adminURL, _ := url.Parse(gateway)
	adminURL.Scheme, adminURL.Path, adminURL.RawPath = "https", "/tgadmin/", ""
	fmt.Fprintf(m.Out, "Open %s, sign in, and choose Enroll worker. Keep its Worker ID and enrollment token ready; the token is shown only once.\n", adminURL.String())
	workerID, err := askSetupValue(ctx, prompt, m.Out, "Enrolled worker ID (UUID)", "", false, func(value string) (string, error) {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			return "", errors.New("worker ID must be the UUID assigned by your gateway during enrollment")
		}
		return id.String(), nil
	})
	if err != nil {
		return err
	}
	name, err := askSetupValue(ctx, prompt, m.Out, "Worker name", "my-worker", false, func(value string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("worker name must be a nonempty single line")
		}
		return value, nil
	})
	if err != nil {
		return err
	}
	roots, err := auth.CanonicalWorkspaceRoots([]string{l.Home})
	if err != nil {
		return errors.New("worker home must be an existing directory")
	}
	fmt.Fprintf(m.Out, "The worker can use your entire home directory (%s), including all subfolders. Choosing a starting directory elsewhere also allows that directory and its subfolders.\n", l.Home)
	workspace, err := askSetupValue(ctx, prompt, m.Out, "Starting directory", l.Home, false, func(value string) (string, error) {
		return setupWorkerDirectory(value, l.Home, cwd)
	})
	if err != nil {
		return err
	}
	if _, err := auth.CanonicalWorkspace(workspace, roots); err != nil {
		roots = append(roots, workspace)
	}
	serviceAccess, err := m.setupWorkerServiceAccess(ctx, l, prompt)
	if err != nil {
		return err
	}
	// Ask for the secret last, after validating the non-secret settings.
	token, err := askSetupValue(ctx, prompt, m.Out, "Worker enrollment token (hidden)", "", true, func(value string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("worker enrollment token must be a nonempty single line")
		}
		return value, nil
	})
	if err != nil {
		return err
	}
	codex, err := m.setupWorkerCodex(ctx, prompt, l)
	if err != nil {
		return err
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
		WorkerID: workerID, Name: name,
		StateFile:  filepath.Join(l.Home, ".local/state/codex-worker/worker.db"),
		GatewayURL: gateway, TokenFile: tokenPath,
		AllowedWorkspaceRoots: roots, MaxQueuedTurns: 20,
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
	if err := execute(ctx, options{Action: "install", Component: "worker", Config: configPath, Version: version, AutoUpdate: true, WorkerServiceAccess: serviceAccess}); err != nil {
		return err
	}
	return m.finishWorkerSetup(ctx, l, prompt)
}

func setupWorkerDirectory(value, home, cwd string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("starting directory must be an existing accessible directory")
	}
	if value == "~" {
		value = home
	} else if strings.HasPrefix(value, "~/") {
		value = filepath.Join(home, value[2:])
	} else if !filepath.IsAbs(value) {
		value = filepath.Join(cwd, value)
	}
	roots, err := auth.CanonicalWorkspaceRoots([]string{value})
	if err != nil {
		return "", errors.New("starting directory must be an existing accessible directory")
	}
	return roots[0], nil
}

func setupGatewayURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	u, err := url.Parse(value)
	if err == nil && u.Scheme == "https" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(value, "#") {
		// Users often copy the admin-console address from their browser.
		if u.EscapedPath() == "/tgadmin" || u.EscapedPath() == "/tgadmin/" {
			u.Path, u.RawPath = "", ""
		}
		u, err = config.ParseHTTPSOrigin(u.String())
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
	return "", errors.New("gateway address must be a hostname, an HTTPS address (optionally ending in /tgadmin/), or a WSS URL without credentials, query, or fragment")
}
