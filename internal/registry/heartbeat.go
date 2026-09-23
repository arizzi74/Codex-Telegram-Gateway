package registry

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Runtime is the runtime snapshot carried in a worker hello or heartbeat.
// Generation is a process fence: a smaller generation can never overwrite a
// newer runtime observation in the registry.
type Runtime struct {
	ID           uuid.UUID
	WorkerID     uuid.UUID
	ProfileID    string
	Name         string
	Generation   int64
	PID          *int64
	State        string
	CodexVersion string
	DefaultCWD   string
	Metadata     json.RawMessage
}

// Heartbeat records liveness under an authenticated connection lease.
type Heartbeat struct {
	WorkerID     uuid.UUID
	ConnectionID uuid.UUID
	Metadata     json.RawMessage
	Runtimes     []Runtime
}

// BindConnection makes connectionID the authoritative connection for workerID.
// A new authenticated websocket intentionally fences an older websocket.
func (s *Store) BindConnection(ctx context.Context, workerID, connectionID uuid.UUID) error {
	if workerID == uuid.Nil || connectionID == uuid.Nil {
		return errors.New("registry: worker and connection IDs are required")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE workers
        SET connection_id = $2, connected_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), last_seen_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
            connectivity = 'online', updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE worker_id = $1 AND enabled = TRUE`, workerID, connectionID)
	if err != nil {
		return fmt.Errorf("registry: bind connection: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrInvalidToken
	}
	s.notifySessionActivity()
	return nil
}

// RegisterConnection authenticates a worker token, records the actual hello
// host metadata, fences prior connections, and persists its runtime snapshot in
// one transaction. This avoids a token-rotation race between authentication and
// connection binding.
func (s *Store) RegisterConnection(ctx context.Context, token string, connectionID uuid.UUID, hello protocol.Hello) (Worker, error) {
	workerID, err := uuid.Parse(hello.WorkerID)
	if err != nil || connectionID == uuid.Nil || token == "" ||
		strings.TrimSpace(hello.WorkerName) == "" || strings.TrimSpace(hello.OS) == "" || strings.TrimSpace(hello.Arch) == "" {
		return Worker{}, ErrInvalidToken
	}
	runtimes := make([]Runtime, 0, len(hello.Runtimes))
	for _, snapshot := range hello.Runtimes {
		runtime, err := runtimeFromProtocol(workerID, snapshot)
		if err != nil {
			return Worker{}, fmt.Errorf("registry: invalid hello runtime: %w", err)
		}
		runtimes = append(runtimes, runtime)
	}
	digest := sha256Token(token)
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return Worker{}, fmt.Errorf("registry: begin connection registration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	helloMetadata, err := json.Marshal(struct {
		ProtocolMin        int  `json:"protocol_min"`
		ProtocolMax        int  `json:"protocol_max"`
		SupportsImageInput bool `json:"supports_image_input,omitempty"`
	}{hello.ProtocolMin, hello.ProtocolMax, hello.SupportsImageInput})
	if err != nil {
		return Worker{}, fmt.Errorf("registry: marshal hello metadata: %w", err)
	}
	var storedHash []byte
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT auth_token_hash, enabled FROM workers
        WHERE worker_id = $1`, workerID).Scan(&storedHash, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Worker{}, ErrInvalidToken
	}
	if err != nil {
		return Worker{}, fmt.Errorf("registry: authenticate connection: %w", err)
	}
	if !enabled || subtle.ConstantTimeCompare(storedHash, digest[:]) != 1 {
		return Worker{}, ErrInvalidToken
	}
	var worker Worker
	err = tx.QueryRow(ctx, `UPDATE workers
        SET name = $2, hostname = NULLIF($3, ''), os = $4, arch = $5,
            worker_version = NULLIF($6, ''), connection_id = $7,
            connected_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), last_seen_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), connectivity = 'online',
            heartbeat_metadata = $8, updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE worker_id = $1
        RETURNING worker_id, name, COALESCE(hostname, ''), os, arch,
                  COALESCE(worker_version, ''), enabled, connectivity,
                  connection_id, connected_at, last_seen_at, heartbeat_metadata,
                  created_at, updated_at`,
		workerID, hello.WorkerName, hello.Hostname, hello.OS, hello.Arch,
		hello.WorkerVersion, connectionID, string(helloMetadata)).Scan(workerScanTargets(&worker)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Worker{}, ErrInvalidToken
	}
	if err != nil {
		return Worker{}, fmt.Errorf("registry: register connection: %w", err)
	}
	for _, runtime := range runtimes {
		if err := upsertRuntime(ctx, tx, workerID, runtime); err != nil {
			return Worker{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Worker{}, fmt.Errorf("registry: commit connection registration: %w", err)
	}
	s.notifySessionActivity()
	return worker, nil
}

// CheckConnection confirms that a worker is still enabled and that connectionID
// has not been replaced or fenced by token rotation or revocation.
func (s *Store) CheckConnection(ctx context.Context, workerID, connectionID uuid.UUID) error {
	var current bool
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(enabled AND connection_id = $2, FALSE)
        FROM workers WHERE worker_id = $1`, workerID, connectionID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConnectionFenced
	}
	if err != nil {
		return fmt.Errorf("registry: check connection: %w", err)
	}
	if !current {
		return ErrConnectionFenced
	}
	return nil
}

// Disconnect marks the worker unreachable only if the caller is still the
// current connection. Socket loss cannot prove that a host is offline, and a
// stale websocket cannot change a newer connection.
func (s *Store) Disconnect(ctx context.Context, workerID, connectionID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx, `UPDATE workers
        SET connectivity = 'unreachable', connection_id = NULL, connected_at = NULL,
            updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE worker_id = $1 AND connection_id = $2`, workerID, connectionID)
	if err != nil {
		return fmt.Errorf("registry: disconnect worker: %w", err)
	}
	// A local admin process may already have revoked/fenced this connection.
	// Its database change still needs to reach viewers in this gateway process.
	s.notifySessionActivity()
	if ct.RowsAffected() == 0 {
		return ErrConnectionFenced
	}
	return nil
}

// MarkUnreachable marks only worker connectivity after a heartbeat timeout. It
// deliberately never changes session state, because connectivity is separate
// from Codex execution state.
func (s *Store) MarkUnreachable(ctx context.Context, threshold time.Duration) (int64, error) {
	if threshold <= 0 {
		return 0, errors.New("registry: unreachable threshold must be positive")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE workers
        SET connectivity = 'unreachable', updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE enabled = TRUE AND connectivity = 'online'
          AND last_seen_at < $1`, time.Now().UTC().Add(-threshold))
	if err != nil {
		return 0, fmt.Errorf("registry: mark unreachable workers: %w", err)
	}
	if ct.RowsAffected() > 0 {
		s.notifySessionActivity()
	}
	return ct.RowsAffected(), nil
}

