package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	if len(plans) > 32 {
		return errors.New("too many Codex installations for safe update recovery")
	}
	if len(plans) == 0 {
		return nil
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return err
	}
	restart := running || resumeStopped
	journal := &codexRecovery{Schema: 1, Phase: "prepared", Restart: restart, ActiveInstall: -1}
	for _, plan := range plans {
		if _, err := ParseVersion(plan.TargetVersion); err != nil {
			return errors.New("Codex runtime update target is invalid")
		}
		current, err := m.currentCodexDistribution(ctx, plan.Distribution)
		if err != nil {
			return err
		}
		snapshot, err := snapshotCodexRuntime(current, plan.Profiles)
		if err != nil {
			return err
		}
		if plan.NeedsInstall {
			target := strings.TrimPrefix(filepath.Base(snapshot.ReleaseDir), snapshot.Distribution.Version+"-")
			snapshot.CandidateReleaseDir = filepath.Join(filepath.Dir(snapshot.ReleaseDir), plan.TargetVersion+"-"+target)
		}
		journal.Snapshots = append(journal.Snapshots, snapshot)
		if plan.NeedsInstall {
			journal.rejectVersion(plan.TargetVersion)
		}
		// An externally updated launcher can be checked without stopping the old
		// app server. A rejected candidate must not disrupt that still-running server.
		if !plan.NeedsInstall {
			if err := m.preflightCodexRuntime(ctx, current); err != nil {
				return err
			}
		}
	}
	if err := validateCodexRecoverySize(journal); err != nil {
		return err
	}
	var lease *workerLease
	stopped, journalWritten := false, false
	defer func() {
		if retErr == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if lease != nil && !stopped {
			if err := m.workerAbort(cleanup, l, lease.Token); err != nil {
				retErr = errors.Join(retErr, errors.New("worker reservation could not be released; it will expire automatically"))
			}
		}
		if !journalWritten {
			return
		}
		// Capture a partially installed candidate only for the installer that
		// just failed, never replace an already-recorded candidate identity.
		if journal.Phase == "installing" && journal.ActiveInstall >= 0 && journal.ActiveInstall < len(journal.Snapshots) {
			snapshot := &journal.Snapshots[journal.ActiveInstall]
			if !snapshot.CandidateObserved {
				current, err := m.currentCodexDistribution(cleanup, &snapshot.Distribution)
				if err == nil && current.Version != snapshot.Distribution.Version {
					if path, err := filepath.EvalSymlinks(current.Binary); err == nil {
						snapshot.CandidateReleaseDir = filepath.Dir(filepath.Dir(path))
						snapshot.CandidateObserved = true
						journal.rejectVersion(current.Version)
					}
				}
			}
		}
		if err := m.recoverCodexRuntime(cleanup, l, journal); err != nil {
			retErr = errors.Join(retErr, err)
		} else if journal.Phase == "prepared" {
			retErr = errors.Join(retErr, errors.New("Codex maintenance did not change the installation; worker service state restored"))
		} else if journal.Phase != "committed" {
			retErr = errors.Join(retErr, errors.New("previous Codex releases restored; failed releases will not be retried automatically"))
		}
	}()
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
		if err = recordRestart(); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if lease != nil && lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
		return &BusyError{Reason: "worker update reservation expired; retry later"}
	}
	if err = writeCodexRecovery(l, journal); err != nil {
		return err
	}
	journalWritten = true
	if running {
		if err = m.Service(ctx, l, "stop"); err != nil {
			return err
		}
	}
	if live, err := m.workerRunning(ctx, l); err != nil {
		return err
	} else if live {
		return errors.New("Codex runtime update could not confirm that the worker stopped")
	}
	stopped = true
	journal.Phase = "installing"
	if err = writeCodexRecovery(l, journal); err != nil {
		return err
	}
	expected := map[string]string{}
	for index, plan := range plans {
		current, err := m.currentCodexDistribution(ctx, plan.Distribution)
		if err != nil {
			return err
		}
		comparison, err := CompareVersions(current.Version, plan.TargetVersion)
		if err != nil {
			return errors.New("Codex runtime update could not verify the installed version")
		}
		if comparison < 0 {
			journal.ActiveInstall = index
			if err = writeCodexRecovery(l, journal); err != nil {
				return err
			}
			if err = m.installCodexRuntime(ctx, current); err != nil {
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
		if current.Version != plan.Distribution.Version {
			path, err := filepath.EvalSymlinks(current.Binary)
			if err != nil {
				return errors.New("cannot checkpoint updated Codex release")
			}
			journal.Snapshots[index].CandidateReleaseDir = filepath.Dir(filepath.Dir(path))
			journal.Snapshots[index].CandidateObserved = true
			journal.rejectVersion(current.Version)
		}
		journal.ActiveInstall = -1
		if err = writeCodexRecovery(l, journal); err != nil {
			return err
		}
		if err = m.preflightCodexRuntime(ctx, current); err != nil {
			return err
		}
		for profile, autostart := range plan.Profiles {
			if autostart {
				expected[profile] = current.Version
			}
		}
	}
	if restart {
		journal.Phase = "starting"
		if err = writeCodexRecovery(l, journal); err != nil {
			return err
		}
		after := m.updateNow()
		if err = m.Service(ctx, l, "start"); err != nil {
			return err
		}
		if err = m.WorkerReady(ctx, l, after, ""); err != nil {
			return err
		}
		if err = m.codexRuntimeReady(ctx, l, after, expected); err != nil {
			return err
		}
	}
	journal.Phase = "committed"
	if err = writeCodexRecovery(l, journal); err != nil {
		return err
	}
	return clearCodexRecovery(l)
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
