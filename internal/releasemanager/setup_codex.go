package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const codexSetupInstallerURL = "https://chatgpt.com/codex/install.sh"

func (m *Manager) setupWorkerCodex(ctx context.Context, prompt workerSetupPrompt, l *Layout) (string, error) {
	binary, err := exec.LookPath("codex")
	if err != nil {
		// A prior installation may have finished before the login shell picked
		// up ~/.local/bin; don't download or replace that installation again.
		binary, err = exec.LookPath(filepath.Join(l.Bin, "codex"))
	}
	if err != nil {
		fmt.Fprintln(m.Out, "Codex is not installed. Setup can install OpenAI's standalone Codex CLI without Node.js, Python, or a compiler.")
		answer, err := askSetupValue(ctx, prompt, m.Out, "Install Codex now? (yes/no)", "yes", false, setupYesNo)
		if err != nil {
			return "", err
		}
		if answer != "yes" {
			return "", errors.New("install Codex using https://chatgpt.com/codex/install.sh, then rerun this installer")
		}
		if err := m.installSetupCodex(ctx, l); err != nil {
			return "", err
		}
		binary, err = exec.LookPath(filepath.Join(l.Bin, "codex"))
		if err != nil {
			return "", errors.New("Codex installation did not create its executable; rerun setup after checking the installer output")
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	for {
		status, statusErr := m.Run(ctx, binary, "login", "status")
		if statusErr == nil && status.ExitCode == 0 {
			fmt.Fprintln(m.Out, "Codex is installed and signed in.")
			return binary, nil
		}
		fmt.Fprintln(m.Out, "Sign in to Codex. Device login works over SSH: open the displayed link on your own computer and enter the code. Enable device-code login in ChatGPT security settings if requested.")
		method, err := askSetupValue(ctx, prompt, m.Out, "Codex login method (device/browser/later)", "device", false, func(value string) (string, error) {
			value = strings.ToLower(strings.TrimSpace(value))
			switch value {
			case "device", "browser", "later":
				return value, nil
			}
			return "", errors.New("Choose device, browser, or later.")
		})
		if err != nil {
			return "", err
		}
		if method == "later" {
			fmt.Fprintf(m.Out, "Run %s login (or login --with-api-key), then rerun the same installer.\n", setupCommandQuote(binary))
			return "", errors.New("worker setup paused until Codex login is complete")
		}
		args := []string{binary, "login"}
		if method == "device" {
			args = append(args, "--device-auth")
		}
		if err := m.runSetupInteractive(ctx, args...); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			fmt.Fprintln(m.Out, "Codex login did not complete. Try again, choose browser login, or finish login separately and rerun setup.")
		}
	}
}

func (m *Manager) installSetupCodex(ctx context.Context, l *Layout) error {
	downloadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, codexSetupInstallerURL, nil)
	if err != nil {
		return errors.New("could not construct the official Codex installer request")
	}
	client := *m.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil ||
			(req.URL.Hostname() != "chatgpt.com" && req.URL.Hostname() != "releases.openai.com") ||
			(req.URL.Port() != "" && req.URL.Port() != "443") {
			return errors.New("unexpected Codex installer redirect")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("could not download the official Codex installer; check internet access and rerun setup")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("the official Codex installer download was unsuccessful; rerun setup to retry")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) == 0 || len(data) > 1024*1024 || !strings.HasPrefix(string(data), "#!/bin/sh\n") || response.ContentLength > int64(len(data)) {
		return errors.New("the official Codex installer response was incomplete or invalid")
	}
	stage, err := os.MkdirTemp("", "codex-telegramgw-codex-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	script := filepath.Join(stage, "install.sh")
	if err := os.WriteFile(script, data, 0600); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Installing the latest stable Codex CLI using OpenAI's installer; its downloads are checksum-verified.")
	args := []string{"env"}
	for _, key := range []string{"CODEX_MANAGED_BY_NPM", "CODEX_MANAGED_BY_BUN", "CODEX_MANAGED_BY_PNPM", "CODEX_MANAGED_BY_VITE_PLUS", "CODEX_INSTALL_IF_LATEST", "CODEX_UPDATE_FROM_RELEASE"} {
		args = append(args, "-u", key)
	}
	args = append(args, "CODEX_INSTALL_DIR="+l.Bin, "CODEX_RELEASE=latest", "CODEX_NON_INTERACTIVE=1", "sh", script)
	// The official installer is noninteractive. Use a separate process group
	// so cancellation also stops its downloader and archive extraction before
	// removing the staging directory. Login and sudo still use the TTY runner.
	run := m.CodexRun
	if run == nil {
		run = RunCodexUpdate
	}
	result, err := run(ctx, args...)
	if err != nil || result.ExitCode != 0 {
		return errors.New("Codex installation did not complete; check that curl, tar, gzip and a SHA-256 utility are available, then rerun setup")
	}
	return nil
}
