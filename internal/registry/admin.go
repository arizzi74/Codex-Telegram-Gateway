package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrAdminBootstrapInvalid = errors.New("registry: invalid or expired admin bootstrap token")
	ErrAdminCeremonyInvalid  = errors.New("registry: invalid or expired admin ceremony")
	ErrAdminSessionInvalid   = errors.New("registry: invalid or expired admin session")
	ErrAdminCredentialGone   = errors.New("registry: admin credential not found or revoked")
)

const (
	adminBootstrapTTL = 15 * time.Minute
	adminSessionTTL   = 8 * time.Hour
)

// AdminCredential is the non-secret record needed to verify a WebAuthn
// assertion. CredentialJSON is the library's complete durable credential.
type AdminCredential struct {
	ID             []byte
	UserHandle     []byte
	CredentialJSON json.RawMessage
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	RevokedAt      *time.Time
}

// AdminCeremony is a short-lived server-side WebAuthn session. BindingHash
// binds it to the HttpOnly ceremony cookie without persisting its plaintext.
type AdminCeremony struct {
	ID          uuid.UUID
	Purpose     string
	UserHandle  []byte
	SessionData json.RawMessage
	Binding     string
}

// BootstrapAdmin creates the sole, one-use bootstrap secret. Calling it again
// replaces any unconsumed prior secret. The plaintext is intentionally only
// returned here for the local gateway CLI to display once.
func (s *Store) BootstrapAdmin(ctx context.Context) (string, error) {
	token, err := randomSecret(32)
	if err != nil {
		return "", err
	}
	hash := hashSecret(token)
	_, err = s.pool.Exec(ctx, `INSERT INTO admin_bootstrap (bootstrap_id, token_hash, expires_at, consumed_at, created_at)
        VALUES (1, $1, (strftime('%Y-%m-%dT%H:%M:%f','now','+15 minutes') || '000000Z'), NULL, (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))
        ON CONFLICT (bootstrap_id) DO UPDATE SET token_hash = EXCLUDED.token_hash,
          expires_at = EXCLUDED.expires_at, consumed_at = NULL, created_at = EXCLUDED.created_at`, hash)
	if err != nil {
		return "", fmt.Errorf("registry: create admin bootstrap: %w", err)
	}
	return token, nil
}

// CheckAdminBootstrap verifies that a bootstrap secret still exists without
// consuming it. Completion performs the authoritative locked consume.
func (s *Store) CheckAdminBootstrap(ctx context.Context, token string) error {
	var actual []byte
	err := s.pool.QueryRow(ctx, `SELECT token_hash FROM admin_bootstrap WHERE bootstrap_id=1 AND consumed_at IS NULL AND expires_at>(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')`).Scan(&actual)
	if err != nil || subtle.ConstantTimeCompare(hashSecret(token), actual) != 1 {
		return ErrAdminBootstrapInvalid
	}
	return nil
}

// NewAdminCeremony saves a short-lived WebAuthn SessionData and returns a
// random cookie binding. Session data is consumed exactly once at completion.
func (s *Store) NewAdminCeremony(ctx context.Context, purpose string, userHandle []byte, sessionData json.RawMessage) (AdminCeremony, error) {
	if purpose != "registration" && purpose != "authentication" || len(sessionData) == 0 {
		return AdminCeremony{}, ErrAdminCeremonyInvalid
	}
	binding, err := randomSecret(32)
	if err != nil {
		return AdminCeremony{}, err
	}
	var sd struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(sessionData, &sd) != nil || sd.Challenge == "" {
		return AdminCeremony{}, ErrAdminCeremonyInvalid
	}
	challenge, err := base64.RawURLEncoding.DecodeString(sd.Challenge)
	if err != nil {
		return AdminCeremony{}, ErrAdminCeremonyInvalid
	}
	id := uuid.New()
	_, err = s.pool.Exec(ctx, `INSERT INTO admin_challenges
        (challenge_id, purpose, challenge_hash, user_handle, expires_at, session_data, ceremony_binding_hash)
        VALUES ($1, $2, $3, $4, (strftime('%Y-%m-%dT%H:%M:%f','now','+5 minutes') || '000000Z'), $5, $6)`, id, purpose, sha256Bytes(challenge), nullableBytes(userHandle), sessionData, hashSecret(binding))
	if err != nil {
		return AdminCeremony{}, fmt.Errorf("registry: save admin ceremony: %w", err)
	}
	return AdminCeremony{ID: id, Purpose: purpose, UserHandle: userHandle, SessionData: sessionData, Binding: binding}, nil
}

