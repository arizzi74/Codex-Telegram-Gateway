package worker

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/workerupdate"
)

// Exercise the real webhook, SQLite queue, authenticated WebSocket, worker
// sidecar and durable outbox, and private Telegram delivery. Only the external
// installer and Telegram API are replaced; no installed services are touched.
func TestControlPlaneWorkerUpdateQueuedOfflineWithoutSession(t *testing.T) {
	registryStore, probe := controlPlaneRegistry(t)
	workerID := uuid.New()
	local, cfg := testConnectionStore(t, workerID.String())
	defer local.Close()
	cfg.StateFile = local.db.Path()
	cfg.GatewayURL = "wss://gateway.example.com/tgw/api/v1/workers/connect"
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(strings.TrimSpace(string(token))))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := registryStore.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "Offline worker", OS: "linux", Arch: "arm64", TokenHash: hash[:]}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	telegram := &telegramRecorder{}
	gw := startControlGateway(t, registryStore, telegram, log)
	defer gw.close()
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 100, "/tgupdateworkers", 0)
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 101, "/tgupdateworkers", 0)
	waitControl(t, func() bool { return telegram.hasText("Offline worker", "Already queued") })
	var requestID string
	if err := probe.QueryRowContext(ctx, `SELECT request_id FROM worker_update_requests WHERE worker_id=? AND state='pending'`, workerID).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	slot := &gatewaySlot{}
	slot.set(gw)
	var commands, launches atomic.Int32
	codexReport := &protocol.CodexUpdateReport{
		State: "up_to_date", LatestVersion: "0.157.1", CheckedAt: time.Now().UTC().Truncate(time.Second),
		Profiles: []protocol.CodexRuntimeVersion{{ProfileID: "primary", InstalledVersion: "0.157.1", RunningVersion: "0.157.1", Support: "supported"}},
	}
	c, err := dialTestServer(slot)(cfg, local, log, nil, func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		commands.Add(1)
		return protocol.CommandAck{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.updateManager = "/example/bin/codex-telegramgw"
	c.updateRun = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "systemctl" {
			return []byte("inactive"), nil
		}
		launches.Add(1)
		request, err := workerupdate.Read(cfg.StateFile, requestID)
		if err != nil || request.WorkerID != workerID.String() {
			t.Errorf("supervisor started without durable worker request: %+v, %v", request, err)
			return nil, err
		}
		err = workerupdate.Complete(cfg.StateFile, protocol.WorkerUpdateResult{RequestID: requestID, State: "up_to_date", Version: "0.5.61", Codex: codexReport})
		select {
		case c.updateWake <- struct{}{}:
		default:
		}
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker transport stopped: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker transport did not stop")
		}
	}()
	waitControl(t, func() bool { return telegram.hasText("Offline worker", "Worker binary up to date") })
	waitControl(t, func() bool { return telegram.hasText("Codex", "0.157.1", "primary") })
	waitControl(t, func() bool {
		events, err := local.OutboxAfter(0)
		return err == nil && len(events) == 0
	})
	if err := c.reconcileWorkerUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	if launches.Load() != 1 || commands.Load() != 0 {
		t.Fatalf("maintenance entered session commands or launched twice: launches=%d commands=%d", launches.Load(), commands.Load())
	}
	var state string
	if err := probe.QueryRowContext(ctx, `SELECT state FROM worker_update_requests WHERE request_id=?`, requestID).Scan(&state); err != nil || state != "up_to_date" {
		t.Fatalf("request outcome = %q, %v", state, err)
	}
	updates, err := registryStore.WorkerUpdateSnapshot(ctx)
	if err != nil || len(updates) != 1 {
		t.Fatalf("worker update snapshot = %+v, %v", updates, err)
	}
	report := updates[0].Codex
	if report == nil || report.State != codexReport.State || report.LatestVersion != codexReport.LatestVersion || !report.CheckedAt.Equal(codexReport.CheckedAt) || len(report.Profiles) != 1 || report.Profiles[0] != codexReport.Profiles[0] {
		t.Fatalf("Codex report lost through worker sidecar, outbox, or gateway persistence: %+v", report)
	}
	var bindings int
	if err := probe.QueryRowContext(ctx, `SELECT count(*) FROM telegram_bindings`).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("maintenance created session selection: %d, %v", bindings, err)
	}
	if count := telegram.countText("Worker binary up to date"); count != 1 {
		t.Fatalf("completion messages = %d", count)
	}
	telegram.mu.Lock()
	defer telegram.mu.Unlock()
	for _, message := range telegram.messages {
		if message.ChatID != 9 {
			t.Errorf("worker result escaped requesting chat: %+v", message)
		}
	}
}
