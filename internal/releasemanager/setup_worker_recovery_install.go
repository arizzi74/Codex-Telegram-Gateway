package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/config"
)

// Recovery is an internal setup operation, unavailable through installer flags.
// Its journal must match the generated private source and any installed config.
// An active matching worker is verified in place; its service and executables
// are never restarted or replaced as a side effect of setup recovery.
func (m *Manager) resumeEnrolledWorkerInstall(ctx context.Context, l *Layout, source string, packages map[string]string, release *Release) error {
	if l.Component != "worker" || source != filepath.Join(workerSetupRecoveryDirectory(l), "worker.json") {
		return errors.New("worker setup recovery must use its private saved configuration")
	}
	recovery, err := loadWorkerSetupRecovery(l)
	if err != nil || recovery == nil {
		return errors.New("worker setup recovery requires a valid private enrollment journal")
	}
	prepared, err := config.LoadWorker(source)
	if err != nil || !reflect.DeepEqual(prepared, recovery.Config) {
		return errors.New("worker setup recovery configuration does not match its enrollment journal")
	}
	preparedToken, readErr := os.ReadFile(prepared.TokenFile)
	if readErr != nil || !bytes.Equal(preparedToken, []byte(recovery.Token+"\n")) {
		return errors.New("worker setup recovery token does not match its enrollment journal")
	}
	servicePath := l.Unit
	if l.System == "darwin" {
		servicePath = workerPlist(l)
	}
	if _, err := os.Lstat(l.Config); errors.Is(err, os.ErrNotExist) {
		if FileExists(prepared.StateFile + ".status.json") {
			return errors.New("saved worker state may belong to a running worker; preserve the private setup recovery files and confirm that worker has stopped before resuming installation")
		}
		if _, serviceErr := os.Lstat(servicePath); !errors.Is(serviceErr, os.ErrNotExist) {
			return errors.New("worker service exists but its installed configuration is missing; preserve the private setup recovery files, confirm the service is stopped, remove the orphaned service file, and rerun the installer")
		}
		return m.FreshInstall(ctx, l, source, "", packages, release)
	} else if err != nil {
		return err
	}
	installed, err := config.LoadWorker(l.Config)
	if err != nil {
		return errors.New("existing worker configuration cannot be verified against saved enrollment; preserve the private recovery files and inspect the installation")
	}
	installedToken, tokenErr := os.ReadFile(installed.TokenFile)
	installed.TokenFile = prepared.TokenFile
	if readErr != nil || tokenErr != nil || !bytes.Equal(preparedToken, installedToken) || !reflect.DeepEqual(installed, prepared) {
		return errors.New("existing worker configuration differs from saved enrollment; recovery will not change this installation")
	}
	serviceInfo, serviceErr := os.Lstat(servicePath)
	if serviceErr != nil && !errors.Is(serviceErr, os.ErrNotExist) {
		return serviceErr
	}
	if serviceErr == nil && !serviceInfo.Mode().IsRegular() {
		return errors.New("worker setup recovery requires a regular service configuration")
	}
	running, err := m.workerSetupRecoveryServiceRunning(ctx, l)
	if err != nil {
		return err
	}
	if running {
		if serviceErr != nil || !regularNoSymlink(l.Binary) {
			return errors.New("active worker installation is incomplete; preserve its private recovery files and inspect the service")
		}
		return m.completeRunningWorkerSetupRecovery(ctx, l, release.Repo)
	}
	// Check for a worker outside the managed service before replacing binaries.
	if regularNoSymlink(l.Binary) {
		if running, err := m.workerRunning(ctx, l); err != nil {
			return err
		} else if running {
			return errors.New("worker started while recovering setup; rerun the installer to verify it in place")
		}
		version, err := m.command(ctx, l.Binary, "version")
		if err != nil {
			return err
		}
		comparison, err := CompareVersions(release.Tag, string(version))
		if err != nil {
			return err
		}
		if comparison < 0 {
			return errors.New("installed worker is newer than the saved setup release; preserve recovery files, start the existing worker service, and rerun the installer to verify it without downgrading")
		}
	} else if FileExists(installed.StateFile + ".status.json") {
		return errors.New("cannot verify that the partially installed worker has stopped; inspect its service before resuming setup")
	}
	if serviceErr != nil {
		if err := m.installService(ctx, l, packages["worker"]); err != nil {
			return err
		}
	}
	if l.System == "linux" {
		if err := m.Service(ctx, l, "enable"); err != nil {
			return err
		}
	}
	return m.ApplyUpdate(ctx, l, packages, release, true)
}

func (m *Manager) workerSetupRecoveryServiceRunning(ctx context.Context, l *Layout) (bool, error) {
	if l.System == "darwin" {
		result, err := m.Run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/com.iaia.codex-worker")
		if err != nil {
			return false, errors.New("could not inspect the worker launchd service; preserve private recovery files and retry from the logged-in Mac desktop session")
		}
		if result.ExitCode == 0 {
			return true, nil
		}
		if result.ExitCode == 113 { // launchctl: service not found in this domain.
			return false, nil
		}
		return false, errors.New("could not confirm that the worker launchd service is absent; inspect launchd before resuming setup")
	}
	data, err := m.command(ctx, "systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState")
	if err != nil {
		return false, err
	}
	values := serviceDetails(data)
	pid, err := strconv.Atoi(values["MainPID"])
	if err == nil && values["ActiveState"] == "active" && pid > 0 {
		return true, nil
	}
	if err != nil || (values["ActiveState"] != "inactive" && values["ActiveState"] != "failed") || pid != 0 {
		return false, &BusyError{Reason: "worker service is changing state; rerun setup when it settles"}
	}
	return false, nil
}

func (m *Manager) completeRunningWorkerSetupRecovery(ctx context.Context, l *Layout, repo string) error {
	version, err := m.command(ctx, l.Binary, "version")
	if err != nil {
		return err
	}
	if _, err := ParseVersion(string(version)); err != nil {
		return err
	}
	if err := m.WorkerReady(ctx, l, m.Now().Add(-15*time.Second), string(version)); err != nil {
		return err
	}
	if err := m.InstallManager(l, m.Self); err != nil {
		return err
	}
	if err := SaveSettings(l, repo, "v"+strings.TrimPrefix(strings.TrimSpace(string(version)), "v")); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Verified the existing worker installation without restarting its service.")
	return nil
}