// ReadAdminCeremony reads a still-live ceremony. The completion methods below
// claim it in their credential/session transaction, preventing a valid result
// from being separated from its one-use durable replay protection.
func (s *Store) ReadAdminCeremony(ctx context.Context, id uuid.UUID, purpose, binding string) (AdminCeremony, error) {
	var c AdminCeremony
	var bind []byte
	err := s.pool.QueryRow(ctx, `SELECT purpose, COALESCE(user_handle, X''), session_data, ceremony_binding_hash
        FROM admin_challenges WHERE challenge_id=$1 AND consumed_at IS NULL AND expires_at > (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')`, id).Scan(&c.Purpose, &c.UserHandle, &c.SessionData, &bind)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AdminCeremony{}, ErrAdminCeremonyInvalid
		}
		return AdminCeremony{}, fmt.Errorf("registry: read admin ceremony: %w", err)
	}
	if c.Purpose != purpose || subtle.ConstantTimeCompare(hashSecret(binding), bind) != 1 {
		return AdminCeremony{}, ErrAdminCeremonyInvalid
	}
	c.ID = id
	return c, nil
}

// CompleteBootstrapCredential atomically consumes the bootstrap secret and
// creates the singleton user handle plus its initial credential.
func (s *Store) CompleteBootstrapCredential(ctx context.Context, ceremonyID uuid.UUID, binding, bootstrap string, credential AdminCredential) error {
	if len(credential.ID) == 0 || len(credential.UserHandle) < 16 || len(credential.CredentialJSON) == 0 {
		return ErrAdminBootstrapInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("registry: begin bootstrap complete: %w", err)
	}
	defer tx.Rollback(ctx)
	var actual []byte
	err = tx.QueryRow(ctx, `SELECT token_hash FROM admin_bootstrap WHERE bootstrap_id=1 AND consumed_at IS NULL AND expires_at>(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')`).Scan(&actual)
	if err != nil || subtle.ConstantTimeCompare(hashSecret(bootstrap), actual) != 1 {
		return ErrAdminBootstrapInvalid
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM admin_users)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrAdminBootstrapInvalid
	}
	if err = claimAdminCeremony(ctx, tx, ceremonyID, "registration", binding); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_users (user_id,user_handle) VALUES (1,$1)`, credential.UserHandle); err != nil {
		return fmt.Errorf("registry: create admin user: %w", err)
	}
	if err = insertAdminCredential(ctx, tx, credential); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_bootstrap SET consumed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE bootstrap_id=1 AND consumed_at IS NULL`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CompleteAdminCredential atomically consumes an add-passkey ceremony and
// persists the new credential. Authorization of the existing session happens
// in the HTTP layer before this call.
func (s *Store) CompleteAdminCredential(ctx context.Context, ceremonyID uuid.UUID, binding string, credential AdminCredential) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var handle []byte
	if err = tx.QueryRow(ctx, `SELECT user_handle FROM admin_users WHERE user_id=1`).Scan(&handle); err != nil || subtle.ConstantTimeCompare(handle, credential.UserHandle) != 1 {
		return ErrAdminCredentialGone
	}
	if err = claimAdminCeremony(ctx, tx, ceremonyID, "registration", binding); err != nil {
		return err
	}
	if err = insertAdminCredential(ctx, tx, credential); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AddAdminCredential(ctx context.Context, credential AdminCredential) error {
	if len(credential.ID) == 0 || len(credential.UserHandle) < 16 || len(credential.CredentialJSON) == 0 {
		return ErrAdminCredentialGone
	}
	var handle []byte
	if err := s.pool.QueryRow(ctx, `SELECT user_handle FROM admin_users WHERE user_id=1`).Scan(&handle); err != nil || subtle.ConstantTimeCompare(handle, credential.UserHandle) != 1 {
		return ErrAdminCredentialGone
	}
	return insertAdminCredential(ctx, s.pool, credential)
}

