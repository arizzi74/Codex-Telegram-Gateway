package workerupdate

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestConcurrentRequestReplayCannotReplaceIdentityOrOutcome(t *testing.T) {
	state := filepath.Join(t.TempDir(), "worker.db")
	request := protocol.WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString()}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if err := Create(state, request); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	changed := request
	changed.WorkerID = uuid.NewString()
	if err := Create(state, changed); err == nil {
		t.Fatal("replay changed worker")
	}
	requests, err := List(state)
	if err != nil || len(requests) != 1 || requests[0] != request {
		t.Fatalf("requests=%v err=%v", requests, err)
	}
	result := protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "completed", Version: "v1.2.3"}
	if err := Complete(state, result); err != nil {
		t.Fatal(err)
	}
	if err := Complete(state, protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "failed", ErrorCode: "update_failed"}); err != nil {
		t.Fatal(err)
	}
	got, err := Result(state, request.RequestID)
	if err != nil || got != result {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	entries, err := os.ReadDir(Directory(state))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().Perm()&0077 != 0 {
			t.Fatal("private queue file is readable by other users")
		}
	}
	if _, err := Read(state, "../outside"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("accepted traversal")
	}
}
