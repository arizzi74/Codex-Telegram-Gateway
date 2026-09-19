package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Check the session's service manager before collecting enrollment secrets or
// installing files. A root shell changed to another user often has no user bus.
func (m *Manager) prepareWorkerSetupService(ctx context.Context, l *Layout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if l.System == "linux" {
		if _, err := m.command(checkCtx, "systemctl", "--user", "show-environment"); err != nil {
			return errors.New("worker setup needs a running systemd user session; sign in directly as this user (SSH is supported), without sudo or su, and rerun the installer; systems without systemd are not supported")
		}
		return nil
	}
	if _, err := m.command(checkCtx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())); err != nil {
		return errors.New("worker setup on macOS needs a logged-in desktop session; sign in to the Mac desktop as this user, open Terminal, and rerun the installer")
	}
	return nil
}

func (m *Manager) finishWorkerSetup(ctx context.Context, l *Layout, prompt workerSetupPrompt) error {
	defer m.printWorkerSetupCommands(l)
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.System == "darwin" {
		fmt.Fprintln(m.Out, "The worker starts automatically when you sign in to your Mac desktop and runs while that account remains logged in.")
		return nil
	}
	uid := strconv.Itoa(os.Getuid())
	if m.workerSetupLingerEnabled(ctx, uid) {
		fmt.Fprintln(m.Out, "The worker will keep running after logout and start automatically at boot.")
		return nil
	}
	// Some distributions allow users to enable lingering for themselves. Avoid
	// invoking a hidden PolicyKit password prompt from the curl pipe.
	_, enableErr := m.command(ctx, "loginctl", "--no-ask-password", "enable-linger", uid)
	if enableErr == nil && m.workerSetupLingerEnabled(ctx, uid) {
		fmt.Fprintln(m.Out, "Enabled background service startup: the worker will keep running after logout and start automatically at boot.")
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "To keep the worker online after logout and start it at boot, this system requires administrator access.")
	for {
		answer, err := prompt.Ask(ctx, "Enable background startup using sudo? (yes/no)", "yes", false)
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "yes", "y":
			if err := m.runSetupInteractive(ctx, "sudo", "loginctl", "enable-linger", uid); err == nil && m.workerSetupLingerEnabled(ctx, uid) {
				fmt.Fprintln(m.Out, "The worker will keep running after logout and start automatically at boot.")
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintln(m.Out, "Background startup could not be enabled. The worker is installed, but may stop when you log out.")
			fmt.Fprintf(m.Out, "An administrator can enable it with: sudo loginctl enable-linger %s\n", uid)
			return nil
		case "no", "n":
			fmt.Fprintln(m.Out, "Background startup was not enabled. The worker may stop when you log out.")
			fmt.Fprintf(m.Out, "To enable it later: sudo loginctl enable-linger %s\n", uid)
			return nil
		default:
			fmt.Fprintln(m.Out, "Enter yes or no.")
		}
	}
}

func (m *Manager) workerSetupLingerEnabled(ctx context.Context, uid string) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := m.command(checkCtx, "loginctl", "show-user", uid, "--property=Linger", "--value")
	return err == nil && strings.TrimSpace(string(output)) == "yes"
}

func (m *Manager) printWorkerSetupCommands(l *Layout) {
	fmt.Fprintf(m.Out, "\nWorker commands (available immediately):\n  %s status\n  %s doctor\n  %s attach --latest\n", setupCommandQuote(l.Binary), setupCommandQuote(l.Binary), setupCommandQuote(l.Binary))
	inPath := false
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory != "" && filepath.Clean(directory) == filepath.Clean(l.Bin) {
			inPath = true
			break
		}
	}
	if !inPath {
		fmt.Fprintln(m.Out, "To use codex-worker, codex-local, and codex-telegramgw by name in this terminal:")
		fmt.Fprintln(m.Out, `  export PATH="$HOME/.local/bin:$PATH"`)
		fmt.Fprintln(m.Out, "Add that line to your shell startup file to keep it in future terminals.")
	}
	fmt.Fprintln(m.Out, "Open your Telegram bot, send /tgstart, then use /tgsessions to select a session or /tgnew to create one.")
}

// Paths are printed as copyable shell commands, including homes containing a
// single quote or command substitution characters.
func setupCommandQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