// CompleteAdminLogin atomically consumes an authentication ceremony, writes
// the advanced signature counter, and issues the one-time plaintext session.
func (s *Store) CompleteAdminLogin(ctx context.Context, ceremonyID uuid.UUID, binding string, credential AdminCredential) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var record struct {
		Authenticator struct {
			SignCount uint32 `json:"signCount"`
		} `json:"authenticator"`
	}
	if json.Unmarshal(credential.CredentialJSON, &record) != nil {
		return "", ErrAdminCredentialGone
	}
	ct, err := tx.Exec(ctx, `UPDATE admin_credentials SET credential_json=$2,sign_count=$3,last_used_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE credential_id=$1 AND revoked_at IS NULL`, credential.ID, credential.CredentialJSON, record.Authenticator.SignCount)
	if err != nil {
		return "", err
	}
	if ct.RowsAffected() != 1 {
		return "", ErrAdminCredentialGone
	}
	if err = claimAdminCeremony(ctx, tx, ceremonyID, "authentication", binding); err != nil {
		return "", err
	}
	token, err := randomSecret(32)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_sessions(session_id,credential_id,token_hash,expires_at) VALUES($1,$2,$3,(strftime('%Y-%m-%dT%H:%M:%f','now','+8 hours') || '000000Z'))`, uuid.New(), credential.ID, hashSecret(token)); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

type adminDB interface {
	Exec(context.Context, string, ...any) (dbCommandTag, error)
}

func claimAdminCeremony(ctx context.Context, tx *dbTx, id uuid.UUID, purpose, binding string) error {
	var got string
	var hash []byte
	err := tx.QueryRow(ctx, `SELECT purpose,ceremony_binding_hash FROM admin_challenges WHERE challenge_id=$1 AND consumed_at IS NULL AND expires_at>(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')`, id).Scan(&got, &hash)
	if err != nil || got != purpose || subtle.ConstantTimeCompare(hash, hashSecret(binding)) != 1 {
		return ErrAdminCeremonyInvalid
	}
	ct, err := tx.Exec(ctx, `UPDATE admin_challenges SET consumed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE challenge_id=$1 AND consumed_at IS NULL`, id)
	if err != nil || ct.RowsAffected() != 1 {
		return ErrAdminCeremonyInvalid
	}
	return nil
}

func insertAdminCredential(ctx context.Context, db adminDB, credential AdminCredential) error {
	_, err := db.Exec(ctx, `INSERT INTO admin_credentials
        (credential_id, user_handle, public_key, sign_count, transports, credential_json)
        VALUES ($1, $2, $3, 0, '[]', $4)`, credential.ID, credential.UserHandle, credential.ID, credential.CredentialJSON)
	if err != nil {
		return fmt.Errorf("registry: save admin credential: %w", err)
	}
	return nil
}

// AdminUser returns the singleton passkey owner and all active credentials.
func (s *Store) AdminUser(ctx context.Context) ([]byte, []AdminCredential, error) {
	var handle []byte
	if err := s.pool.QueryRow(ctx, `SELECT user_handle FROM admin_users WHERE user_id=1`).Scan(&handle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrAdminCredentialGone
		}
		return nil, nil, fmt.Errorf("registry: read admin user: %w", err)
	}
	creds, err := s.AdminCredentials(ctx)
	return handle, creds, err
}

