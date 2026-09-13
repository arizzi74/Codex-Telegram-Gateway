// Package registry persists the gateway's relational control-plane state.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrWorkerNotFound identifies an enrollment record that does not exist.
	ErrWorkerNotFound = errors.New("registry: worker not found")
	// ErrInvalidToken deliberately does not distinguish an unknown, revoked, or
	// disabled worker to callers authenticating a websocket.
	ErrInvalidToken = errors.New("registry: invalid worker token")
	// ErrConnectionFenced means a newer connection replaced the caller.
	ErrConnectionFenced = errors.New("registry: connection is no longer current")
)

// Store is safe for concurrent use. The caller owns its lifecycle.
type Store struct {
	pool *pgxpool.Pool
}

// Open creates a Store backed by url. It does not run migrations or contact
// PostgreSQL; use Migrate and Ping during gateway startup/readiness checks.
func Open(ctx context.Context, url string) (*Store, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("registry: database URL is required")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("registry: open pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases database connections. It is safe to call on a nil Store.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies that PostgreSQL is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("registry: store is closed")
	}
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("registry: ping: %w", err)
	}
	return nil
}

// Migrate applies embedded migrations exactly once. The transaction advisory
// lock serializes concurrent gateway starts against the same database.
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("registry: store is closed")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(731902841)"); err != nil {
		return fmt.Errorf("registry: lock migrations: %w", err)
	}
	// The migration table is needed before discovering prior versions.
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version BIGINT PRIMARY KEY,
        checksum BYTEA NOT NULL,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`); err != nil {
		return fmt.Errorf("registry: create migration ledger: %w", err)
	}
	rows, err := tx.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("registry: read migration ledger: %w", err)
	}
	applied := make(map[int64][]byte)
	for rows.Next() {
		var version int64
		var checksum []byte
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("registry: scan migration ledger: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("registry: read migration ledger: %w", err)
	}
	rows.Close()

	files := append([]string(nil), migrations.Files...)
	sort.Strings(files)
	for _, file := range files {
		version, err := migrationVersion(file)
		if err != nil {
			return err
		}
		sql, err := migrations.SQL.ReadFile(file)
		if err != nil {
			return fmt.Errorf("registry: read embedded migration %q: %w", file, err)
		}
		checksum := sha256.Sum256(sql)
		if prior, ok := applied[version]; ok {
			if !bytes.Equal(prior, checksum[:]) {
				return fmt.Errorf("registry: migration %s checksum does not match applied schema", file)
			}
			continue
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("registry: apply migration %s: %w", file, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)", version, checksum[:]); err != nil {
			return fmt.Errorf("registry: record migration %s: %w", file, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("registry: commit migrations: %w", err)
	}
	return nil
}

// CheckMigrations verifies that the database has exactly the embedded schema
// history, including checksums. It is used by readiness without mutating state.
func (s *Store) CheckMigrations(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("registry: store is closed")
	}
	rows, err := s.pool.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("registry: read migration ledger: %w", err)
	}
	defer rows.Close()
	applied := make(map[int64][]byte)
	for rows.Next() {
		var version int64
		var checksum []byte
		if err := rows.Scan(&version, &checksum); err != nil {
			return fmt.Errorf("registry: scan migration ledger: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("registry: read migration ledger: %w", err)
	}
	if len(applied) != len(migrations.Files) {
		return errors.New("registry: migration set is incomplete or unknown")
	}
	for _, file := range migrations.Files {
		version, err := migrationVersion(file)
		if err != nil {
			return err
		}
		sql, err := migrations.SQL.ReadFile(file)
		if err != nil {
			return fmt.Errorf("registry: read embedded migration %q: %w", file, err)
		}
		digest := sha256.Sum256(sql)
		actual, ok := applied[version]
		if !ok || !bytes.Equal(actual, digest[:]) {
			return fmt.Errorf("registry: migration %s is absent or has an invalid checksum", file)
		}
	}
	return nil
}

func migrationVersion(file string) (int64, error) {
	prefix, _, ok := strings.Cut(file, "_")
	if !ok {
		return 0, fmt.Errorf("registry: migration %q has no numeric version", file)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("registry: invalid migration version in %q", file)
	}
	return version, nil
}

// Worker is the non-secret enrollment and liveness record.
type Worker struct {
	ID                uuid.UUID
	Name              string
	Hostname          string
	OS                string
	Arch              string
	Version           string
	Enabled           bool
	Connectivity      string
	ConnectionID      *uuid.UUID
	ConnectedAt       *time.Time
	LastSeenAt        *time.Time
	HeartbeatMetadata json.RawMessage
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreateWorkerInput contains a pre-hashed enrollment token. The registry never
// generates or returns plaintext enrollment tokens.
type CreateWorkerInput struct {
	ID        uuid.UUID
	Name      string
	Hostname  string
	OS        string
	Arch      string
	Version   string
	TokenHash []byte
}

// CreateWorker creates an enabled worker record and its event watermark.
func (s *Store) CreateWorker(ctx context.Context, in CreateWorkerInput) (Worker, error) {
	if err := validateWorkerInput(in); err != nil {
		return Worker{}, err
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Worker{}, fmt.Errorf("registry: begin worker create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var worker Worker
	err = tx.QueryRow(ctx, `INSERT INTO workers
        (worker_id, name, hostname, os, arch, worker_version, auth_token_hash)
        VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7)
        RETURNING worker_id, name, COALESCE(hostname, ''), os, arch,
                  COALESCE(worker_version, ''), enabled, connectivity,
                  connection_id, connected_at, last_seen_at, heartbeat_metadata,
                  created_at, updated_at`,
		in.ID, in.Name, in.Hostname, in.OS, in.Arch, in.Version, in.TokenHash).Scan(workerScanTargets(&worker)...)
	if err != nil {
		return Worker{}, fmt.Errorf("registry: create worker: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO worker_event_watermarks (worker_id) VALUES ($1)", in.ID); err != nil {
		return Worker{}, fmt.Errorf("registry: create event watermark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Worker{}, fmt.Errorf("registry: commit worker create: %w", err)
	}
	return worker, nil
}

func validateWorkerInput(in CreateWorkerInput) error {
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.OS) == "" || strings.TrimSpace(in.Arch) == "" {
		return errors.New("registry: worker name, OS, and architecture are required")
	}
	if len(in.TokenHash) != sha256.Size {
		return errors.New("registry: worker token hash must be a SHA-256 digest")
	}
	return nil
}

// ListWorkers returns worker records ordered by creation time. Token hashes are
// intentionally never selected.
func (s *Store) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.pool.Query(ctx, `SELECT worker_id, name, COALESCE(hostname, ''), os, arch,
        COALESCE(worker_version, ''), enabled, connectivity, connection_id,
        connected_at, last_seen_at, heartbeat_metadata, created_at, updated_at
        FROM workers ORDER BY created_at, worker_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list workers: %w", err)
	}
	defer rows.Close()
	workers := make([]Worker, 0)
	for rows.Next() {
		var worker Worker
		if err := rows.Scan(workerScanTargets(&worker)...); err != nil {
			return nil, fmt.Errorf("registry: scan worker: %w", err)
		}
		workers = append(workers, worker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list workers: %w", err)
	}
	return workers, nil
}

