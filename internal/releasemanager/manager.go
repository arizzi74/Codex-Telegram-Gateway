package releasemanager

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/buildinfo"
)

func New(out io.Writer) *Manager {
	if out == nil {
		out = io.Discard
	}
	self, _ := os.Executable()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &Manager{Run: RunCommand, CodexRun: RunCodexUpdate, Out: out, HTTP: &http.Client{Transport: transport, Timeout: 5 * time.Minute}, Self: self, Now: time.Now, ReadyTimeout: 45 * time.Second, PollInterval: time.Second}
}

// RunCommand intentionally discards stderr: service commands can include private
// configuration and must not leak their output through updater logs.
func RunCommand(ctx context.Context, args ...string) (CommandResult, error) {
	if len(args) == 0 {
		return CommandResult{}, errors.New("missing command")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	output, err := command.Output()
	if ctx.Err() != nil {
		return CommandResult{}, fmt.Errorf("required command timed out or was cancelled: %s", filepath.Base(args[0]))
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return CommandResult{Output: output, ExitCode: exit.ExitCode()}, nil
	}
	if err != nil {
		return CommandResult{}, fmt.Errorf("could not execute required command: %s", filepath.Base(args[0]))
	}
	return CommandResult{Output: output}, nil
}

func (m *Manager) command(ctx context.Context, args ...string) ([]byte, error) {
	result, err := m.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("a required command failed: %s", filepath.Base(args[0]))
	}
	return result.Output, nil
}

type options struct {
	Action, Component, Setting, Config, Environment, Repo, Version string
	AutoUpdate, Check                                              bool
}

const usage = `Install and update Codex Telegram Gateway from verified GitHub Releases.

Usage:
  codex-telegramgw setup [gateway|worker]
  codex-telegramgw finish gateway
  codex-telegramgw install gateway --config PATH --secrets-env PATH [--auto-update]
  codex-telegramgw install worker --config PATH [--auto-update]
  codex-telegramgw adopt gateway|worker [--auto-update]
  codex-telegramgw update gateway|worker [--check]
  codex-telegramgw update codex [--check]
  codex-telegramgw auto-update enable|disable gateway|worker
  codex-telegramgw version

Install and update accept --version vMAJOR.MINOR.PATCH and --repo OWNER/REPOSITORY.
Run gateway administration with sudo and worker administration as its user.
Setup defaults to gateway under sudo/root and worker otherwise.
Gateway setup reuses ./gateway.json and ./secrets.env or prompts for settings.
It guides HTTPS, Telegram activation, and administrator enrollment.
Worker setup can install Codex and guide sign-in when needed.
Worker setup reuses ./worker.json or prompts for enrollment and workspace details.
Setup enables daily updates and adopts existing services without restarting them.
Worker updates also check the stable Codex runtime once per day and apply it when idle.
Runtime updates support the official standalone installation and preserve active turns.
`

func setupComponent(args []string, uid int) (string, error) {
	if len(args) == 0 {
		if uid == 0 {
			return "gateway", nil
		}
		return "worker", nil
	}
	if len(args) == 1 && (args[0] == "gateway" || args[0] == "worker") {
		return args[0], nil
	}
	return "", errors.New("usage: codex-telegramgw setup [gateway|worker]")
}

func parseOptions(args []string) (options, error) {
	var result options
	if len(args) < 2 {
		return result, errors.New("an action and component are required; use --help")
	}
	result.Action = args[0]
	if result.Action == "auto-update" {
		if len(args) != 3 || (args[1] != "enable" && args[1] != "disable") {
			return result, errors.New("usage: codex-telegramgw auto-update enable|disable gateway|worker")
		}
		result.Setting = args[1]
		result.Component = args[2]
	} else {
		result.Component = args[1]
		if result.Action != "install" && result.Action != "adopt" && result.Action != "update" {
			return result, errors.New("unknown installer action")
		}
		flags := flag.NewFlagSet(result.Action, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&result.Repo, "repo", "", "GitHub repository")
		if result.Action != "adopt" {
			flags.StringVar(&result.Version, "version", "", "stable release tag")
		}
		if result.Action == "install" {
			flags.StringVar(&result.Config, "config", "", "configuration file")
			flags.StringVar(&result.Environment, "secrets-env", "", "gateway environment file")
		}
		if result.Action == "update" {
			flags.BoolVar(&result.Check, "check", false, "check without installing")
		} else {
			flags.BoolVar(&result.AutoUpdate, "auto-update", false, "enable daily updates")
		}
		if err := flags.Parse(args[2:]); err != nil {
			return result, errors.New("invalid installer arguments; use --help")
		}
		if flags.NArg() != 0 {
			return result, errors.New("unexpected installer argument")
		}
		if result.Action == "install" && result.Config == "" {
			return result, errors.New("install requires --config PATH")
		}
	}
	if result.Component == "codex" {
		if result.Action != "update" || result.Repo != "" || result.Version != "" {
			return result, errors.New("use update codex [--check]; runtime updates follow the official stable channel")
		}
		return result, nil
	}
	if result.Component != "gateway" && result.Component != "worker" {
		return result, errors.New("component must be gateway or worker")
	}
	if result.Repo != "" {
		if err := ValidateRepo(result.Repo); err != nil {
			return result, err
		}
	}
	if result.Version != "" {
		if _, err := ParseVersion(result.Version); err != nil {
			return result, err
		}
	}
	return result, nil
}

func Execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")) {
		fmt.Fprint(out, usage)
		return 0
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(out, buildinfo.Version)
		return 0
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Fprint(out, usage)
			return 0
		}
	}
	var err error
	if args[0] == "setup" {
		var component string
		component, err = setupComponent(args[1:], os.Geteuid())
		if err == nil {
			if component == "gateway" {
				err = New(out).SetupGateway(ctx)
			} else {
				err = New(out).SetupWorker(ctx)
			}
		}
	} else if args[0] == "finish" {
		if len(args) != 2 || args[1] != "gateway" {
			err = errors.New("usage: codex-telegramgw finish gateway")
		} else {
			err = New(out).FinishGateway(ctx)
		}
	} else {
		var opts options
		opts, err = parseOptions(args)
		if err == nil {
			if opts.Action == "install" && opts.Version == "" {
				opts.Version = os.Getenv("CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE")
			}
			if opts.Version != "" {
				_, err = ParseVersion(opts.Version)
			}
		}
		if err == nil {
			err = New(out).execute(ctx, opts)
		}
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(errOut, err)
	var busy *BusyError
	if errors.As(err, &busy) {
		return 75
	}
	return 1
}

func (m *Manager) execute(ctx context.Context, opts options) (retErr error) {
	component := opts.Component
	if component == "codex" {
		component = "worker"
	}
	l, err := NewLayout(component)
	if err != nil {
		return err
	}
	if opts.Action == "adopt" && l.Component == "gateway" {
		if err = m.SecureGatewayAdoption(l); err != nil {
			return err
		}
	}
	if err = l.RequireUser(); err != nil {
		return err
	}
	unlock, err := Lock(l.Lock)
	if err != nil {
		return err
	}
	defer unlock()
	if opts.Component == "codex" {
		if _, err := SavedSettings(l); err != nil {
			return err
		}
		return m.UpdateCodexRuntime(ctx, l, opts.Check)
	}
	if opts.Action == "auto-update" {
		if _, err := SavedSettings(l); err != nil {
			return err
		}
		return m.AutoUpdate(ctx, l, opts.Setting == "enable")
	}
	if opts.Action == "adopt" {
		if !FileExists(l.Config) || !FileExists(l.Binary) {
			return errors.New("no existing installation found in the standard paths")
		}
		installed, e := m.command(ctx, l.Binary, "version")
		if e != nil {
			return e
		}
		if _, err = ParseVersion(string(installed)); err != nil {
			return err
		}
		if l.Component == "gateway" {
			cfg, e := ReadJSON(l.Config)
			if e != nil {
				return e
			}
			_, hasPostgres := cfg["database_url_env"]
			if hasPostgres || cfg["database_path"] == nil {
				return errors.New("only SQLite gateway installations can be adopted")
			}
		}
		previous := map[string]any{}
		if FileExists(l.State) {
			previous, err = SavedSettings(l)
			if err != nil {
				return err
			}
		}
		repo := opts.Repo
		if repo == "" {
			repo = DefaultRepo
			if saved, ok := previous["repo"].(string); ok && saved != "" {
				repo = saved
			}
		}
		if err = ValidateRepo(repo); err != nil {
			return err
		}
		if err = m.InstallManager(l, m.Self); err != nil {
			return err
		}
		state := map[string]any{
			"schema": 1, "component": l.Component, "repo": repo,
			"version":    "v" + strings.TrimPrefix(strings.TrimSpace(string(installed)), "v"),
			"updated_at": m.Now().UTC().Format(time.RFC3339Nano),
		}
		if pending, _ := previous["pending"].(bool); pending {
			state["pending"] = true
		}
		if err = WriteJSON(l.State, state); err != nil {
			return err
		}
		if opts.AutoUpdate {
			if err = m.AutoUpdate(ctx, l, true); err != nil {
				return err
			}
		}
		fmt.Fprintf(m.Out, "Adopted existing %s %s; service was not restarted.\n", l.Component, strings.TrimSpace(string(installed)))
		return nil
	}
	settings := map[string]any{}
	if opts.Action == "update" {
		settings, err = SavedSettings(l)
		if err != nil {
			return err
		}
		if l.Component == "worker" {
			// A failed or busy gateway-project update must not starve the daily
			// Codex check. Both maintenance paths remain under the worker lock.
			defer func() {
				if ctx.Err() != nil {
					return
				}
				runtimeErr := m.UpdateCodexRuntime(ctx, l, opts.Check)
				if retErr == nil {
					retErr = runtimeErr
				} else if runtimeErr != nil {
					var workerBusy, runtimeBusy *BusyError
					if errors.As(retErr, &workerBusy) && !errors.As(runtimeErr, &runtimeBusy) {
						retErr = fmt.Errorf("%v; %w", retErr, runtimeErr)
					} else {
						retErr = fmt.Errorf("%w; %v", retErr, runtimeErr)
					}
				}
			}()
		}
	}
	repo := opts.Repo
	if repo == "" {
		repo, _ = settings["repo"].(string)
	}
	if repo == "" {
		repo = DefaultRepo
	}
	release, err := m.FetchRelease(ctx, repo, opts.Version)
	if err != nil {
		return err
	}
	if opts.Action == "update" {
		data, e := m.command(ctx, l.Binary, "version")
		if e != nil {
			return e
		}
		installed := strings.TrimSpace(string(data))
		comparison, e := CompareVersions(release.Tag, installed)
		if e != nil {
			return e
		}
		if comparison < 0 {
			return errors.New("downgrades are not supported")
		}
		if comparison == 0 {
			if pending, _ := settings["pending"].(bool); pending {
				if opts.Check {
					return errors.New("installed version is awaiting a successful readiness check; inspect service logs")
				}
				if l.Component == "gateway" {
					err = m.GatewayReady(ctx, l)
				} else {
					err = m.WorkerReady(ctx, l, m.Now().Add(-15*time.Second), release.Tag)
				}
				if err != nil {
					return err
				}
				if err = SaveSettings(l, repo, release.Tag); err != nil {
					return err
				}
			}
			// An old updater may have installed this native executable at its
			// legacy .py path. Finish that migration without restarting services.
			if !opts.Check && filepath.Clean(m.Self) == filepath.Clean(l.LegacyManager) {
				if err = m.InstallManager(l, m.Self); err != nil {
					return err
				}
			}
			fmt.Fprintf(m.Out, "%s is already at %s.\n", l.Component, release.Tag)
			return nil
		}
		if opts.Check {
			fmt.Fprintf(m.Out, "Update available for %s: %s -> %s\n", l.Component, installed, release.Tag)
			return nil
		}
	}
	stage, err := os.MkdirTemp("", "codex-telegramgw-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	componentPackage, err := m.Package(ctx, release, l, "codex-"+l.Component, stage)
	if err != nil {
		return err
	}
	packages := map[string]string{l.Component: componentPackage}
	if l.Component == "worker" {
		packages["local"], err = m.Package(ctx, release, l, "codex-local", stage)
		if err != nil {
			return err
		}
	}
	if opts.Action == "install" {
		source, e := filepath.Abs(opts.Config)
		if e != nil {
			return e
		}
		environment := opts.Environment
		if environment != "" {
			environment, e = filepath.Abs(environment)
			if e != nil {
				return e
			}
		}
		if err = m.FreshInstall(ctx, l, source, environment, packages, release); err != nil {
			return err
		}
		if opts.AutoUpdate {
			return m.AutoUpdate(ctx, l, true)
		}
		return nil
	}
	return m.ApplyUpdate(ctx, l, packages, release, false)
}