func (s *Store) AdminCredentials(ctx context.Context) ([]AdminCredential, error) {
	rows, err := s.pool.Query(ctx, `SELECT credential_id,user_handle,credential_json,created_at,last_used_at,revoked_at
        FROM admin_credentials ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("registry: list admin credentials: %w", err)
	}
	defer rows.Close()
	var result []AdminCredential
	for rows.Next() {
		var c AdminCredential
		if err = rows.Scan(&c.ID, &c.UserHandle, &c.CredentialJSON, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt); err != nil {
			return nil, fmt.Errorf("registry: scan admin credential: %w", err)
		}
		result = append(result, c)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list admin credentials: %w", err)
	}
	return result, nil
}

func (s *Store) ActiveAdminCredential(ctx context.Context, id []byte) (AdminCredential, error) {
	var c AdminCredential
	err := s.pool.QueryRow(ctx, `SELECT credential_id,user_handle,credential_json,created_at,last_used_at,revoked_at FROM admin_credentials WHERE credential_id=$1 AND revoked_at IS NULL`, id).Scan(&c.ID, &c.UserHandle, &c.CredentialJSON, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminCredential{}, ErrAdminCredentialGone
	}
	if err != nil {
		return AdminCredential{}, fmt.Errorf("registry: read admin credential: %w", err)
	}
	return c, nil
}

func (s *Store) TouchAdminCredential(ctx context.Context, credential AdminCredential) error {
	var record struct {
		Authenticator struct {
			SignCount uint32 `json:"signCount"`
		} `json:"authenticator"`
	}
	if json.Unmarshal(credential.CredentialJSON, &record) != nil {
		return ErrAdminCredentialGone
	}
	ct, err := s.pool.Exec(ctx, `UPDATE admin_credentials SET credential_json=$2,sign_count=$3,last_used_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE credential_id=$1 AND revoked_at IS NULL`, credential.ID, credential.CredentialJSON, record.Authenticator.SignCount)
	if err != nil {
		return fmt.Errorf("registry: update admin credential: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return ErrAdminCredentialGone
	}
	return nil
}

func (s *Store) RevokeAdminCredential(ctx context.Context, id []byte) error {
	// Keep at least one active credential so an authenticated operator cannot
	// accidentally make the passkey-only console permanently inaccessible.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT credential_id FROM admin_credentials WHERE revoked_at IS NULL`)
	if err != nil {
		return err
	}
	n := 0
	for rows.Next() {
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if n <= 1 {
		return errors.New("registry: cannot revoke the final admin credential")
	}
	ct, err := tx.Exec(ctx, `UPDATE admin_credentials SET revoked_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE credential_id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if ct.RowsAffected() != 1 {
		return ErrAdminCredentialGone
	}
	_, err = tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE credential_id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateAdminSession returns a plaintext cookie token exactly once. Only its
// hash is persisted and successful validation updates last_seen_at.
func (s *Store) CreateAdminSession(ctx context.Context, credentialID []byte) (string, error) {
	token, err := randomSecret(32)
	if err != nil {
		return "", err
	}
	id := uuid.New()
	_, err = s.pool.Exec(ctx, `INSERT INTO admin_sessions(session_id,credential_id,token_hash,expires_at) VALUES($1,$2,$3,(strftime('%Y-%m-%dT%H:%M:%f','now','+8 hours') || '000000Z'))`, id, credentialID, hashSecret(token))
	if err != nil {
		return "", fmt.Errorf("registry: create admin session: %w", err)
	}
	return token, nil
}

func (s *Store) ValidateAdminSession(ctx context.Context, token string) (AdminCredential, error) {
	var c AdminCredential
	err := s.pool.QueryRow(ctx, `SELECT c.credential_id,c.user_handle,c.credential_json,c.created_at,c.last_used_at,c.revoked_at
      FROM admin_sessions s JOIN admin_credentials c ON c.credential_id=s.credential_id
      WHERE s.token_hash=$1 AND s.expires_at>(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') AND s.revoked_at IS NULL AND c.revoked_at IS NULL`, hashSecret(token)).Scan(&c.ID, &c.UserHandle, &c.CredentialJSON, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminCredential{}, ErrAdminSessionInvalid
	}
	if err != nil {
		return AdminCredential{}, fmt.Errorf("registry: validate admin session: %w", err)
	}
	_, _ = s.pool.Exec(ctx, `UPDATE admin_sessions SET last_seen_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE token_hash=$1`, hashSecret(token))
	return c, nil
}
func (s *Store) RevokeAdminSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET revoked_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE token_hash=$1 AND revoked_at IS NULL`, hashSecret(token))
	if err != nil {
		return fmt.Errorf("registry: revoke admin session: %w", err)
	}
	return nil
}

// AdminDashboard is intentionally summary-only so the web console never
// exposes worker enrollment secrets or command payloads.
type AdminDashboard struct {
	Workers          []Worker `json:"workers"`
	Runtimes         any      `json:"runtimes"`
	Sessions         any      `json:"sessions"`
	PendingApprovals int      `json:"pending_approvals"`
	QueuedCommands   int      `json:"queued_commands"`
}

func (s *Store) AdminDashboardSnapshot(ctx context.Context) (AdminDashboard, error) {
	workers, err := s.ListWorkers(ctx)
	if err != nil {
		return AdminDashboard{}, err
	}
	runtimes, err := s.RuntimeSnapshot(ctx)
	if err != nil {
		return AdminDashboard{}, err
	}
	sessions, err := s.SessionSnapshot(ctx)
	if err != nil {
		return AdminDashboard{}, err
	}
	var approvals, commands int
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE state='pending'`).Scan(&approvals); err != nil {
		return AdminDashboard{}, err
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM commands WHERE status IN ('pending','dispatched','acknowledged')`).Scan(&commands); err != nil {
		return AdminDashboard{}, err
	}
	return AdminDashboard{Workers: workers, Runtimes: runtimes, Sessions: sessions, PendingApprovals: approvals, QueuedCommands: commands}, nil
}

func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hashSecret(v string) []byte  { return sha256Bytes([]byte(v)) }
func sha256Bytes(v []byte) []byte { sum := sha256.Sum256(v); return sum[:] }
func nullableBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}
