package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
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
	unlock, err := Lock(filepath.Join(filepath.Dir(l.Lock), "setup.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if recovery, err := loadWorkerSetupRecovery(l); err != nil {
		return err
	} else if recovery != nil {
		return m.resumeWorkerSetup(ctx, l, recovery, execute)
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
	if err := m.prepareWorkerSetupService(ctx, l); err != nil {
		return err
	}
	servicePath := l.Unit
	if l.System == "darwin" {
		servicePath = workerPlist(l)
	}
	if _, err := os.Lstat(servicePath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("existing worker service has no standard configuration; inspect the installation before enrolling a new worker")
	}
	if FileExists(filepath.Join(l.Home, ".local/state/codex-worker/worker.db.status.json")) {
		return errors.New("existing worker state may belong to a running worker; inspect the installation before enrolling a new worker")
	}
	prompt, err := openPrompt()
	if err != nil {
		return errors.New("worker setup needs a terminal or a private worker.json in the current directory; run this command in an interactive terminal")
	}
	defer prompt.Close()
	fmt.Fprintln(m.Out, "Set up this machine as a worker. Enter its name and the one-use enrollment URL from your gateway admin console. Daily automatic updates will be enabled.")
	name, err := askSetupValue(ctx, prompt, m.Out, "Worker name", "my-worker", false, normalizeWorkerSetupName)
	if err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Enrollment URLs last 10 minutes and can be used once. The pasted URL is hidden.")
	enrollmentValue, err := askSetupValue(ctx, prompt, m.Out, "Enrollment URL (hidden)", "", true, normalizeWorkerEnrollmentURL)
	if err != nil {
		return err
	}
	roots, err := auth.CanonicalWorkspaceRoots([]string{l.Home})
	if err != nil {
		return errors.New("worker home must be an existing directory")
	}
	fmt.Fprintf(m.Out, "The worker starts in your home directory (%s) and can use all its subfolders.\n", l.Home)
	codex, err := m.setupWorkerCodex(ctx, prompt, l)
	if err != nil {
		return err
	}
	// Finish service and Codex prerequisites before consuming the one-use URL.
	// The private journal remains outside temporary directories if installation
	// later fails, so rerunning setup never needs to enroll this worker again.
	if err := prepareWorkerSetupRecovery(l); err != nil {
		return err
	}
	var enrollmentResponse workerEnrollmentResponse
	for {
		enrollment, _ := parseWorkerEnrollmentURL(enrollmentValue)
		enrollmentResponse, err = m.redeemWorkerEnrollment(ctx, enrollment, name, l)
		if err == nil {
			break
		}
		var rejected *workerEnrollmentRejected
		if !errors.As(err, &rejected) {
			return err
		}
		fmt.Fprintln(m.Out, err)
		enrollmentValue, err = askSetupValue(ctx, prompt, m.Out, "Fresh enrollment URL (hidden)", "", true, func(value string) (string, error) {
			fresh, err := parseWorkerEnrollmentURL(value)
			if err != nil {
				return "", err
			}
			if fresh == enrollment {
				return "", errors.New("create a fresh enrollment URL in the gateway admin console; the previous URL cannot be reused")
			}
			return strings.TrimSpace(value), nil
		})
		if err != nil {
			return err
		}
	}
	recovery := &workerSetupRecovery{
		Schema: 1, Token: enrollmentResponse.Token, ServiceAccess: enrollmentResponse.ServiceAccess, Version: version,
		Config: config.WorkerConfig{
			WorkerID: enrollmentResponse.WorkerID, Name: name,
			StateFile:  filepath.Join(l.Home, ".local/state/codex-worker/worker.db"),
			GatewayURL: enrollmentResponse.GatewayURL, TokenFile: filepath.Join(workerSetupRecoveryDirectory(l), "worker.token"),
			AllowedWorkspaceRoots: roots, MaxQueuedTurns: 20,
			Runtimes: []config.RuntimeProfile{{
				ID: "primary", Name: "Primary Codex", CodexBinary: codex,
				WorkingDirectory: roots[0], Autostart: true, RestartPolicy: "on-failure",
			}},
		},
	}
	if err := WriteJSON(workerSetupRecoveryPath(l), recovery); err != nil {
		return errors.New("could not save the enrolled worker credentials; check the gateway admin worker list before creating a fresh URL and rerunning setup")
	}
	if err := m.installRecoveredWorkerSetup(ctx, l, recovery, execute); err != nil {
		return err
	}
	return m.finishWorkerSetup(ctx, l, prompt)
}

func normalizeWorkerSetupName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if err := protocol.ValidateWorkerEnrollmentName(value); err != nil {
		return "", err
	}
	return value, nil
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
		switch u.EscapedPath() {
		case "/tgw", "/tgw/", "/tgw/admin", "/tgw/admin/", "/tgw/webui", "/tgw/webui/", "/tgadmin", "/tgadmin/":
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
	return "", errors.New("gateway address must be a hostname, an HTTPS address (optionally ending in /tgw/admin/), or a WSS URL without credentials, query, or fragment")
}
