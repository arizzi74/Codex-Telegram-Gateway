package releasemanager

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

func (m *Manager) requestWorkerUpdate(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "worker" {
		return errors.New("request-update requires worker")
	}
	flags := flag.NewFlagSet("request-update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var id, stateFile string
	flags.StringVar(&id, "request-id", "", "durable request UUID")
	flags.StringVar(&stateFile, "worker-state", "", "worker state path")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("invalid worker update request arguments")
	}
	request, err := workerupdate.Read(stateFile, id)
	if err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return err
	}
	l, err := NewLayout("worker")
	if err != nil {
		return workerupdate.Complete(stateFile, protocol.WorkerUpdateResult{RequestID: id, State: "failed", ErrorCode: "update_unavailable"})
	}
	if err := l.RequireUser(); err != nil {
		return workerupdate.Complete(stateFile, protocol.WorkerUpdateResult{RequestID: id, State: "failed", ErrorCode: "update_unavailable"})
	}
	// The request names only a durable queue, never an arbitrary installation.
	// Bind it to the installed config before invoking any service operation.
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return workerupdate.Complete(stateFile, protocol.WorkerUpdateResult{RequestID: id, State: "failed", ErrorCode: "update_unavailable"})
	}
	configuredState, _ := cfg["state_file"].(string)
	if !filepath.IsAbs(configuredState) {
		configuredState = filepath.Join(filepath.Dir(l.Config), configuredState)
	}
	if cfg["worker_id"] != request.WorkerID || filepath.Clean(configuredState) != filepath.Clean(stateFile) {
		return workerupdate.Complete(stateFile, protocol.WorkerUpdateResult{RequestID: id, State: "failed", ErrorCode: "update_unavailable"})
	}
	unlock, err := Lock(filepath.Join(workerupdate.Directory(stateFile), id+".run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := workerupdate.Result(stateFile, id); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp("", "codex-worker-request-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	plan := &requestedWorkerPlan{stage: stage, codexCache: filepath.Join(workerupdate.Directory(stateFile), id+".codex-check.json")}
	loggedBusy := false
	return runRequestedWorkerUpdate(ctx, stateFile, request, 5*time.Second, 30*time.Second, func(ctx context.Context) (protocol.WorkerUpdateResult, error) {
		result, err := m.requestedWorkerStep(ctx, l, plan)
		var busy *BusyError
		isBusy := errors.As(err, &busy)
		if err != nil && (!isBusy || !loggedBusy) && m.Out != nil {
			// Command stderr and untrusted preparation output are already removed
			// by the manager helpers. Keep private journal diagnostics bounded.
			diagnostic := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, err.Error())
			if len(diagnostic) > 512 {
				diagnostic = diagnostic[:512] + "..."
			}
			fmt.Fprintf(m.Out, "Worker update waiting/retrying: %s\n", diagnostic)
		}
		loggedBusy = isBusy
		return result, err
	})
}

type requestedWorkerPlan struct {
	stage        string
	release      *Release
	packages     map[string]string
	workerResult *protocol.WorkerUpdateResult
	codexReport  *protocol.CodexUpdateReport
	codexCache   string
}

