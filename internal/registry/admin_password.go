package registry

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
)

var (
	ErrAdminPasswordInvalid          = errors.New("registry: invalid admin username or password")
	ErrAdminPasswordPolicy           = errors.New("registry: username must contain 1 to 64 bytes without control characters and password must contain 12 to 256 bytes")
	ErrAdminReauthenticationRequired = errors.New("registry: recent admin authentication required")
)

// AdminPasswordSettings contains no authentication material and never includes
// the durable password hash or its credential identifier.
type AdminPasswordSettings struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
}

func (s *Store) AdminPasswordSettings(ctx context.Context) (AdminPasswordSettings, error) {
	var settings AdminPasswordSettings
	err := s.pool.QueryRow(ctx, `SELECT p.username FROM admin_password_login p
 JOIN admin_credentials c ON c.credential_id=p.credential_id
 WHERE p.login_id=1 AND c.kind='password' AND c.revoked_at IS NULL`).Scan(&settings.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	settings.Enabled = err == nil
	return settings, err
}

func normalizeAdminUsername(username string) (string, bool) {
	if !utf8.ValidString(username) || len(username) > 256 {
		return "", false
	}
	for _, r := range username {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	username = strings.TrimSpace(username)
	return username, len(username) >= 1 && len(username) <= 64
}

// SetAdminPassword enables or replaces the optional password login. Hashing
// occurs before the transaction; authorization is rechecked atomically with
// the replacement, including any session revocation during expensive hashing.
func (s *Store) SetAdminPassword(ctx context.Context, currentToken, username, password string) error {
	username, valid := normalizeAdminUsername(username)
	if !valid || len(password) < 12 || len(password) > 256 {
		return ErrAdminPasswordPolicy
	}
	// Avoid password hashing for sessions that are already invalid or stale.
	if err := s.checkAdminPasswordWrite(ctx, currentToken); err != nil {
		return err
	}
	encoded, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	credentialID := make([]byte, 32)
	if _, err = rand.Read(credentialID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	handle, err := authorizeAdminPasswordWrite(ctx, tx, currentToken)
	if err != nil {
		return err
	}
	if err = revokeAdminPasswordCredential(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_credentials
 (credential_id,user_handle,public_key,credential_json,kind)
 VALUES($1,$2,$1,'{}','password')`, credentialID, handle); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_password_login(login_id,username,password_hash,credential_id)
 VALUES(1,$1,$2,$3) ON CONFLICT(login_id) DO UPDATE SET username=excluded.username,
 password_hash=excluded.password_hash,credential_id=excluded.credential_id,updated_at=`+sqliteNow, username, encoded, credentialID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DisableAdminPassword revokes its sessions and dependent browser data in the
// same transaction that deletes the hash. Passkey logins remain available.
func (s *Store) DisableAdminPassword(ctx context.Context, currentToken string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = authorizeAdminPasswordWrite(ctx, tx, currentToken); err != nil {
		return err
	}
	if err = revokeAdminPasswordCredential(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM admin_password_login WHERE login_id=1`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) checkAdminPasswordWrite(ctx context.Context, token string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = authorizeAdminPasswordWrite(ctx, tx, token); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func authorizeAdminPasswordWrite(ctx context.Context, tx *dbTx, token string) ([]byte, error) {
	var handle []byte
	var recent bool
	err := tx.QueryRow(ctx, `SELECT c.user_handle,
 a.created_at>(strftime('%Y-%m-%dT%H:%M:%f','now','-5 minutes') || '000000Z') AND a.created_at<=`+sqliteNow+`
 FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id
 WHERE a.token_hash=$1 AND a.revoked_at IS NULL AND c.revoked_at IS NULL AND a.expires_at>`+sqliteNow, hashSecret(token)).Scan(&handle, &recent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAdminSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if !recent {
		return nil, ErrAdminReauthenticationRequired
	}
	var bootstrapped bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM admin_users u WHERE u.user_id=1 AND u.user_handle=$1
 AND EXISTS(SELECT 1 FROM admin_credentials c WHERE c.user_handle=u.user_handle AND c.kind='passkey' AND c.revoked_at IS NULL))`, handle).Scan(&bootstrapped)
	if err != nil {
		return nil, err
	}
	if !bootstrapped {
		return nil, ErrAdminCredentialGone
	}
	return handle, nil
}

func revokeAdminPasswordCredential(ctx context.Context, tx *dbTx) error {
	// Include expired sessions: notification subscriptions may outlive cookies.
	if _, err := tx.Exec(ctx, `DELETE FROM webpush_subscriptions WHERE credential_id IN
 (SELECT credential_id FROM admin_credentials WHERE kind='password')`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_drafts SET ciphertext=NULL,deleted=1 WHERE admin_session_id IN
 (SELECT a.session_id FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id WHERE c.kind='password')`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_sessions SET revoked_at=`+sqliteNow+` WHERE revoked_at IS NULL AND credential_id IN
 (SELECT credential_id FROM admin_credentials WHERE kind='password')`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE admin_credentials SET revoked_at=`+sqliteNow+` WHERE kind='password' AND revoked_at IS NULL`)
	return err
}

type adminPasswordLoginSnapshot struct {
	username     string
	encoded      string
	credentialID []byte
	handle       []byte
	predecessor  []byte
}

// LoginAdminPassword verifies outside database transactions and then fences the
// captured credential/hash and predecessor before issuing a normal session.
func (s *Store) LoginAdminPassword(ctx context.Context, username, password, predecessorToken, userAgent string) (string, error) {
	username, valid := normalizeAdminUsername(username)
	if !valid || len(password) > 256 {
		return "", ErrAdminPasswordInvalid
	}
	snapshot, err := s.adminPasswordLoginSnapshot(ctx, predecessorToken)
	if err != nil {
		return "", err
	}
	usernameMatches := subtle.ConstantTimeCompare(hashSecret(username), hashSecret(snapshot.username)) == 1
	if len(snapshot.credentialID) == 0 || !usernameMatches {
		auth.DummyVerifyPassword(password)
		return "", ErrAdminPasswordInvalid
	}
	if !auth.VerifyPassword(password, snapshot.encoded) {
		return "", ErrAdminPasswordInvalid
	}
	return s.completeAdminPasswordLogin(ctx, snapshot, adminBrowserLabel(userAgent))
}

func (s *Store) adminPasswordLoginSnapshot(ctx context.Context, predecessorToken string) (adminPasswordLoginSnapshot, error) {
	var snapshot adminPasswordLoginSnapshot
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT p.username,p.password_hash,c.credential_id,c.user_handle FROM admin_password_login p
 JOIN admin_credentials c ON c.credential_id=p.credential_id
 JOIN admin_users u ON u.user_id=1 AND u.user_handle=c.user_handle
 WHERE p.login_id=1 AND c.kind='password' AND c.revoked_at IS NULL`).Scan(&snapshot.username, &snapshot.encoded, &snapshot.credentialID, &snapshot.handle)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return snapshot, err
	}
	if predecessorToken != "" {
		// An already revoked or unknown cookie starts a fresh login. A captured
		// predecessor is instead fenced again after password verification.
		err = tx.QueryRow(ctx, `SELECT a.token_hash FROM admin_sessions a
 JOIN admin_credentials c ON c.credential_id=a.credential_id
 WHERE a.token_hash=$1 AND a.revoked_at IS NULL AND c.revoked_at IS NULL`, hashSecret(predecessorToken)).Scan(&snapshot.predecessor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return snapshot, err
		}
	}
	return snapshot, tx.Commit(ctx)
}

func (s *Store) completeAdminPasswordLogin(ctx context.Context, snapshot adminPasswordLoginSnapshot, label string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var currentHash string
	var currentID, handle []byte
	err = tx.QueryRow(ctx, `SELECT p.password_hash,c.credential_id,c.user_handle FROM admin_password_login p
 JOIN admin_credentials c ON c.credential_id=p.credential_id
 JOIN admin_users u ON u.user_id=1 AND u.user_handle=c.user_handle
 WHERE p.login_id=1 AND c.kind='password' AND c.revoked_at IS NULL`).Scan(&currentHash, &currentID, &handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAdminPasswordInvalid
	}
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare(hashSecret(currentHash), hashSecret(snapshot.encoded)) != 1 ||
		subtle.ConstantTimeCompare(currentID, snapshot.credentialID) != 1 ||
		subtle.ConstantTimeCompare(handle, snapshot.handle) != 1 {
		return "", ErrAdminPasswordInvalid
	}
	token, err := issueAdminSession(ctx, tx, currentID, handle, snapshot.predecessor, label)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE admin_credentials SET last_used_at=`+sqliteNow+` WHERE credential_id=$1`, currentID); err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}
