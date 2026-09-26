package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
)

const AdminDraftMaxBytes = 256 * 1024
const AdminDraftTTL = 30 * time.Minute
const adminDraftLimit = 32

// Short-lived tombstones are small and need a separate bound: clearing a draft
// after every send must not exhaust the encrypted-record allowance.
const adminDraftTombstoneLimit = 1024
const adminDraftMaxRevision = int64(1<<53 - 1)

var (
	ErrAdminDraftInvalid  = errors.New("registry: invalid encrypted draft")
	ErrAdminDraftNotFound = errors.New("registry: encrypted draft not found")
	ErrAdminDraftConflict = errors.New("registry: encrypted draft revision conflict")
	ErrAdminDraftLimit    = errors.New("registry: encrypted draft limit reached")
)

type AdminDraft struct {
	Ciphertext string    `json:"ciphertext"`
	Revision   int64     `json:"revision"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func validAdminDraft(id uuid.UUID, revision int64, ciphertext string, deletion bool) bool {
	if id == uuid.Nil || revision <= 0 || revision > adminDraftMaxRevision {
		return false
	}
	if deletion {
		return ciphertext == ""
	}
	if len(ciphertext) > base64.RawURLEncoding.EncodedLen(AdminDraftMaxBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(ciphertext)
	// AES-GCM envelope: 12-byte nonce, nonempty plaintext, 16-byte authentication tag.
	return err == nil && len(decoded) > 28 && len(decoded) <= AdminDraftMaxBytes && base64.RawURLEncoding.EncodeToString(decoded) == ciphertext
}

// Every operation authenticates inside the same transaction as the data access.
// The recovery UUID is globally unique; a different account cannot claim, read,
// overwrite, or tombstone an existing account's record.
func (s *Store) GetAdminDraft(ctx context.Context, token string, id uuid.UUID) (AdminDraft, error) {
	if id == uuid.Nil {
		return AdminDraft{}, ErrAdminDraftInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AdminDraft{}, err
	}
	defer tx.Rollback(ctx)
	owner, sessionID, err := adminSessionOwner(ctx, tx, token)
	if err != nil {
		return AdminDraft{}, err
	}
	var out AdminDraft
	err = tx.QueryRow(ctx, `SELECT ciphertext,revision,expires_at FROM admin_drafts WHERE recovery_id=$1 AND user_handle=$2 AND deleted=0 AND expires_at>`+sqliteNow, id, owner).Scan(&out.Ciphertext, &out.Revision, &out.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminDraft{}, ErrAdminDraftNotFound
	}
	if err != nil {
		return AdminDraft{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_drafts SET admin_session_id=$1 WHERE recovery_id=$2 AND user_handle=$3`, sessionID, id, owner); err != nil {
		return AdminDraft{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) PutAdminDraft(ctx context.Context, token string, id uuid.UUID, revision int64, ciphertext string) (AdminDraft, error) {
	return s.mutateAdminDraft(ctx, token, id, revision, ciphertext, false)
}

func (s *Store) DeleteAdminDraft(ctx context.Context, token string, id uuid.UUID, revision int64) error {
	_, err := s.mutateAdminDraft(ctx, token, id, revision, "", true)
	return err
}

func (s *Store) mutateAdminDraft(ctx context.Context, token string, id uuid.UUID, revision int64, ciphertext string, deletion bool) (AdminDraft, error) {
	if !validAdminDraft(id, revision, ciphertext, deletion) {
		return AdminDraft{}, ErrAdminDraftInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AdminDraft{}, err
	}
	defer tx.Rollback(ctx)
	owner, sessionID, err := adminSessionOwner(ctx, tx, token)
	if err != nil {
		return AdminDraft{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM admin_drafts WHERE expires_at<=`+sqliteNow); err != nil {
		return AdminDraft{}, err
	}
	var priorOwner []byte
	var priorRevision int64
	var deleted bool
	var priorCiphertext sql.NullString
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT user_handle,revision,deleted,ciphertext,expires_at FROM admin_drafts WHERE recovery_id=$1`, id).Scan(&priorOwner, &priorRevision, &deleted, &priorCiphertext, &expires)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AdminDraft{}, err
	}
	if err == nil {
		if !bytes.Equal(owner, priorOwner) {
			return AdminDraft{}, ErrAdminDraftNotFound
		}
		if !deletion && (deleted || revision < priorRevision || (revision == priorRevision && ciphertext != priorCiphertext.String)) {
			return AdminDraft{}, ErrAdminDraftConflict
		}
		if !deletion && revision == priorRevision {
			return AdminDraft{Ciphertext: priorCiphertext.String, Revision: priorRevision, ExpiresAt: expires}, tx.Commit(ctx)
		}
		if deletion && revision < priorRevision {
			revision = priorRevision
		}
	} else {
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM admin_drafts WHERE user_handle=$1 AND deleted=0`, owner).Scan(&count); err != nil {
			return AdminDraft{}, err
		}
		if !deletion && count >= adminDraftLimit {
			return AdminDraft{}, ErrAdminDraftLimit
		}
	}
	if deletion && !deleted {
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM admin_drafts WHERE user_handle=$1 AND deleted=1`, owner).Scan(&count); err != nil {
			return AdminDraft{}, err
		}
		if count >= adminDraftTombstoneLimit {
			return AdminDraft{}, ErrAdminDraftLimit
		}
	}
	expires = time.Now().UTC().Add(AdminDraftTTL)
	var payload any = ciphertext
	if deletion {
		payload = nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO admin_drafts(recovery_id,user_handle,ciphertext,revision,deleted,expires_at,admin_session_id) VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT(recovery_id) DO UPDATE SET ciphertext=excluded.ciphertext,revision=excluded.revision,deleted=excluded.deleted,expires_at=excluded.expires_at,admin_session_id=excluded.admin_session_id`, id, owner, payload, revision, deletion, expires, sessionID)
	if err != nil {
		return AdminDraft{}, err
	}
	return AdminDraft{Ciphertext: ciphertext, Revision: revision, ExpiresAt: expires}, tx.Commit(ctx)
}

// CleanupExpiredAdminDrafts bounds storage even when browsers never return.
// Call from the gateway's periodic maintenance loop; writes also prune expiry.
func (s *Store) CleanupExpiredAdminDrafts(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_drafts WHERE expires_at<=`+sqliteNow)
	return err
}

func migrateAdminDraftSession(ctx context.Context, tx *dbTx, oldID, newID uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE admin_drafts SET admin_session_id=$1 WHERE admin_session_id=$2`, newID, oldID)
	return err
}