// Explicit maintenance checks Codex once per durable request, independently of
// automatic discovery. Each component still obtains its own live idle lease.
func (m *Manager) requestedWorkerStep(ctx context.Context, l *Layout, plan *requestedWorkerPlan) (protocol.WorkerUpdateResult, error) {
	unlock, err := Lock(l.Lock)
	if err != nil {
		return protocol.WorkerUpdateResult{}, err
	}
	defer unlock()
	m.requestedCodexDiscovery(ctx, plan)
	if ctx.Err() != nil {
		return protocol.WorkerUpdateResult{}, ctx.Err()
	}
	// An interrupted runtime installation may leave its launcher unavailable.
	// Restore its journal before worker upgrade preflight or pending readiness
	// checks, which also depend on those configured runtime executables.
	journal, recoveryErr := loadCodexRecovery(l)
	if recoveryErr == nil && journal != nil {
		recoveryErr = m.updateCodexRuntime(ctx, l, false, plan.codexReport.LatestVersion)
	}
	if recoveryErr != nil {
		result := protocol.WorkerUpdateResult{}
		if plan.workerResult != nil {
			result = *plan.workerResult
		}
		report := *plan.codexReport
		report.State, report.ErrorCode = "failed", "update_failed"
		result.Codex = &report
		var busy *BusyError
		if ctx.Err() != nil || errors.As(recoveryErr, &busy) || plan.workerResult == nil {
			return result, recoveryErr
		}
		return result, nil
	}
	if plan.workerResult == nil {
		result, err := m.requestedWorkerReleaseStep(ctx, l, plan)
		if err != nil {
			report := *plan.codexReport
			if report.ErrorCode == "" {
				report.State, report.ErrorCode = "failed", "worker_update_failed"
			}
			result.Codex = &report
			return result, err
		}
		plan.workerResult = &result
	}
	return m.requestedCodexMaintenance(ctx, l, plan)
}

func (m *Manager) requestedWorkerReleaseStep(ctx context.Context, l *Layout, plan *requestedWorkerPlan) (protocol.WorkerUpdateResult, error) {
	settings, err := SavedSettings(l)
	if err != nil {
		return protocol.WorkerUpdateResult{}, err
	}
	if plan.release == nil {
		repo, _ := settings["repo"].(string)
		if repo == "" {
			repo = DefaultRepo
		}
		plan.release, err = m.FetchRelease(ctx, repo, "")
		if err != nil {
			return protocol.WorkerUpdateResult{}, err
		}
	}
	data, err := m.command(ctx, l.Binary, "version")
	if err != nil {
		return protocol.WorkerUpdateResult{}, err
	}
	installed := strings.TrimSpace(string(data))
	comparison, err := CompareVersions(plan.release.Tag, installed)
	if err != nil {
		return protocol.WorkerUpdateResult{}, err
	}
	if comparison <= 0 {
		if pending, _ := settings["pending"].(bool); pending {
			if err := m.WorkerReady(ctx, l, m.updateNow().Add(-15*time.Second), installed); err != nil {
				return protocol.WorkerUpdateResult{}, err
			}
			repo, _ := settings["repo"].(string)
			if err := SaveSettings(l, repo, installed); err != nil {
				return protocol.WorkerUpdateResult{}, err
			}
			m.pruneUpdateBackups(l, installed)
		}
		return protocol.WorkerUpdateResult{State: "up_to_date", Version: installed}, nil
	}
	if plan.packages == nil {
		plan.packages = map[string]string{}
	}
	for _, component := range []string{"worker", "local"} {
		if plan.packages[component] != "" {
			continue
		}
		attemptStage, err := os.MkdirTemp(plan.stage, component+"-")
		if err != nil {
			return protocol.WorkerUpdateResult{}, err
		}
		packagePath, err := m.Package(ctx, plan.release, l, "codex-"+component, attemptStage)
		if err != nil {
			return protocol.WorkerUpdateResult{}, err
		}
		plan.packages[component] = packagePath
	}
	// ApplyUpdate reserves the idle worker before creating any backup files
	// and retains that reservation through the final check before stopping it.
	if err := m.ApplyUpdate(ctx, l, plan.packages, plan.release, false); err != nil {
		return protocol.WorkerUpdateResult{}, err
	}
	return protocol.WorkerUpdateResult{State: "completed", Version: plan.release.Tag}, nil
}

func runRequestedWorkerUpdate(ctx context.Context, stateFile string, request protocol.WorkerUpdateRequest, busyDelay, errorDelay time.Duration, step func(context.Context) (protocol.WorkerUpdateResult, error)) error {
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := step(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			result.RequestID = request.RequestID
			return workerupdate.Complete(stateFile, result)
		}
		delay := busyDelay
		var busy *BusyError
		if !errors.As(err, &busy) {
			failures++
			if failures >= 3 {
				result.RequestID, result.State, result.ErrorCode = request.RequestID, "failed", "update_failed"
				return workerupdate.Complete(stateFile, result)
			}
			delay = errorDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
