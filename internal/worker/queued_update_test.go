package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

func TestQueuedUpdateSurvivesRestartAndResultIsExactlyOnce(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	cfg.StateFile = store.db.Path()
	cfg.GatewayURL = "wss://gateway.example/tgworker"
	newConn := func(store *Store) *Connection {
		conn, err := NewConnection(cfg, store, nil, nil, func(context.Context, protocol.Command) (protocol.CommandAck, error) {
			t.Fatal("update entered command handler")
			return protocol.CommandAck{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		conn.updateManager = "/example/bin/manager"
		return conn
	}
	conn := newConn(store)
	request := protocol.WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: workerID}
	if err := conn.receiveWorkerUpdate(request); err != nil {
		t.Fatal(err)
	}
	if err := store.checkUpdateIdle(); err != nil {
		t.Fatalf("queued maintenance prevented idle lease: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(cfg.StateFile, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn = newConn(store)
	launches := 0
	conn.updateRun = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "systemctl" {
			return []byte("inactive"), nil
		}
		launches++
		return nil, nil
	}
	if err := conn.reconcileWorkerUpdates(t.Context()); err != nil || launches != 1 {
		t.Fatalf("recovery launches=%d err=%v", launches, err)
	}
	result := protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "completed", Version: "v1.2.3"}
	if err := workerupdate.Complete(cfg.StateFile, result); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := conn.receiveWorkerUpdate(request); err != nil {
			t.Fatal(err)
		}
		if err := conn.reconcileWorkerUpdates(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "worker_update_result" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	var got protocol.WorkerUpdateResult
	if err := json.Unmarshal(events[0].Data, &got); err != nil || got != result {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if events[0].RuntimeID != "" || events[0].SessionID != "" {
		t.Fatal("worker update bound to active session")
	}
	if err := store.AckThrough(events[0].Seq); err != nil {
		t.Fatal(err)
	}
	if err := conn.reconcileWorkerUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, _ = store.OutboxAfter(0)
	if len(events) != 0 || launches != 1 {
		t.Fatal("completion replay emitted another event or relaunched updater")
	}
}

func TestQueuedUpdateRetriesSupervisorFailureAndRejectsOtherWorker(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	cfg.StateFile = store.db.Path()
	cfg.GatewayURL = "wss://gateway.example/tgworker"
	defer store.Close()
	conn, err := NewConnection(cfg, store, nil, nil, func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		return protocol.CommandAck{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.WorkerUpdateRequest{RequestID: uuid.NewString(), WorkerID: uuid.NewString()}
	if err := conn.receiveWorkerUpdate(request); err == nil {
		t.Fatal("accepted wrong worker")
	}
	request.WorkerID = workerID
	if err := conn.receiveWorkerUpdate(request); err != nil {
		t.Fatal(err)
	}
	conn.updateManager = "/example/bin/manager"
	conn.updateRun = func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("user service bus temporarily unavailable")
	}
	if err := conn.reconcileWorkerUpdates(t.Context()); err == nil {
		t.Fatal("expected supervisor error")
	}
	if _, err := workerupdate.Result(cfg.StateFile, request.RequestID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("transient supervisor failure completed request")
	}
	conn.updateRun = func(context.Context, ...string) ([]byte, error) { return []byte("active"), nil }
	if err := conn.reconcileWorkerUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	conn.updateManager = ""
	if err := conn.reconcileWorkerUpdates(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, _ := store.OutboxAfter(0)
	if len(events) != 1 {
		t.Fatalf("missing unsupported result: %v", events)
	}
}
