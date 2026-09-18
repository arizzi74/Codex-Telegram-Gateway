package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	path, err := defaultConfigPath()
	if err != nil {
		return err
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 >= len(args) {
				return errors.New("--config requires a file")
			}
			path = args[i+1]
			args = append(args[:i], args[i+2:]...)
			i--
		} else if strings.HasPrefix(args[i], "--config=") {
			path = strings.TrimPrefix(args[i], "--config=")
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}
	if len(args) == 0 {
		args = []string{"run"}
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println(buildinfo.Version)
		return nil
	}
	if args[0] == "help" || args[0] == "--help" {
		fmt.Println("codex-worker [--config PATH] run|status|doctor|attach [SESSION]|config export|update prepare|update abort --token TOKEN")
		return nil
	}
	cfg, err := config.LoadWorker(path)
	if err != nil {
		return err
	}
	if args[0] == "config" {
		if len(args) != 2 || args[1] != "export" {
			return errors.New("usage: codex-worker [--config PATH] config export")
		}
		data, err := exportWorkerConfig(path)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(data))
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "update":
		action, token := "", ""
		if len(args) == 2 && args[1] == "prepare" {
			action = "prepare"
		} else if len(args) == 4 && args[1] == "abort" && args[2] == "--token" && args[3] != "" {
			action, token = "abort", args[3]
		} else {
			return errors.New("usage: codex-worker [--config PATH] update prepare|update abort --token TOKEN")
		}
		return requestUpdate(ctx, cfg, action, token, os.Stdout)
	case "run":
		if len(args) > 1 {
			return errors.New("unexpected run arguments")
		}
		store, err := worker.OpenStore(cfg.StateFile, cfg.WorkerID)
		if err != nil {
			return err
		}
		defer store.Close()
		agent, err := worker.NewAgent(cfg, store, logger)
		if err != nil {
			return err
		}
		return agent.Run(ctx)
	case "status", "attach":
		data, err := os.ReadFile(cfg.StateFile + ".status.json")
		if err != nil {
			return errors.New("worker status unavailable; start the worker first")
		}
		var status worker.Status
		if err = json.Unmarshal(data, &status); err != nil {
			return err
		}
		if status.WorkerID != cfg.WorkerID {
			return errors.New("status belongs to another worker")
		}
		if args[0] == "status" {
			if time.Since(status.UpdatedAt) > 20*time.Second {
				fmt.Fprintln(os.Stderr, "Worker status is stale; the worker may have stopped.")
			}
			return json.NewEncoder(os.Stdout).Encode(status)
		}
		if time.Since(status.UpdatedAt) > 20*time.Second {
			return errors.New("worker status is stale; inspect the worker before attaching")
		}
		var session *struct{ thread, runtime string }
		if len(args) > 2 {
			return errors.New("attach takes one session name or thread ID")
		}
		if len(args) == 2 {
			for _, s := range status.Sessions {
				if s.ID == args[1] || s.ThreadID == args[1] || s.Name == args[1] {
					if session != nil {
						return errors.New("session name is ambiguous; use the thread ID")
					}
					session = &struct{ thread, runtime string }{s.ThreadID, s.RuntimeID}
				}
			}
			if session == nil {
				return errors.New("session not found")
			}
		}
		var socket string
		for _, r := range status.Runtimes {
			if session != nil && r.ID != session.runtime {
				continue
			}
			if r.LocalSocket != "" {
				if socket != "" {
					return errors.New("multiple runtimes; specify a session")
				}
				socket = r.LocalSocket
			}
		}
		if socket == "" {
			return errors.New("runtime has no local attachment socket")
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		helper := filepath.Join(filepath.Dir(executable), "codex-local")
		cliArgs := []string{"attach", "--socket", socket}
		if session != nil {
			cliArgs = append(cliArgs, session.thread)
		}
		cmd := exec.CommandContext(ctx, helper, cliArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	case "doctor":
		for _, profile := range cfg.Runtimes {
			checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
			version, err := codexadapter.ExecutableVersion(checkCtx, profile.CodexBinary)
			done()
			if err != nil {
				return fmt.Errorf("profile %s: Codex version check failed", profile.ID)
			}
			fmt.Printf("%s: %s\n", profile.ID, version)
		}
		store, err := worker.OpenStore(cfg.StateFile, cfg.WorkerID)
		if err != nil {
			data, readErr := os.ReadFile(cfg.StateFile + ".status.json")
			var status worker.Status
			if readErr != nil || json.Unmarshal(data, &status) != nil || status.WorkerID != cfg.WorkerID || time.Since(status.UpdatedAt) > 20*time.Second {
				return err
			}
			fmt.Println("State database is held by the running worker.")
		} else {
			store.Close()
			fmt.Println("State database opens successfully.")
		}
		fmt.Println("Configuration, workspace roots, WSS URL and private token file are valid.")
		return nil
	default:
		return errors.New("unknown command; use --help")
	}
}

// exportWorkerConfig is the installer's normalization boundary. Loading first
// resolves relative fields against the source JSON file and validates every
// workspace and secret path before a replacement config is written elsewhere.
func exportWorkerConfig(path string) ([]byte, error) {
	cfg, err := config.LoadWorker(path)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// defaultConfigPath intentionally follows the same stable location as the
// per-user service installer. os.UserConfigDir differs on macOS, while the
// installer and launch agents consistently use the owning user's HOME.
func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve worker home directory: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("resolve worker home directory: home is empty")
	}
	return filepath.Join(home, ".config", "codex-worker", "config.json"), nil
}
