package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	workerServiceRestricted = "restricted"
	workerServiceFull       = "full"
)

func validateWorkerServiceAccess(access string) error {
	if access != "" && access != workerServiceRestricted && access != workerServiceFull {
		return errors.New("worker service access must be restricted or full")
	}
	return nil
}

func configureWorkerServiceAccess(l *Layout, opts options) error {
	if err := validateWorkerServiceAccess(opts.WorkerServiceAccess); err != nil {
		return err
	}
	if opts.WorkerServiceAccess == "" {
		return nil
	}
	if opts.Action != "install" || l.Component != "worker" || l.System != "linux" {
		return errors.New("--service-access is supported only when installing a Linux worker; existing service settings are preserved")
	}
	l.WorkerServiceAccess = opts.WorkerServiceAccess
	return nil
}

func (m *Manager) setupWorkerServiceAccess(ctx context.Context, l *Layout, prompt workerSetupPrompt) (string, error) {
	if l.System != "linux" {
		fmt.Fprintln(m.Out, "On macOS the worker runs as your account using launchd. The Linux service restriction profiles are unavailable; account permissions and macOS privacy controls still apply.")
		return "", nil
	}
	fmt.Fprintln(m.Out, "Choose the Linux worker service access. Restricted services use private temporary files and user isolation, and block gaining privileges. For example, sudo apt update from a worker session will be blocked even if this account can normally use sudo.")
	fmt.Fprintln(m.Out, "Full system access removes these service restrictions. The worker still runs as your account: normal file permissions and your existing sudo policy apply. This does not grant sudo access or change Codex session permissions or allowed working directories.")
	answer, err := askSetupValue(ctx, prompt, m.Out, "Restrict this worker service? (yes = restricted, no = full system access)", "yes", false, setupYesNo)
	if err != nil {
		return "", err
	}
	if answer == "no" {
		return workerServiceFull, nil
	}
	return workerServiceRestricted, nil
}

// Access is recorded in the installed unit. Adoption and release updates leave
// that unit (and any administrator drop-ins) unchanged, retaining the choice.
func renderWorkerServiceAccess(data []byte, access string) ([]byte, error) {
	if err := validateWorkerServiceAccess(access); err != nil {
		return nil, err
	}
	if access == "" {
		return data, nil
	}
	value := "yes"
	if access == workerServiceFull {
		value = "no"
	}
	var result []string
	foundService, inService := false, false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inService = trimmed == "[Service]"
			if inService {
				foundService = true
				result = append(result, line, "PrivateTmp="+value, "PrivateUsers="+value, "NoNewPrivileges="+value)
				continue
			}
		}
		if inService {
			key, _, _ := strings.Cut(trimmed, "=")
			switch strings.TrimSpace(key) {
			case "PrivateTmp", "PrivateUsers", "NoNewPrivileges":
				continue
			}
		}
		result = append(result, line)
	}
	if !foundService {
		return nil, errors.New("worker service template has no Service section")
	}
	return []byte(strings.Join(result, "\n")), nil
}
