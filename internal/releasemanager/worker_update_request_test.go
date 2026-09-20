package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

func TestRequestedWorkerAlreadyCurrentNeverTouchesServicesOrRuntime(t *testing.T) {
	for _, installed := range []string{"1.2.3", "1.3.0"} {
		t.Run(installed, func(t *testing.T) {
			l, _, release := updateFixture(t, "worker")
			l.Lock = filepath.Join(l.Home, "update.lock")
			m := New(nil)
			calls := 0
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				calls++
				if len(args) != 2 || args[0] != l.Binary || args[1] != "version" {
					t.Fatalf("same-version request touched services: %v", args)
				}
				return CommandResult{Output: []byte(installed)}, nil
			}
			result, err := m.requestedWorkerStep(t.Context(), l, &requestedWorkerPlan{release: release})
			if err != nil || result.State != "up_to_date" || result.Version != installed || calls != 1 {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}

func TestRequestedWorkerBusyKeepsTurnsAndTimersUntouched(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	l.Lock = filepath.Join(l.Home, "update.lock")
	m := New(nil)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		switch {
		case len(args) == 2 && args[1] == "version":
			return CommandResult{Output: []byte("1.0.0")}, nil
		case args[0] == "systemctl" && args[2] == "show":
			return CommandResult{Output: []byte("MainPID=1234\nActiveState=active\n")}, nil
		case strings.Join(args[1:], " ") == "--config "+l.Config+" update prepare":
			return CommandResult{Output: []byte("worker update: session has active or queued work"), ExitCode: 1}, nil
		default:
			t.Fatalf("busy request attempted mutation: %v", args)
			return CommandResult{}, nil
		}
	}
	for range 3 {
		_, err := m.requestedWorkerStep(t.Context(), l, &requestedWorkerPlan{release: release, packages: packages})
		var busy *BusyError
		if !errors.As(err, &busy) {
			t.Fatalf("expected busy, got %v", err)
		}
	}
	if _, err := os.Stat(l.Backups); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("busy polling created backups")
	}
}

func TestRequestedWorkerAppliesOnlyAfterBusyTurnEnds(t *testing.T) {
	l, packages, release := updateFixture(t, "worker")
	l.Lock = filepath.Join(l.Home, "update.lock")
	m := New(nil)
	busy := true
	prepared := false
	var actions []string
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if len(args) == 2 && args[0] == l.Binary && args[1] == "version" {
			return CommandResult{Output: []byte("1.0.0")}, nil
		}
		if args[0] == "systemctl" {
			if args[2] == "show" {
				if args[len(args)-1] == "--value" {
					return CommandResult{Output: []byte("123\n")}, nil
				}
				return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
			}
			actions = append(actions, args[2])
			if busy || args[2] == "stop" && !prepared {
				t.Fatal("stopped worker without current idle lease")
			}
			return CommandResult{}, nil
		}
		if args[0] == l.Binary && args[3] == "update" {
			if args[4] == "abort" {
				prepared = false
				return CommandResult{}, nil
			}
			if busy {
				return CommandResult{ExitCode: 75, Output: []byte("worker update: a session has an active turn or pending response")}, nil
			}
			prepared = true
			data, _ := json.Marshal(map[string]any{"worker_id": "worker-id", "pid": 123, "token": "lease", "expires_at": time.Now().Add(2 * time.Minute)})
			return CommandResult{Output: data}, nil
		}
		if args[0] == l.Binary && args[3] == "status" {
			data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": true, "version": "1.2.3", "runtimes": []any{}})
			return CommandResult{Output: data}, nil
		}
		t.Fatalf("unexpected command: %v", args)
		return CommandResult{}, nil
	}
	plan := &requestedWorkerPlan{release: release, packages: packages}
	if _, err := m.requestedWorkerStep(t.Context(), l, plan); err == nil {
		t.Fatal("active turn did not defer")
	}
	if len(actions) != 0 || updateRead(t, l.Binary) != "old binary" {
		t.Fatal("active worker changed")
	}
	busy = false
	result, err := m.requestedWorkerStep(t.Context(), l, plan)
	if err != nil || result.State != "completed" || result.Version != release.Tag {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !reflect.DeepEqual(actions, []string{"stop", "start"}) {
		t.Fatalf("unexpected service/timer actions %v", actions)
	}
	if updateRead(t, l.Binary) != "new binary" || updateRead(t, filepath.Join(l.Bin, "codex-local")) != "new local" {
		t.Fatal("new release was not installed")
	}
}

func TestRequestedWorkerRetriesBusyAndFailureAndPersistsFinal(t *testing.T) {
	state := filepath.Join(t.TempDir(), "worker.db")
	request := protocol.WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString()}
	if err := workerupdate.Create(state, request); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := runRequestedWorkerUpdate(t.Context(), state, request, time.Millisecond, time.Millisecond, func(context.Context) (protocol.WorkerUpdateResult, error) {
		calls++
		if calls <= 4 {
			return protocol.WorkerUpdateResult{}, &BusyError{Reason: "active turn"}
		}
		if calls == 5 {
			return protocol.WorkerUpdateResult{}, errors.New("temporary network failure")
		}
		return protocol.WorkerUpdateResult{State: "completed", Version: "v1.2.3"}, nil
	})
	if err != nil || calls != 6 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	result, err := workerupdate.Result(state, request.RequestID)
	if err != nil || result.State != "completed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRequestedWorkerCancellationLeavesQueueAndFailureIsBounded(t *testing.T) {
	state := filepath.Join(t.TempDir(), "worker.db")
	request := protocol.WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString()}
	if err := workerupdate.Create(state, request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	err := runRequestedWorkerUpdate(ctx, state, request, time.Millisecond, time.Millisecond, func(context.Context) (protocol.WorkerUpdateResult, error) {
		cancel()
		return protocol.WorkerUpdateResult{}, &BusyError{Reason: "active turn"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := workerupdate.Result(state, request.RequestID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled updater completed request")
	}
	calls := 0
	err = runRequestedWorkerUpdate(t.Context(), state, request, time.Millisecond, time.Millisecond, func(context.Context) (protocol.WorkerUpdateResult, error) {
		calls++
		return protocol.WorkerUpdateResult{}, errors.New("download unavailable")
	})
	result, readErr := workerupdate.Result(state, request.RequestID)
	if err != nil || readErr != nil || calls != 3 || result.State != "failed" || result.ErrorCode != "update_failed" {
		t.Fatalf("result=%+v calls=%d err=%v/%v", result, calls, err, readErr)
	}
}
