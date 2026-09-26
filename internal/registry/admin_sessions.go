package registry

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AdminBrowserSession exposes only non-secret browser-login information. Every new
// session follows passkey verification; CreatedAt is also the last verified
// authentication time. Activity never extends ExpiresAt.
type AdminBrowserSession struct {
	ID                uuid.UUID `json:"session_id"`
	BrowserLabel      string    `json:"browser_label"`
	CreatedAt         time.Time `json:"created_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	ReauthenticatedAt time.Time `json:"reauthenticated_at"`
	Current           bool      `json:"current"`
	UserHandle        []byte    `json:"-"`
}

func (s *Store) AdminSessionInfo(ctx context.Context, token string) (AdminBrowserSession, error) {
	var a AdminBrowserSession
	err := s.pool.QueryRow(ctx, `SELECT a.session_id,a.browser_label,a.created_at,COALESCE(a.last_seen_at,a.created_at),a.expires_at,c.user_handle
 FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id
 WHERE a.token_hash=$1 AND a.revoked_at IS NULL AND a.expires_at>`+sqliteNow+` AND c.revoked_at IS NULL`, hashSecret(token)).Scan(&a.ID, &a.BrowserLabel, &a.CreatedAt, &a.LastSeenAt, &a.ExpiresAt, &a.UserHandle)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminBrowserSession{}, ErrAdminSessionInvalid
	}
	a.ReauthenticatedAt = a.CreatedAt
	a.Current = true
	return a, err
}

func (s *Store) AdminBrowserSessions(ctx context.Context, token string) ([]AdminBrowserSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	handle, current, err := adminSessionOwner(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT a.session_id,a.browser_label,a.created_at,COALESCE(a.last_seen_at,a.created_at),a.expires_at
 FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id
 WHERE c.user_handle=$1 AND c.revoked_at IS NULL AND a.revoked_at IS NULL AND a.expires_at>`+sqliteNow+`
 ORDER BY a.last_seen_at DESC,a.created_at DESC,a.session_id`, handle)
	if err != nil {
		return nil, err
	}
	result := make([]AdminBrowserSession, 0)
	for rows.Next() {
		var a AdminBrowserSession
		if err := rows.Scan(&a.ID, &a.BrowserLabel, &a.CreatedAt, &a.LastSeenAt, &a.ExpiresAt); err != nil {
			rows.Close()
			return nil, err
		}
		a.ReauthenticatedAt = a.CreatedAt
		a.Current = a.ID == current
		result = append(result, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return result, tx.Commit(ctx)
}

func adminSessionOwner(ctx context.Context, tx *dbTx, token string) ([]byte, uuid.UUID, error) {
	var handle []byte
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT c.user_handle,a.session_id FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id
 WHERE a.token_hash=$1 AND a.revoked_at IS NULL AND a.expires_at>`+sqliteNow+` AND c.revoked_at IS NULL`, hashSecret(token)).Scan(&handle, &id)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrAdminSessionInvalid
	}
	return handle, id, err
}

// RevokeAdminBrowserSessions scopes every mutation to the verified account.
// A nil target revokes all logins, including expired notification opt-ins.
func (s *Store) RevokeAdminBrowserSessions(ctx context.Context, token string, target *uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	handle, _, err := adminSessionOwner(ctx, tx, token)
	if err != nil {
		return err
	}
	var id any
	if target != nil {
		id = *target
	}
	predicate := `credential_id IN (SELECT credential_id FROM admin_credentials WHERE user_handle=$1) AND ($2 IS NULL OR session_id=$2)`
	if _, err = tx.Exec(ctx, `DELETE FROM webpush_subscriptions WHERE admin_session_id IN (SELECT session_id FROM admin_sessions WHERE `+predicate+`)`, handle, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_drafts SET ciphertext=NULL,deleted=1 WHERE admin_session_id IN (SELECT session_id FROM admin_sessions WHERE `+predicate+`)`, handle, id); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=`+sqliteNow+` WHERE `+predicate+` AND revoked_at IS NULL`, handle, id)
	if err != nil {
		return err
	}
	if target != nil && result.RowsAffected() == 0 {
		return ErrAdminSessionInvalid
	}
	return tx.Commit(ctx)
}

// CompleteAdminLogin consumes a verified ceremony and rotates its bound
// predecessor atomically. It never uses a finish-request cookie to select the
// session to revoke or the notification subscriptions to migrate.
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
	var handle []byte
	if err = tx.QueryRow(ctx, `SELECT user_handle FROM admin_credentials WHERE credential_id=$1 AND revoked_at IS NULL`, credential.ID).Scan(&handle); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrAdminCredentialGone
		}
		return "", err
	}
	var predecessor []byte
	var label string
	if err = tx.QueryRow(ctx, `SELECT predecessor_token_hash,browser_label FROM admin_challenges WHERE challenge_id=$1`, ceremonyID).Scan(&predecessor, &label); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrAdminCeremonyInvalid
		}
		return "", err
	}
	var oldID uuid.UUID
	if len(predecessor) > 0 {
		var oldHandle []byte
		err = tx.QueryRow(ctx, `SELECT a.session_id,c.user_handle FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id
    WHERE a.token_hash=$1 AND a.revoked_at IS NULL AND c.revoked_at IS NULL`, predecessor).Scan(&oldID, &oldHandle)
		if err != nil || subtle.ConstantTimeCompare(oldHandle, handle) != 1 {
			if err == nil || errors.Is(err, sql.ErrNoRows) {
				err = ErrAdminSessionInvalid
			}
			return "", err
		}
	}
	if err = claimAdminCeremony(ctx, tx, ceremonyID, "authentication", binding); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_credentials SET credential_json=$2,sign_count=$3,last_used_at=`+sqliteNow+` WHERE credential_id=$1 AND revoked_at IS NULL`, credential.ID, credential.CredentialJSON, record.Authenticator.SignCount); err != nil {
		return "", err
	}
	token, err := randomSecret(32)
	if err != nil {
		return "", err
	}
	newID := uuid.New()
	if _, err = tx.Exec(ctx, `INSERT INTO admin_sessions(session_id,credential_id,token_hash,expires_at,browser_label,last_seen_at)
 VALUES($1,$2,$3,(strftime('%Y-%m-%dT%H:%M:%f','now','+8 hours') || '000000Z'),$4,`+sqliteNow+`)`, newID, credential.ID, hashSecret(token), label); err != nil {
		return "", err
	}
	if oldID != uuid.Nil {
		if _, err = tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=`+sqliteNow+` WHERE session_id=$1`, oldID); err != nil {
			return "", err
		}
		// Changing passkeys on the same account is allowed. Move both session and
		// credential ownership so later revocation of the old passkey is isolated.
		if _, err = tx.Exec(ctx, `UPDATE webpush_subscriptions SET admin_session_id=$1,credential_id=$2,updated_at=`+sqliteNow+` WHERE admin_session_id=$3`, newID, credential.ID, oldID); err != nil {
			return "", err
		}
		if err = migrateAdminDraftSession(ctx, tx, oldID, newID); err != nil {
			return "", err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

// No raw user-agent bytes enter session inventories (which may contain device
// names or attacker-controlled markup). These labels are intentionally coarse.
func adminBrowserLabel(userAgent string) string {
	ua := strings.ToLower(userAgent[:min(len(userAgent), 1024)])
	browser := "Browser"
	switch {
	case strings.Contains(ua, "edg/") || strings.Contains(ua, "edgios/"):
		browser = "Edge"
	case strings.Contains(ua, "firefox/") || strings.Contains(ua, "fxios/"):
		browser = "Firefox"
	case strings.Contains(ua, "chrome/") || strings.Contains(ua, "crios/"):
		browser = "Chrome"
	case strings.Contains(ua, "safari/"):
		browser = "Safari"
	}
	os := ""
	switch {
	case strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad"):
		os = "iOS"
	case strings.Contains(ua, "android"):
		os = "Android"
	case strings.Contains(ua, "windows"):
		os = "Windows"
	case strings.Contains(ua, "macintosh"):
		os = "macOS"
	case strings.Contains(ua, "linux"):
		os = "Linux"
	}
	if os != "" {
		return browser + " on " + os
	}
	return browser
}
