package protocol

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWorkerUpdateMessagesHaveOnlyWorkerTargetsAndBoundedResults(t *testing.T) {
	request := WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString()}
	frame, err := NewEnvelope("worker_update_request", request)
	if err != nil || frame.Validate() != nil || request.Validate() != nil {
		t.Fatalf("worker-only maintenance rejected: %#v %v", frame, err)
	}
	for _, invalid := range []WorkerUpdateRequest{{}, {RequestID: request.RequestID}, {RequestID: uuid.Nil.String(), WorkerID: request.WorkerID}, {RequestID: request.RequestID, WorkerID: "session"}} {
		if invalid.Validate() == nil {
			t.Fatalf("invalid request accepted: %#v", invalid)
		}
	}
	for _, state := range []string{"completed", "up_to_date", "failed"} {
		result := WorkerUpdateResult{RequestID: request.RequestID, State: state, Version: "0.5.29"}
		if state == "failed" {
			result.ErrorCode = "update_failed"
		}
		if err := result.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []WorkerUpdateResult{
		{RequestID: request.RequestID, State: "completed"},
		{RequestID: request.RequestID, State: "up_to_date", Version: "1.0.0", ErrorCode: "error"},
		{RequestID: request.RequestID, State: "failed"},
		{RequestID: request.RequestID, State: "failed", ErrorCode: "raw error with private path"},
		{RequestID: request.RequestID, State: "running", Version: "1.0.0"},
		{RequestID: request.RequestID, State: "completed", Version: "1.0\n0"},
		{RequestID: request.RequestID, State: "completed", Version: strings.Repeat("1", 129)},
	} {
		if invalid.Validate() == nil {
			t.Fatalf("invalid outcome accepted: %#v", invalid)
		}
	}
}

func TestWorkerUpdateCodexReportsAreOptionalAndBounded(t *testing.T) {
	base := WorkerUpdateResult{RequestID: uuid.NewString(), State: "completed", Version: "0.5.61"}
	for _, state := range []string{"up_to_date", "completed", "unsupported", "no_runtimes", "failed", "withheld"} {
		report := &CodexUpdateReport{State: state, LatestVersion: "0.157.1", CheckedAt: time.Now().UTC(), Profiles: []CodexRuntimeVersion{{ProfileID: "main", InstalledVersion: "0.157.1", RunningVersion: "0.156.0", Support: "supported"}}}
		if state == "failed" {
			report.ErrorCode = "update_failed"
		} else if state == "withheld" {
			report.ErrorCode = "rejected_release"
		}
		result := base
		result.Codex = report
		if err := result.Validate(); err != nil {
			t.Fatalf("valid %s report rejected: %v", state, err)
		}
	}
	for _, report := range []CodexUpdateReport{
		{State: "unknown"},
		{State: "up_to_date", LatestVersion: "0.157.1\nprivate"},
		{State: "up_to_date", LatestVersion: strings.Repeat("1", 129)},
		{State: "failed", ErrorCode: "private raw error"},
		{State: "failed"},
		{State: "withheld", ErrorCode: "update_failed"},
		{State: "completed", ErrorCode: "check_failed"},
		{State: "completed", Profiles: make([]CodexRuntimeVersion, 129)},
		{State: "completed", Profiles: []CodexRuntimeVersion{{ProfileID: "main", Support: "other"}}},
		{State: "completed", Profiles: []CodexRuntimeVersion{{ProfileID: "main\nprivate", Support: "supported"}}},
		{State: "completed", Profiles: []CodexRuntimeVersion{{ProfileID: "main", Support: "supported", RunningVersion: "https://private.example"}}},
		{State: "completed", Profiles: []CodexRuntimeVersion{{ProfileID: "main", Support: "supported"}, {ProfileID: "main", Support: "external"}}},
	} {
		result := base
		result.Codex = &report
		if result.Validate() == nil {
			t.Fatalf("invalid Codex report accepted: %#v", report)
		}
	}
}
