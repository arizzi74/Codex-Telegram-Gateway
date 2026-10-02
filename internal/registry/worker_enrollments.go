package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	WorkerEnrollmentTTL          = protocol.WorkerEnrollmentLifetime
	workerEnrollmentCodeAlphabet = protocol.WorkerEnrollmentCodeAlphabet
	workerEnrollmentCodeLength   = protocol.WorkerEnrollmentCodeLength
)

var (
	// ErrWorkerEnrollmentGone treats unknown, expired, revoked, and used codes alike.
	ErrWorkerEnrollmentGone  = errors.New("registry: enrollment unavailable")
	ErrWorkerEnrollmentInput = errors.New("registry: invalid enrollment request")
)

// WorkerEnrollment contains only non-secret durable enrollment metadata.
type WorkerEnrollment struct {
	ID            uuid.UUID
	ServiceAccess string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type RedeemWorkerEnrollmentInput struct {
	Code string
	Name string
	OS   string
	Arch string
}

// RedeemedWorkerEnrollment is returned once, after its transaction commits.
// Token is never stored in the registry and must never be logged.
type RedeemedWorkerEnrollment struct {
	WorkerID      uuid.UUID
	Token         string
	ServiceAccess string
}

// CreateWorkerEnrollment creates a ten-minute enrollment without creating a
// worker. The caller receives the code once; only its hash is persisted.
func (s *Store) CreateWorkerEnrollment(ctx context.Context, serviceAccess string) (WorkerEnrollment, string, error) {
	return s.createWorkerEnrollment(ctx, serviceAccess, time.Now().UTC())
}

func (s *Store) createWorkerEnrollment(ctx context.Context, serviceAccess string, now time.Time) (WorkerEnrollment, string, error) {
	serviceAccess, err := protocol.NormalizeWorkerEnrollmentServiceAccess(serviceAccess)
	if err != nil {
		return WorkerEnrollment{}, "", ErrWorkerEnrollmentInput
	}
	var random [workerEnrollmentCodeLength]byte
	if _, err := rand.Read(random[:]); err != nil {
		return WorkerEnrollment{}, "", errors.New("registry: generate enrollment code")
	}
	var code [workerEnrollmentCodeLength]byte
	for i, value := range random {
		// The alphabet has 32 symbols, so all eight-bit values map uniformly.
		code[i] = workerEnrollmentCodeAlphabet[value&31]
	}
	enrollment := WorkerEnrollment{ID: uuid.New(), ServiceAccess: serviceAccess, CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(WorkerEnrollmentTTL)}
	digest := sha256.Sum256(code[:])
	if _, err := s.pool.Exec(ctx, `INSERT INTO worker_enrollments
		(enrollment_id, code_hash, service_access, created_at, expires_at) VALUES ($1,$2,$3,$4,$5)`,
		enrollment.ID, digest[:], enrollment.ServiceAccess, enrollment.CreatedAt, enrollment.ExpiresAt); err != nil {
		return WorkerEnrollment{}, "", fmt.Errorf("registry: create enrollment: %w", err)
	}
	return enrollment, string(code[:]), nil
}

// RedeemWorkerEnrollment checks the capability before generating a bearer
// token, then consumes it together with the worker and event watermark. An
// IMMEDIATE transaction also serializes independent Store instances.
func (s *Store) RedeemWorkerEnrollment(ctx context.Context, in RedeemWorkerEnrollmentInput) (RedeemedWorkerEnrollment, error) {
	return s.redeemWorkerEnrollment(ctx, in, time.Now, auth.GenerateWorkerToken)
}

func (s *Store) redeemWorkerEnrollment(ctx context.Context, in RedeemWorkerEnrollmentInput, clock func() time.Time, tokenGenerator func() (string, error)) (RedeemedWorkerEnrollment, error) {
	code, name, err := validateWorkerEnrollmentInput(in)
	if err != nil {
		return RedeemedWorkerEnrollment{}, err
	}
	digest := sha256.Sum256([]byte(code))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RedeemedWorkerEnrollment{}, fmt.Errorf("registry: begin enrollment redeem: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Capture the time after acquiring the writer reservation; waiting for a
	// concurrent transaction must not extend the capability's validity.
	now := clock().UTC()
	var id uuid.UUID
	var access string
	err = tx.QueryRow(ctx, `SELECT enrollment_id, service_access FROM worker_enrollments
		WHERE code_hash=$1 AND expires_at>$2 AND consumed_at IS NULL AND revoked_at IS NULL`, digest[:], now).Scan(&id, &access)
	if errors.Is(err, sql.ErrNoRows) {
		return RedeemedWorkerEnrollment{}, ErrWorkerEnrollmentGone
	}
	if err != nil {
		return RedeemedWorkerEnrollment{}, fmt.Errorf("registry: check enrollment: %w", err)
	}
	token, err := tokenGenerator()
	if err != nil {
		return RedeemedWorkerEnrollment{}, errors.New("registry: generate worker credentials")
	}
	workerID := uuid.New()
	if _, err = tx.Exec(ctx, `INSERT INTO workers (worker_id, name, os, arch, auth_token_hash)
		VALUES ($1,$2,$3,$4,$5)`, workerID, name, in.OS, in.Arch, auth.HashWorkerToken(token)); err != nil {
		return RedeemedWorkerEnrollment{}, fmt.Errorf("registry: create enrolled worker: %w", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO worker_event_watermarks (worker_id) VALUES ($1)`, workerID); err != nil {
		return RedeemedWorkerEnrollment{}, fmt.Errorf("registry: create enrolled worker watermark: %w", err)
	}
	changed, err := tx.Exec(ctx, `UPDATE worker_enrollments SET consumed_at=$2, worker_id=$3
		WHERE enrollment_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at>$2`, id, now, workerID)
	if err != nil {
		return RedeemedWorkerEnrollment{}, fmt.Errorf("registry: consume enrollment: %w", err)
	}
	if changed.RowsAffected() != 1 {
		return RedeemedWorkerEnrollment{}, ErrWorkerEnrollmentGone
	}
	if err = tx.Commit(ctx); err != nil {
		return RedeemedWorkerEnrollment{}, fmt.Errorf("registry: commit enrollment redeem: %w", err)
	}
	return RedeemedWorkerEnrollment{WorkerID: workerID, Token: token, ServiceAccess: access}, nil
}

func validateWorkerEnrollmentInput(in RedeemWorkerEnrollmentInput) (string, string, error) {
	code, err := protocol.NormalizeWorkerEnrollmentCode(in.Code)
	name := strings.TrimSpace(in.Name)
	if err != nil || protocol.ValidateWorkerEnrollmentName(in.Name) != nil {
		return "", "", ErrWorkerEnrollmentInput
	}
	if (in.OS != "linux" && in.OS != "darwin") || (in.Arch != "amd64" && in.Arch != "arm64") {
		return "", "", ErrWorkerEnrollmentInput
	}
	return code, name, nil
}

// RevokeWorkerEnrollment invalidates an unused link. Repeating a revocation is
// harmless; consumed links cannot affect their already enrolled worker.
func (s *Store) RevokeWorkerEnrollment(ctx context.Context, id uuid.UUID) error {
	changed, err := s.pool.Exec(ctx, `UPDATE worker_enrollments SET revoked_at=COALESCE(revoked_at,$2)
		WHERE enrollment_id=$1 AND consumed_at IS NULL`, id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("registry: revoke enrollment: %w", err)
	}
	if changed.RowsAffected() != 1 {
		return ErrWorkerEnrollmentGone
	}
	return nil
}
