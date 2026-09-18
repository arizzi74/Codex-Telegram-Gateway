package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// codexRuntimePlan groups profiles that share one native Codex installation.
// Profiles records whether each profile starts automatically with the worker.
type codexRuntimePlan struct {
	Distribution  *codexDistribution
	TargetVersion string
	NeedsInstall  bool
	Profiles      map[string]bool
}

func (m *Manager) currentCodexDistribution(ctx context.Context, expected *codexDistribution) (*codexDistribution, error) {
	if expected == nil {
		return nil, errors.New("Codex runtime update has no installation identity")
	}
	current, err := m.inspectCodexDistribution(ctx, expected.Binary)
	if err != nil {
		return nil, err
	}
	if current.Binary != expected.Binary || current.Home != expected.Home || current.InstallDir != expected.InstallDir {
		return nil, errors.New("Codex installation changed while preparing its update; retry later")
	}
	return current, nil
}

// applyCodexRuntimeUpdates must run under the worker installation update lock.
// The live worker lease, rather than a status snapshot, authorizes its shutdown.
// recordRestart durably records recovery intent before any service is stopped.
func (m *Manager) applyCodexRuntimeUpdates(ctx context.Context, l *Layout, plans []codexRuntimePlan, resumeStopped bool, recordRestart func() error) (retErr error) {
	if len(plans) == 0 && !resumeStopped {
		return nil
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return err
	}
	restart := running || resumeStopped
	var lease *workerLease
	stopAttempted, stopped, restarted := false, false, false
	defer func() {
		if retErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if lease != nil && !stopped {
			if err := m.workerAbort(cleanupCtx, l, lease.Token); err != nil && !stopAttempted {
				retErr = errors.Join(retErr, errors.New("worker reservation could not be released; it will expire automatically"))
			}
		}
		if restart && !restarted && (stopAttempted || (!running && resumeStopped)) {
			if err := m.Service(cleanupCtx, l, "start"); err != nil {
				retErr = errors.Join(retErr, errors.New("Codex runtime update could not restore the worker service; inspect its service logs"))
			}
		}
	}()
	for _, plan := range plans {
		if _, err := ParseVersion(plan.TargetVersion); err != nil {
			return errors.New("Codex runtime update target is invalid")
		}
		if _, err := m.currentCodexDistribution(ctx, plan.Distribution); err != nil {
			return err
		}
	}
	if running {
		lease, err = m.workerPrepare(ctx, l)
		if err != nil {
			return err
		}
	}
	if restart {
		if recordRestart == nil {
			return errors.New("Codex runtime update cannot record worker restart recovery")
		}
		if err := recordRestart(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease != nil && lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
		return &BusyError{Reason: "worker update reservation expired; retry later"}
	}
	if running {
		stopAttempted = true
		if err := m.Service(ctx, l, "stop"); err != nil {
			return err
		}
	}
	// A successful service command alone is insufficient: an unmanaged worker
	// or a service transition must not race an update of its shared runtime.
	if stillRunning, err := m.workerRunning(ctx, l); err != nil {
		return err
	} else if stillRunning {
		return errors.New("Codex runtime update could not confirm that the worker stopped")
	}
	stopped = true
	expectedVersions := make(map[string]string)
	for _, plan := range plans {
		current, err := m.currentCodexDistribution(ctx, plan.Distribution)
		if err != nil {
			return err
		}
		comparison, err := CompareVersions(current.Version, plan.TargetVersion)
		if err != nil {
			return errors.New("Codex runtime update could not verify the installed version")
		}
		if comparison < 0 {
			if err := m.installCodexRuntime(ctx, current); err != nil {
				return err
			}
			current, err = m.currentCodexDistribution(ctx, plan.Distribution)
			if err != nil {
				return err
			}
			comparison, err = CompareVersions(current.Version, plan.TargetVersion)
			if err != nil || comparison < 0 {
				return errors.New("Codex runtime update did not install the required version")
			}
		}
		for profile, autostart := range plan.Profiles {
			if autostart {
				expectedVersions[profile] = current.Version
			}
		}
	}
	if !restart {
		return nil
	}
	after := m.updateNow()
	if err := m.Service(ctx, l, "start"); err != nil {
		return err
	}
	restarted = true
	if err := m.WorkerReady(ctx, l, after, ""); err != nil {
		return err
	}
	return m.codexRuntimeReady(ctx, l, after, expectedVersions)
}

func (m *Manager) managedCodexWorkerPID(ctx context.Context, l *Layout) (int, error) {
	if l.System == "linux" {
		data, err := m.command(ctx, "systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState")
		if err != nil {
			return 0, err
		}
		values := serviceDetails(data)
		pid, err := strconv.Atoi(values["MainPID"])
		if err == nil && pid > 0 && values["ActiveState"] == "active" {
			return pid, nil
		}
	} else {
		data, err := m.command(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/com.iaia.codex-worker")
		if err != nil {
			return 0, err
		}
		pid, matches := 0, 0
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.TrimSpace(key) == "pid" {
				pid, err = strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					return 0, errors.New("worker service has no verifiable process")
				}
				matches++
			}
		}
		if pid > 0 && matches == 1 {
			return pid, nil
		}
	}
	return 0, errors.New("worker service has no verifiable process")
}

func (m *Manager) codexRuntimeReady(ctx context.Context, l *Layout, after time.Time, expected map[string]string) error {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	workerID, _ := cfg["worker_id"].(string)
	ctx, cancel := m.readyContext(ctx)
	defer cancel()
	for ctx.Err() == nil {
		pid, pidErr := m.managedCodexWorkerPID(ctx, l)
		data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
		var status struct {
			PID       int       `json:"pid"`
			WorkerID  string    `json:"worker_id"`
			UpdatedAt time.Time `json:"updated_at"`
			Connected bool      `json:"gateway_connected"`
			Runtimes  []struct {
				ProfileID string `json:"profile_id"`
				State     string `json:"state"`
				PID       int    `json:"pid"`
				Version   string `json:"codex_version"`
			} `json:"runtimes"`
		}
		if pidErr == nil && err == nil && json.Unmarshal(data, &status) == nil && status.PID == pid && status.WorkerID == workerID && workerID != "" && !status.UpdatedAt.Before(after) && status.Connected {
			seen := make(map[string]bool)
			ready := true
			for _, runtime := range status.Runtimes {
				version, required := expected[runtime.ProfileID]
				if !required {
					continue
				}
				if seen[runtime.ProfileID] || runtime.PID <= 0 || runtime.State != "running" {
					ready = false
				}
				seen[runtime.ProfileID] = true
				actual, err := parseCodexVersion(runtime.Version)
				if err != nil {
					ready = false
					continue
				}
				comparison, err := CompareVersions(actual, version)
				ready = ready && err == nil && comparison >= 0
			}
			if ready && len(seen) == len(expected) {
				return nil
			}
		}
		if m.readyPause(ctx) != nil {
			break
		}
	}
	return errors.New("worker restarted but the updated Codex runtimes did not become ready; inspect its service logs")
}
