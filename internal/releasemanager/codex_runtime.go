package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type codexUpdateState struct {
	Schema                  int       `json:"schema"`
	CheckedAt               time.Time `json:"checked_at,omitempty"`
	LatestVersion           string    `json:"latest_version,omitempty"`
	CheckFailed             bool      `json:"check_failed,omitempty"`
	RestartPending          bool      `json:"restart_pending,omitempty"`
	RejectedVersions        []string  `json:"rejected_versions,omitempty"`
	RejectedByWorkerVersion string    `json:"rejected_by_worker_version,omitempty"`
}

type codexRuntimeProfile struct {
	ID        string `json:"id"`
	Binary    string `json:"codex_binary"`
	Autostart bool   `json:"autostart"`
}

type codexObservedStatus struct {
	WorkerID  string    `json:"worker_id"`
	PID       int       `json:"pid"`
	UpdatedAt time.Time `json:"updated_at"`
	Runtimes  []struct {
		ProfileID string `json:"profile_id"`
		Version   string `json:"codex_version"`
		State     string `json:"state"`
	} `json:"runtimes"`
}

func codexUpdateStatePath(l *Layout) string {
	return filepath.Join(filepath.Dir(l.State), "codex-update.json")
}

func loadCodexUpdateState(l *Layout) (codexUpdateState, error) {
	state := codexUpdateState{Schema: 1}
	data, err := os.ReadFile(codexUpdateStatePath(l))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil || len(data) > 8192 {
		return state, errors.New("cannot read Codex runtime update state")
	}
	if json.Unmarshal(data, &state) != nil || state.Schema != 1 {
		return state, errors.New("invalid Codex runtime update state")
	}
	if state.RejectedByWorkerVersion != "" {
		if _, err := ParseVersion(state.RejectedByWorkerVersion); err != nil {
			return state, errors.New("invalid rejected Codex worker version")
		}
	}
	if len(state.RejectedVersions) > 64 {
		return state, errors.New("too many rejected Codex runtime versions")
	}
	for _, version := range state.RejectedVersions {
		if _, err := ParseVersion(version); err != nil {
			return state, errors.New("invalid rejected Codex runtime version")
		}
	}
	if state.LatestVersion != "" {
		if _, err := ParseVersion(state.LatestVersion); err != nil {
			return state, errors.New("invalid cached Codex runtime version")
		}
	}
	return state, nil
}

func codexCheckDue(state codexUpdateState, now time.Time) bool {
	// Calendar days keep daily timers with randomized delays from skipping
	// every other check merely because today's timer fired a little earlier.
	return state.CheckedAt.IsZero() || state.CheckedAt.UTC().Format(time.DateOnly) != now.UTC().Format(time.DateOnly) || state.CheckedAt.After(now.Add(5*time.Minute))
}

func codexProfileBinary(l *Layout, binary string) (string, error) {
	if binary == "" {
		return "", errors.New("configured Codex runtime executable is unavailable")
	}
	if filepath.IsAbs(binary) {
		return binary, nil
	}
	if strings.ContainsRune(binary, filepath.Separator) {
		return filepath.Join(filepath.Dir(l.Config), binary), nil
	}
	if candidate := filepath.Join(l.Bin, binary); FileExists(candidate) {
		return candidate, nil
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", errors.New("configured Codex runtime executable is unavailable")
	}
	return path, nil
}

