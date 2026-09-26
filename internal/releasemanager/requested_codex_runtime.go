package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

// Keep discovery beside the durable request so an updater restart or a busy
// worker cannot turn one explicit check into a repeated metadata polling loop.
// This cache never changes the automatic daily discovery timestamp.
func (m *Manager) requestedCodexDiscovery(ctx context.Context, plan *requestedWorkerPlan) {
	if plan.codexReport != nil {
		return
	}
	report := &protocol.CodexUpdateReport{State: "failed", ErrorCode: "check_failed", CheckedAt: m.updateNow().UTC()}
	plan.codexReport = report
	if plan.codexCache != "" {
		info, err := os.Lstat(plan.codexCache)
		if err == nil {
			if !codexOwnedPath(plan.codexCache, false) || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
				return
			}
			data, err := os.ReadFile(plan.codexCache)
			var cached protocol.CodexUpdateReport
			if err != nil || json.Unmarshal(data, &cached) != nil || cached.Validate() != nil || cached.CheckedAt.IsZero() || cached.CheckedAt.After(m.updateNow().Add(5*time.Minute)) || len(cached.Profiles) != 0 {
				return
			}
			if cached.State == "up_to_date" {
				if _, err := ParseVersion(cached.LatestVersion); err != nil {
					return
				}
			} else if cached.State != "failed" || cached.ErrorCode != "check_failed" {
				return
			}
			plan.codexReport = &cached
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	latest, err := m.fetchCodexLatest(ctx)
	if ctx.Err() != nil {
		// Cancellation does not consume the request's opportunity to check.
		plan.codexReport = nil
		return
	}
	if err == nil {
		report.State, report.ErrorCode, report.LatestVersion = "up_to_date", "", latest
	}
	if plan.codexCache != "" {
		data, err := json.Marshal(report)
		if err != nil || workerupdate.WritePrivate(plan.codexCache, data) != nil {
			report.State, report.ErrorCode = "failed", "check_failed"
		}
	}
}

func (m *Manager) requestedCodexMaintenance(ctx context.Context, l *Layout, plan *requestedWorkerPlan) (protocol.WorkerUpdateResult, error) {
	result := *plan.workerResult
	report := *plan.codexReport
	result.Codex = &report
	profiles, err := m.requestedCodexVersions(ctx, l)
	report.Profiles = profiles
	if report.ErrorCode != "" {
		return result, nil
	}
	if err != nil {
		var busy *BusyError
		if errors.As(err, &busy) {
			return result, err
		}
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	if len(profiles) == 0 {
		report.State = "no_runtimes"
		return result, nil
	}
	if !slices.ContainsFunc(profiles, func(p protocol.CodexRuntimeVersion) bool { return p.Support == "supported" }) {
		report.State = "unsupported"
		return result, nil
	}
	// The runtime helper performs recovery, quarantine, preflight and readiness
	// checks. In particular it acquires a fresh lease after any worker upgrade.
	err = m.updateCodexRuntime(ctx, l, false, report.LatestVersion)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var busy *BusyError
	if errors.As(err, &busy) {
		return result, err
	}
	after, inspectErr := m.requestedCodexVersions(ctx, l)
	report.Profiles = after
	if err != nil {
		report.State, report.ErrorCode = "failed", "update_failed"
		return result, nil
	}
	if inspectErr != nil {
		if errors.As(inspectErr, &busy) {
			return result, inspectErr
		}
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	state, err := loadCodexUpdateState(l)
	if err != nil {
		report.State, report.ErrorCode = "failed", "inspection_failed"
		return result, nil
	}
	for _, profile := range after {
		if profile.Support != "supported" {
			continue
		}
		comparison, _ := CompareVersions(report.LatestVersion, profile.InstalledVersion)
		if comparison > 0 && slices.Contains(state.RejectedVersions, report.LatestVersion) {
			report.State, report.ErrorCode = "withheld", "rejected_release"
			return result, nil
		}
		runningBehind := false
		if profile.RunningVersion != "" {
			comparison, _ := CompareVersions(profile.RunningVersion, profile.InstalledVersion)
			runningBehind = comparison < 0
		}
		if comparison > 0 || runningBehind {
			// An interrupted prepared transaction can recover without installing.
			// Re-enter maintenance on the next retry instead of declaring success.
			return result, &BusyError{Reason: "Codex runtime maintenance recovered; retrying the requested update"}
		}
	}
	report.State = "up_to_date"
	for _, profile := range after {
		if profile.Support != "supported" {
			continue
		}
		for _, previous := range profiles {
			if previous.ProfileID == profile.ProfileID && previous != profile {
				report.State = "completed"
			}
		}
	}
	return result, nil
}

func (m *Manager) requestedCodexVersions(ctx context.Context, l *Layout) ([]protocol.CodexRuntimeVersion, error) {
	data, err := os.ReadFile(l.Config)
	if err != nil {
		return nil, errors.New("cannot inspect configured Codex runtimes")
	}
	var cfg struct {
		WorkerID string                `json:"worker_id"`
		Profiles []codexRuntimeProfile `json:"runtimes"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Profiles) > 128 {
		return nil, errors.New("cannot inspect configured Codex runtimes")
	}
	profiles := make([]protocol.CodexRuntimeVersion, 0, len(cfg.Profiles))
	required := make(map[string]bool)
	var inspectionErr error
	for _, profile := range cfg.Profiles {
		required[profile.ID] = profile.Autostart
		entry := protocol.CodexRuntimeVersion{ProfileID: profile.ID, Support: "unavailable"}
		binary, err := codexProfileBinary(l, profile.Binary)
		if err == nil {
			distribution, distributionErr := m.inspectCodexDistribution(ctx, binary)
			if distributionErr == nil {
				entry.InstalledVersion, entry.Support = distribution.Version, "supported"
			} else if errors.Is(distributionErr, ErrCodexDistributionUnsupported) {
				output, versionErr := m.command(ctx, binary, "--version")
				version := codexReportedVersion(string(output))
				if versionErr == nil && version != "" {
					entry.InstalledVersion, entry.Support = version, "external"
				}
			}
		}
		if entry.Support == "unavailable" {
			inspectionErr = errors.New("cannot inspect a configured Codex runtime")
		}
		profiles = append(profiles, entry)
	}
	if len(profiles) > 0 {
		running, err := m.workerRunning(ctx, l)
		if err != nil {
			if inspectionErr == nil {
				inspectionErr = &BusyError{Reason: "Codex runtime update deferred: worker status is not yet available"}
			}
		} else if running {
			var observed codexObservedStatus
			data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
			if err != nil {
				if inspectionErr == nil {
					inspectionErr = &BusyError{Reason: "Codex runtime update deferred: fresh worker status is unavailable"}
				}
			} else if json.Unmarshal(data, &observed) != nil || observed.WorkerID != cfg.WorkerID || observed.PID <= 0 {
				inspectionErr = errors.New("cannot verify running Codex versions")
			} else if m.updateNow().Sub(observed.UpdatedAt) > 20*time.Second || observed.UpdatedAt.After(m.updateNow().Add(5*time.Second)) {
				if inspectionErr == nil {
					inspectionErr = &BusyError{Reason: "Codex runtime update deferred: fresh worker status is unavailable"}
				}
			} else {
				for i := range profiles {
					verified := false
					for _, runtime := range observed.Runtimes {
						if runtime.ProfileID != profiles[i].ProfileID || runtime.State != "running" {
							continue
						}
						version := codexReportedVersion(runtime.Version)
						if profiles[i].Support == "supported" {
							if _, err := parseCodexVersion(version); err != nil {
								inspectionErr = errors.New("cannot verify running Codex versions")
								continue
							}
						}
						profiles[i].RunningVersion, verified = version, version != ""
					}
					if required[profiles[i].ProfileID] && !verified && inspectionErr == nil {
						inspectionErr = &BusyError{Reason: "Codex runtime update deferred: an autostart runtime is not ready"}
					}
				}
			}
		}
	}
	// Do not let malformed local metadata inject messages or make the durable
	// completion unrepresentable. Report inspection failure without those values.
	if (protocol.CodexUpdateReport{State: "up_to_date", Profiles: profiles}).Validate() != nil {
		return nil, errors.New("invalid configured Codex version metadata")
	}
	return profiles, inspectionErr
}

func codexReportedVersion(output string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(output), "codex-cli "), "codex "))
}
