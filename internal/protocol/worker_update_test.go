package protocol

import (
	"strings"
	"testing"

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
