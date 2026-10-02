package releasemanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/iaia/telegramgw/internal/config"
)

// One atomic private journal preserves the response even if generating the
// token file, worker.json, or service fails. Never journal an enrollment code.
type workerSetupRecovery struct {
	Schema        int                 `json:"schema"`
	Config        config.WorkerConfig `json:"config"`
	Token         string              `json:"token"`
	ServiceAccess string              `json:"service_access"`
	Version       string              `json:"version"`
}

func workerSetupRecoveryDirectory(l *Layout) string {
	return filepath.Join(l.Home, ".local/state/codex-worker/setup")
}

func workerSetupRecoveryPath(l *Layout) string {
	return filepath.Join(workerSetupRecoveryDirectory(l), "enrolled-worker.json")
}

func prepareWorkerSetupRecovery(l *Layout) error {
	directory := workerSetupRecoveryDirectory(l)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return errors.New("could not prepare private worker setup recovery storage; check your home directory permissions")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("worker setup recovery directory must be a private regular directory with permissions 0700")
	}
	// Every newly created directory entry needs its parent synced before the
	// one-use request. Syncing only setup/ would not make setup/ itself durable.
	for path := directory; withinDirectory(l.Home, path); path = filepath.Dir(path) {
		if err := SyncDir(path); err != nil {
			return errors.New("worker setup recovery directories could not be prepared durably; check your home directory")
		}
		if path == filepath.Clean(l.Home) {
			break
		}
	}
	probe, err := os.CreateTemp(directory, ".write-check-")
	if err != nil {
		return errors.New("worker setup recovery storage is not writable; check your home directory permissions")
	}
	path := probe.Name()
	err = probe.Close()
	removeErr := os.Remove(path)
	if err != nil || removeErr != nil || SyncDir(directory) != nil {
		return errors.New("worker setup recovery storage could not be prepared durably; check your home directory")
	}
	return nil
}

func loadWorkerSetupRecovery(l *Layout) (*workerSetupRecovery, error) {
	path := workerSetupRecoveryPath(l)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	invalid := errors.New("private worker setup recovery is invalid; preserve its files and inspect the saved worker configuration before retrying")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64*1024 {
		return nil, invalid
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0077 != 0 {
		return nil, invalid
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, invalid
	}
	var recovery workerSetupRecovery
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&recovery); err != nil || decoder.Decode(new(any)) != io.EOF || recovery.Schema != 1 {
		return nil, invalid
	}
	gateway, err := url.Parse(recovery.Config.GatewayURL)
	if err != nil || gateway.Scheme != "wss" {
		return nil, invalid
	}
	origin, err := config.ParseHTTPSOrigin((&url.URL{Scheme: "https", Host: gateway.Host}).String())
	if err != nil || validateWorkerEnrollmentResponse(workerEnrollmentResponse{
		WorkerID: recovery.Config.WorkerID, Token: recovery.Token,
		GatewayURL: recovery.Config.GatewayURL, ServiceAccess: recovery.ServiceAccess,
	}, origin.String()) != nil {
		return nil, invalid
	}
	if _, err := normalizeWorkerSetupName(recovery.Config.Name); err != nil || recovery.Config.TokenFile != filepath.Join(workerSetupRecoveryDirectory(l), "worker.token") {
		return nil, invalid
	}
	if recovery.Config.StateFile != filepath.Join(l.Home, ".local/state/codex-worker/worker.db") ||
		len(recovery.Config.AllowedWorkspaceRoots) != 1 || len(recovery.Config.Runtimes) != 1 ||
		recovery.Config.MaxQueuedTurns != 20 || len(recovery.Config.RedactPatterns) != 0 {
		return nil, invalid
	}
	home, err := filepath.EvalSymlinks(l.Home)
	if err != nil || recovery.Config.AllowedWorkspaceRoots[0] != home {
		return nil, invalid
	}
	runtime := recovery.Config.Runtimes[0]
	if runtime.ID != "primary" || runtime.Name != "Primary Codex" || runtime.WorkingDirectory != home ||
		!runtime.Autostart || runtime.RestartPolicy != "on-failure" || !filepath.IsAbs(runtime.CodexBinary) {
		return nil, invalid
	}
	if recovery.Version != "" {
		if _, err := ParseVersion(recovery.Version); err != nil {
			return nil, invalid
		}
	}
	return &recovery, nil
}

func (m *Manager) resumeWorkerSetup(ctx context.Context, l *Layout, recovery *workerSetupRecovery, execute func(context.Context, options) error) error {
	if err := m.prepareWorkerSetupService(ctx, l); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Resuming the saved worker installation using its existing enrollment credentials.")
	if err := m.installRecoveredWorkerSetup(ctx, l, recovery, execute); err != nil {
		return err
	}
	return m.finishWorkerSetup(ctx, l, nil)
}

func (m *Manager) installRecoveredWorkerSetup(ctx context.Context, l *Layout, recovery *workerSetupRecovery, execute func(context.Context, options) error) error {
	directory := workerSetupRecoveryDirectory(l)
	if err := writePrivateSecret(filepath.Join(directory, "worker.token"), []byte(recovery.Token+"\n"), nil); err != nil {
		return errors.New("could not prepare the saved worker token; its credentials remain in private recovery storage, so rerun the installer after fixing home directory permissions")
	}
	prepared := filepath.Join(directory, "worker.json")
	if err := WriteJSON(prepared, recovery.Config); err != nil {
		return errors.New("could not prepare the saved worker configuration; its credentials remain in private recovery storage, so rerun the installer after fixing home directory permissions")
	}
	if _, err := config.LoadWorker(prepared); err != nil {
		return errors.New("saved worker setup configuration failed validation; preserve its private recovery files and check the worker home directory")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	serviceAccess := recovery.ServiceAccess
	if l.System != "linux" {
		serviceAccess = ""
	}
	if err := execute(ctx, options{Action: "install", Component: "worker", Config: prepared, Version: recovery.Version, AutoUpdate: true, WorkerServiceAccess: serviceAccess, WorkerSetupRecovery: true}); err != nil {
		fmt.Fprintln(m.Out, "The worker credentials are saved privately. Rerun this installer to resume installation with the same worker identity.")
		return err
	}
	if err := syncWorkerSetupInstallation(l); err != nil {
		return errors.New("worker installed, but its directories could not be saved durably; private recovery credentials are retained, so rerun the installer")
	}
	if err := os.RemoveAll(directory); err != nil {
		return errors.New("worker installed, but its private setup recovery files could not be removed; remove the setup directory from the worker's local state directory")
	}
	return SyncDir(filepath.Dir(directory))
}

func syncWorkerSetupInstallation(l *Layout) error {
	servicePath := l.Unit
	if l.System == "darwin" {
		servicePath = workerPlist(l)
	}
	seen := map[string]bool{}
	for _, file := range []string{l.Config, l.State, servicePath, l.Binary, l.Manager, l.Command} {
		for path := filepath.Dir(file); withinDirectory(l.Home, path); path = filepath.Dir(path) {
			if !seen[path] {
				seen[path] = true
				if err := SyncDir(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if path == filepath.Clean(l.Home) {
				break
			}
		}
	}
	return nil
}