func workerScanTargets(w *Worker) []any {
	return []any{&w.ID, &w.Name, &w.Hostname, &w.OS, &w.Arch, &w.Version,
		&w.Enabled, &w.Connectivity, &w.ConnectionID, &w.ConnectedAt, &w.LastSeenAt,
		&w.HeartbeatMetadata, &w.CreatedAt, &w.UpdatedAt}
}

// RevokeWorker disables a worker and fences any active connection.
func (s *Store) RevokeWorker(ctx context.Context, workerID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx, `UPDATE workers
        SET enabled = FALSE, connectivity = 'disabled', connection_id = NULL,
            connected_at = NULL, updated_at = now()
        WHERE worker_id = $1`, workerID)
	if err != nil {
		return fmt.Errorf("registry: revoke worker: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// RotateWorkerToken replaces an enrollment token hash and fences active
// connections. The caller must retain or display plaintext tokens itself.
func (s *Store) RotateWorkerToken(ctx context.Context, workerID uuid.UUID, tokenHash []byte) error {
	if len(tokenHash) != sha256.Size {
		return errors.New("registry: worker token hash must be a SHA-256 digest")
	}
	ct, err := s.pool.Exec(ctx, `UPDATE workers
        SET auth_token_hash = $2, connection_id = NULL, connected_at = NULL,
            connectivity = CASE WHEN enabled THEN 'offline' ELSE 'disabled' END,
            updated_at = now()
        WHERE worker_id = $1`, workerID, tokenHash)
	if err != nil {
		return fmt.Errorf("registry: rotate worker token: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// AuthenticateWorker performs a SHA-256 lookup of plaintext token. It returns
// only public worker metadata and never exposes the stored token digest.
func (s *Store) AuthenticateWorker(ctx context.Context, token string) (Worker, error) {
	if token == "" {
		return Worker{}, ErrInvalidToken
	}
	digest := sha256.Sum256([]byte(token))
	var worker Worker
	var storedHash []byte
	err := s.pool.QueryRow(ctx, `SELECT auth_token_hash, worker_id, name, COALESCE(hostname, ''), os, arch,
        COALESCE(worker_version, ''), enabled, connectivity, connection_id,
        connected_at, last_seen_at, heartbeat_metadata, created_at, updated_at
        FROM workers WHERE auth_token_hash = $1 AND enabled = TRUE`, digest[:]).Scan(append([]any{&storedHash}, workerScanTargets(&worker)...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Worker{}, ErrInvalidToken
	}
	if err != nil {
		return Worker{}, fmt.Errorf("registry: authenticate worker: %w", err)
	}
	if subtle.ConstantTimeCompare(storedHash, digest[:]) != 1 {
		return Worker{}, ErrInvalidToken
	}
	return worker, nil
}