// RecordHeartbeat updates worker liveness and runtime snapshots atomically.
// Runtime rows are only changed when this is the current worker connection and
// their generation is at least the generation already stored.
func (s *Store) RecordHeartbeat(ctx context.Context, heartbeat Heartbeat) error {
	if heartbeat.WorkerID == uuid.Nil || heartbeat.ConnectionID == uuid.Nil {
		return errors.New("registry: worker and connection IDs are required")
	}
	if len(heartbeat.Metadata) == 0 {
		heartbeat.Metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(heartbeat.Metadata) {
		return errors.New("registry: heartbeat metadata must be valid JSON")
	}
	for _, runtime := range heartbeat.Runtimes {
		if err := validateRuntime(heartbeat.WorkerID, runtime); err != nil {
			return err
		}
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin heartbeat: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	activityChanged, err := s.heartbeatActivityChanged(ctx, tx, heartbeat)
	if err != nil {
		return err
	}
	ct, err := tx.Exec(ctx, `UPDATE workers
        SET last_seen_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), connectivity = 'online', heartbeat_metadata = $3,
            updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE worker_id = $1 AND connection_id = $2 AND enabled = TRUE`,
		heartbeat.WorkerID, heartbeat.ConnectionID, heartbeat.Metadata)
	if err != nil {
		return fmt.Errorf("registry: update heartbeat: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrConnectionFenced
	}
	for _, runtime := range heartbeat.Runtimes {
		if err := upsertRuntime(ctx, tx, heartbeat.WorkerID, runtime); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("registry: commit heartbeat: %w", err)
	}
	if activityChanged {
		s.notifySessionActivity()
	}
	return nil
}

func upsertRuntime(ctx context.Context, tx *dbTx, workerID uuid.UUID, runtime Runtime) error {
	metadata := runtime.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(ctx, `INSERT INTO runtimes
        (runtime_id, worker_id, profile_id, name, generation, pid, state,
         codex_version, default_cwd, started_at, stopped_at, last_seen_at, metadata)
        VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''),
            CASE WHEN $7 IN ('starting', 'running', 'degraded') THEN (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') END,
            CASE WHEN $7 = 'stopped' THEN (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') END, (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), $10)
        ON CONFLICT (runtime_id) DO UPDATE SET
            profile_id = EXCLUDED.profile_id, name = EXCLUDED.name,
            generation = EXCLUDED.generation, pid = EXCLUDED.pid,
            state = EXCLUDED.state, codex_version = EXCLUDED.codex_version,
            default_cwd = EXCLUDED.default_cwd,
            started_at = CASE WHEN EXCLUDED.generation > runtimes.generation
                AND EXCLUDED.state IN ('starting', 'running', 'degraded') THEN (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
                ELSE runtimes.started_at END,
            stopped_at = CASE WHEN EXCLUDED.state = 'stopped' THEN (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
                ELSE runtimes.stopped_at END,
            last_seen_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), metadata = EXCLUDED.metadata
        WHERE runtimes.worker_id = EXCLUDED.worker_id
          AND EXCLUDED.generation >= runtimes.generation`,
		runtime.ID, workerID, runtime.ProfileID, runtime.Name, runtime.Generation,
		runtime.PID, runtime.State, runtime.CodexVersion, runtime.DefaultCWD, metadata)
	if err != nil {
		return fmt.Errorf("registry: upsert runtime %s: %w", runtime.ID, err)
	}
	return nil
}

func runtimeFromProtocol(workerID uuid.UUID, source protocol.Runtime) (Runtime, error) {
	id, err := uuid.Parse(source.ID)
	if err != nil {
		return Runtime{}, errors.New("invalid runtime ID")
	}
	if source.WorkerID != "" && source.WorkerID != workerID.String() {
		return Runtime{}, errors.New("runtime worker ID does not match hello")
	}
	if source.Generation > uint64(1<<63-1) {
		return Runtime{}, errors.New("runtime generation exceeds database range")
	}
	var pid *int64
	if source.PID != 0 {
		value := int64(source.PID)
		pid = &value
	}
	return Runtime{ID: id, WorkerID: workerID, ProfileID: source.ProfileID,
		Name: source.Name, Generation: int64(source.Generation), PID: pid,
		State: source.State, CodexVersion: source.CodexVersion,
		DefaultCWD: source.DefaultCWD}, nil
}

func sha256Token(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

func validateRuntime(workerID uuid.UUID, runtime Runtime) error {
	if runtime.ID == uuid.Nil || runtime.Generation < 0 || strings.TrimSpace(runtime.ProfileID) == "" ||
		strings.TrimSpace(runtime.Name) == "" || !validRuntimeState(runtime.State) {
		return errors.New("registry: invalid runtime heartbeat")
	}
	if runtime.WorkerID != uuid.Nil && runtime.WorkerID != workerID {
		return errors.New("registry: runtime worker does not match heartbeat worker")
	}
	if len(runtime.Metadata) > 0 && !json.Valid(runtime.Metadata) {
		return errors.New("registry: runtime metadata must be valid JSON")
	}
	return nil
}

func validRuntimeState(state string) bool {
	switch state {
	case "stopped", "starting", "running", "degraded", "failed":
		return true
	default:
		return false
	}
}

// EventWatermark returns the highest event sequence durably acknowledged for a
// worker. Worker creation initializes it to zero.
func (s *Store) EventWatermark(ctx context.Context, workerID uuid.UUID) (int64, error) {
	var watermark int64
	err := s.pool.QueryRow(ctx, "SELECT event_seq FROM worker_event_watermarks WHERE worker_id = $1", workerID).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrWorkerNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("registry: read event watermark: %w", err)
	}
	return watermark, nil
}

// AdvanceEventWatermark records a monotonically increasing durable event
// acknowledgement under the current connection fence. Event ingestion should
// call this inside the same transaction that persists its durable event; this
// standalone method is provided for callers that only advance an already
// durable sequence.
func (s *Store) AdvanceEventWatermark(ctx context.Context, workerID, connectionID uuid.UUID, eventSeq int64) error {
	if workerID == uuid.Nil || connectionID == uuid.Nil || eventSeq < 0 {
		return errors.New("registry: invalid event watermark")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE worker_event_watermarks AS watermark
        SET event_seq = max(watermark.event_seq, $3), updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE watermark.worker_id = $1
          AND EXISTS (
              SELECT 1 FROM workers
              WHERE worker_id = $1 AND connection_id = $2 AND enabled = TRUE
          )`, workerID, connectionID, eventSeq)
	if err != nil {
		return fmt.Errorf("registry: advance event watermark: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrConnectionFenced
	}
	return nil
}