func (m *Manager) codexRuntimePlans(ctx context.Context, l *Layout) ([]codexRuntimePlan, string, error) {
	data, err := os.ReadFile(l.Config)
	if err != nil {
		return nil, "", errors.New("cannot read worker runtime configuration")
	}
	var cfg struct {
		WorkerID string                `json:"worker_id"`
		Profiles []codexRuntimeProfile `json:"runtimes"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return nil, "", errors.New("invalid worker runtime configuration")
	}
	var plans []codexRuntimePlan
	byHome := make(map[string]int)
	for _, profile := range cfg.Profiles {
		if profile.ID == "" || profile.Binary == "" {
			return nil, "", errors.New("invalid configured Codex runtime profile")
		}
		binary, err := codexProfileBinary(l, profile.Binary)
		if err != nil {
			return nil, "", err
		}
		distribution, err := m.inspectCodexDistribution(ctx, binary)
		if errors.Is(err, ErrCodexDistributionUnsupported) {
			fmt.Fprintln(m.Out, "Codex runtime update skipped: this profile does not use a supported standalone installation.")
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if index, exists := byHome[distribution.Home]; exists {
			plans[index].Profiles[profile.ID] = profile.Autostart
			continue
		}
		byHome[distribution.Home] = len(plans)
		plans = append(plans, codexRuntimePlan{Distribution: distribution, Profiles: map[string]bool{profile.ID: profile.Autostart}})
	}
	return plans, cfg.WorkerID, nil
}

// UpdateCodexRuntime runs under the same lock as worker binary maintenance.
// Only release discovery is daily: cached upgrades and an already-updated
// launcher are reconsidered at every scheduler tick until the worker is idle.
func (m *Manager) UpdateCodexRuntime(ctx context.Context, l *Layout, check bool) error {
	return m.updateCodexRuntime(ctx, l, check, "")
}

// Explicit requests supply their freshly checked stable version. They do not
// change the automatic discovery clock or refetch metadata while waiting idle.
func (m *Manager) updateCodexRuntime(ctx context.Context, l *Layout, check bool, latestOverride string) error {
	if l.Component != "worker" {
		return errors.New("Codex runtime updates require a worker installation")
	}
	journal, err := loadCodexRecovery(l)
	if err != nil {
		return err
	}
	if journal != nil {
		if check {
			return &BusyError{Reason: "Codex runtime recovery is pending"}
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if err := m.recoverCodexRuntime(cleanup, l, journal); err != nil {
			return err
		}
		if m.Out != nil {
			fmt.Fprintln(m.Out, "Interrupted Codex runtime maintenance recovered; normal update checks resume on the next tick.")
		}
		return nil
	}
	state, err := loadCodexUpdateState(l)
	if err != nil {
		return err
	}
	if latestOverride != "" {
		if _, err := ParseVersion(latestOverride); err != nil {
			return errors.New("invalid explicit Codex release version")
		}
		state.LatestVersion, state.CheckFailed = latestOverride, false
	}
	if len(state.RejectedVersions) > 0 && state.RejectedByWorkerVersion != "" {
		if version, err := m.installedCodexWorkerVersion(ctx, l); err == nil && codexWorkerUpgrade(version, state.RejectedByWorkerVersion) {
			state.RejectedVersions = nil
			state.RejectedByWorkerVersion = ""
			if !check {
				if err = WriteJSON(codexUpdateStatePath(l), state); err != nil {
					return err
				}
			}
		}
	}
	plans, workerID, err := m.codexRuntimePlans(ctx, l)
	if err != nil || len(plans) == 0 {
		if state.RestartPending && !check {
			// Recovery of a stopped worker must not depend on an intact native
			// installation or a successful release metadata request.
			recoveryErr := m.restorePendingCodexWorker(ctx, l)
			return errors.Join(err, recoveryErr, errors.New("Codex runtime restart is pending but its installation could not be verified"))
		}
		return err
	}
	var checkErr error
	if latestOverride == "" && codexCheckDue(state, m.updateNow()) {
		state.CheckedAt = m.updateNow().UTC()
		state.CheckFailed = true
		if !check {
			// Record the attempt before networking. Failures and process restarts
			// must not turn the five-minute timer into repeated release checks.
			if err := WriteJSON(codexUpdateStatePath(l), state); err != nil {
				return err
			}
		}
		latest, err := m.fetchCodexLatest(ctx)
		if err != nil {
			checkErr = err
		} else {
			state.LatestVersion = latest
			state.CheckFailed = false
			if !check {
				if err := WriteJSON(codexUpdateStatePath(l), state); err != nil {
					return err
				}
			}
		}
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return errors.Join(checkErr, err)
	}
	var observed codexObservedStatus
	if running {
		data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
		if err != nil || json.Unmarshal(data, &observed) != nil || observed.WorkerID != workerID || observed.PID <= 0 ||
			m.updateNow().Sub(observed.UpdatedAt) > 20*time.Second || observed.UpdatedAt.After(m.updateNow().Add(5*time.Second)) {
			return errors.Join(checkErr, &BusyError{Reason: "Codex runtime update deferred: fresh worker status is unavailable"})
		}
	}
	maintenance := state.RestartPending
	for index := range plans {
		plan := &plans[index]
		plan.TargetVersion = plan.Distribution.Version
		if state.LatestVersion != "" {
			comparison, err := CompareVersions(state.LatestVersion, plan.TargetVersion)
			if err != nil {
				return err
			}
			if comparison > 0 && !slices.Contains(state.RejectedVersions, state.LatestVersion) {
				plan.TargetVersion = state.LatestVersion
				plan.NeedsInstall, maintenance = true, true
				fmt.Fprintf(m.Out, "Codex runtime update available: %s -> %s.\n", plan.Distribution.Version, state.LatestVersion)
			}
		}
		for _, runtime := range observed.Runtimes {
			if _, matches := plan.Profiles[runtime.ProfileID]; !matches || runtime.State == "stopped" {
				continue
			}
			version, err := parseCodexVersion(runtime.Version)
			if err != nil {
				return errors.Join(checkErr, &BusyError{Reason: "Codex runtime update deferred: running app-server version is unavailable"})
			}
			if comparison, _ := CompareVersions(version, plan.Distribution.Version); comparison < 0 {
				maintenance = true
				fmt.Fprintf(m.Out, "Codex app-server restart pending: %s -> %s.\n", version, plan.Distribution.Version)
			}
		}
	}
	if slices.Contains(state.RejectedVersions, state.LatestVersion) && m.Out != nil {
		fmt.Fprintln(m.Out, "The latest Codex runtime release previously failed validation and will not be retried automatically.")
	}
	if check || !maintenance {
		if !maintenance && checkErr == nil {
			if slices.Contains(state.RejectedVersions, state.LatestVersion) {
				// Failure notice above explains why this installation stays on its prior release.
			} else if state.CheckFailed {
				fmt.Fprintln(m.Out, "The last Codex release check failed; it will be retried at the next daily check.")
			} else {
				fmt.Fprintln(m.Out, "Codex runtime is current; automatic release checks run once per UTC day.")
			}
		}
		return checkErr
	}
	if err := m.applyCodexRuntimeUpdates(ctx, l, plans, state.RestartPending, func() error {
		state.RestartPending = true
		return WriteJSON(codexUpdateStatePath(l), state)
	}); err != nil {
		return errors.Join(checkErr, err)
	}
	state.RestartPending = false
	if err := WriteJSON(codexUpdateStatePath(l), state); err != nil {
		return errors.Join(checkErr, err)
	}
	fmt.Fprintln(m.Out, "Codex runtime maintenance completed.")
	return checkErr
}

func (m *Manager) restorePendingCodexWorker(ctx context.Context, l *Layout) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	running, err := m.workerRunning(ctx, l)
	if err != nil || running {
		return err
	}
	after := m.updateNow()
	if err := m.Service(ctx, l, "start"); err != nil {
		return errors.New("Codex runtime recovery could not restore the worker service; inspect its service logs")
	}
	return m.WorkerReady(ctx, l, after, "")
}

func codexWorkerUpgrade(current, previous string) bool {
	comparison, err := CompareVersions(current, previous)
	return err == nil && comparison > 0
}
