package registry

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Each integration test uses a private SQLite file, so database-backed tests
// always run and never touch the installed gateway database.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("open isolated SQLite store: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate isolated SQLite store: %v", err)
	}
	return store
}

func TestMigrateAndWorkerEnrollmentIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	if err := store.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	token := "cwk_test_token"
	hash := sha256.Sum256([]byte(token))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{
		Name: "test-worker", Hostname: "host", OS: "linux", Arch: "arm64", TokenHash: hash[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.AuthenticateWorker(ctx, token)
	if err != nil || got.ID != worker.ID {
		t.Fatalf("authenticate worker = %#v, %v", got, err)
	}
	if _, err := store.AuthenticateWorker(ctx, "not-the-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong token error = %v, want ErrInvalidToken", err)
	}
	workers, err := store.ListWorkers(ctx)
	if err != nil || len(workers) != 1 || workers[0].ID != worker.ID {
		t.Fatalf("list workers = %#v, %v", workers, err)
	}
	if watermark, err := store.EventWatermark(ctx, worker.ID); err != nil || watermark != 0 {
		t.Fatalf("event watermark = %d, %v", watermark, err)
	}
	if err := store.RotateWorkerToken(ctx, worker.ID, hash[:]); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateWorker(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked worker auth error = %v, want ErrInvalidToken", err)
	}
}

func TestConnectionFencingAndRuntimeGenerationIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	token := "cwk_test_token"
	hash := sha256.Sum256([]byte(token))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "worker", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceEventWatermark(ctx, worker.ID, first, 7); err != nil {
		t.Fatal(err)
	}
	if watermark, err := store.EventWatermark(ctx, worker.ID); err != nil || watermark != 7 {
		t.Fatalf("event watermark = %d, %v", watermark, err)
	}
	runtimeID := uuid.New()
	pid := int64(1234)
	if err := store.RecordHeartbeat(ctx, Heartbeat{WorkerID: worker.ID, ConnectionID: first, Runtimes: []Runtime{{
		ID: runtimeID, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 3, PID: &pid, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindConnection(ctx, worker.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceEventWatermark(ctx, worker.ID, first, 8); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("old connection watermark error = %v, want ErrConnectionFenced", err)
	}
	if err := store.RecordHeartbeat(ctx, Heartbeat{WorkerID: worker.ID, ConnectionID: first}); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("old connection heartbeat error = %v, want ErrConnectionFenced", err)
	}
	if err := store.RecordHeartbeat(ctx, Heartbeat{WorkerID: worker.ID, ConnectionID: second, Runtimes: []Runtime{{
		ID: runtimeID, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 2, State: "failed",
	}}}); err != nil {
		t.Fatal(err)
	}
	var generation int64
	var state string
	if err := store.pool.QueryRow(ctx, "SELECT generation, state FROM runtimes WHERE runtime_id = $1", runtimeID).Scan(&generation, &state); err != nil {
		t.Fatal(err)
	}
	if generation != 3 || state != "running" {
		t.Fatalf("stale heartbeat overwrote runtime: generation=%d state=%q", generation, state)
	}
	if err := store.CheckConnection(ctx, worker.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := store.Disconnect(ctx, worker.ID, second); err != nil {
		t.Fatal(err)
	}
	if err := store.CheckConnection(ctx, worker.ID, second); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("disconnected check = %v, want ErrConnectionFenced", err)
	}
}

func TestRegisterConnectionIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	token := "cwk_register_token"
	hash := sha256.Sum256([]byte(token))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "enrolled-name", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	connectionID, runtimeID := uuid.New(), uuid.New()
	registered, err := store.RegisterConnection(ctx, token, connectionID, protocol.Hello{
		WorkerID: worker.ID.String(), WorkerName: "actual-name", Hostname: "workstation",
		OS: "darwin", Arch: "arm64", WorkerVersion: "1.2.3", ProtocolMin: 1, ProtocolMax: 1,
		Runtimes: []protocol.Runtime{{ID: runtimeID.String(), ProfileID: "main", Name: "Main", Generation: 1, PID: 99, State: "running"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Name != "actual-name" || registered.Hostname != "workstation" || registered.OS != "darwin" || registered.Arch != "arm64" {
		t.Fatalf("hello metadata was not persisted: %#v", registered)
	}
	if err := store.CheckConnection(ctx, worker.ID, connectionID); err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := store.pool.QueryRow(ctx, "SELECT generation FROM runtimes WHERE runtime_id = $1", runtimeID).Scan(&generation); err != nil || generation != 1 {
		t.Fatalf("registered runtime = generation %d, err %v", generation, err)
	}
}

func TestCommandRoutingIsImmutableIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("cwk_test_token"))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "worker", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := uuid.New()
	if err := store.BindConnection(ctx, worker.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, Heartbeat{WorkerID: worker.ID, ConnectionID: mustConnectionID(t, store, worker.ID), Runtimes: []Runtime{{
		ID: runtimeID, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	commandID := uuid.New()
	if _, err := store.pool.Exec(ctx, `INSERT INTO commands
        (command_id, source, worker_id, runtime_id, runtime_generation, operation, payload, status)
        VALUES ($1, 'telegram', $2, $3, 1, 'start_turn', '{}', 'pending')`, commandID, worker.ID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE commands SET runtime_generation = 2 WHERE command_id = $1", commandID); err == nil {
		t.Fatal("mutable command routing update succeeded")
	}
	if _, err := store.pool.Exec(ctx, "UPDATE commands SET status = 'dispatched' WHERE command_id = $1", commandID); err != nil {
		t.Fatalf("command lifecycle update failed: %v", err)
	}
}

func mustConnectionID(t *testing.T, store *Store, workerID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := store.pool.QueryRow(context.Background(), "SELECT connection_id FROM workers WHERE worker_id = $1", workerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMarkUnreachableIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("cwk_test_token"))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "worker", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindConnection(ctx, worker.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, "UPDATE workers SET last_seen_at = (strftime('%Y-%m-%dT%H:%M:%f','now','-10 minutes') || '000000Z') WHERE worker_id = $1", worker.ID); err != nil {
		t.Fatal(err)
	}
	n, err := store.MarkUnreachable(ctx, time.Second)
	if err != nil || n != 1 {
		t.Fatalf("mark unreachable = %d, %v", n, err)
	}
	var connectivity string
	if err := store.pool.QueryRow(ctx, "SELECT connectivity FROM workers WHERE worker_id = $1", worker.ID).Scan(&connectivity); err != nil {
		t.Fatal(err)
	}
	if connectivity != "unreachable" {
		t.Fatalf("connectivity = %q", connectivity)
	}
}

func ExampleCreateWorkerInput() {
	digest := sha256.Sum256([]byte("plaintext enrollment token held by the CLI"))
	input := CreateWorkerInput{Name: "laptop", OS: "darwin", Arch: "arm64", TokenHash: digest[:]}
	fmt.Println(len(input.TokenHash))
	// Output: 32
}
