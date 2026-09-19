package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Validation errors describe the expected input, never the entered value:
// callers use this helper for both public settings and hidden credentials.
func askSetupValue(ctx context.Context, prompt workerSetupPrompt, out io.Writer, label, fallback string, secret bool, normalize func(string) (string, error)) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		value, err := prompt.Ask(ctx, label, fallback, secret)
		if err != nil {
			return "", err
		}
		value, err = normalize(value)
		if err == nil {
			return value, nil
		}
		fmt.Fprintf(out, "%s Please try again.\n", err)
	}
}

func setupYesNo(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "yes", "y":
		return "yes", nil
	case "no", "n":
		return "no", nil
	default:
		return "", errors.New("Enter yes or no.")
	}
}

// Authentication and sudo must read the controlling terminal, never the pipe
// containing the bootstrap script. These commands run only in guided setup.
func (m *Manager) runSetupInteractive(ctx context.Context, args ...string) error {
	if len(args) == 0 {
		return errors.New("missing setup command")
	}
	if m.SetupRun != nil {
		result, err := m.SetupRun(ctx, args...)
		if err != nil || result.ExitCode != 0 {
			return fmt.Errorf("setup command did not complete: %s", filepath.Base(args[0]))
		}
		return nil
	}
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("this setup step needs an interactive terminal")
	}
	defer terminal.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdin, command.Stdout, command.Stderr = terminal, terminal, terminal
	command.WaitDelay = 5 * time.Second
	if err := command.Run(); err != nil {
		return fmt.Errorf("setup command did not complete: %s", filepath.Base(args[0]))
	}
	return nil
}
