package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

func TestUpdatePrepareReportsRejectionOnStdoutAndStillFails(t *testing.T) {
	message := "worker update: session has active or queued work"
	testUpdatePrepareRejection(t, message, message)
}

func TestUpdatePrepareBoundsError(t *testing.T) {
	testUpdatePrepareRejection(t, strings.Repeat("detail", 400), "worker update: preparation failed; inspect the worker logs")
}

func testUpdatePrepareRejection(t *testing.T, rejection, wantOutput string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "s")
	listener, err := net.Listen("unix", state+".control.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		var request map[string]string
		if err := json.NewDecoder(connection).Decode(&request); err != nil {
			done <- err
			return
		}
		done <- json.NewEncoder(connection).Encode(map[string]string{"error": rejection})
	}()
	var output bytes.Buffer
	err = requestUpdate(context.Background(), config.WorkerConfig{WorkerID: "worker", StateFile: state}, "prepare", "", &output)
	if err == nil || err.Error() != rejection {
		t.Fatalf("rejected preparation must preserve its error: %v", err)
	}
	var result struct {
		Error string `json:"error"`
	}
	if decodeErr := json.Unmarshal(output.Bytes(), &result); decodeErr != nil || result.Error != wantOutput {
		t.Fatalf("stdout must contain the structured rejection: %q, %v", output.String(), decodeErr)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
