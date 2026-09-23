package registry

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrWebPushKeysMissing          = errors.New("registry: Web Push keys are not initialized")
	ErrWebPushSubscriptionNotFound = errors.New("registry: Web Push subscription not found")
	ErrWebPushSubscriptionLimit    = errors.New("registry: Web Push device limit reached")
	ErrWebPushInvalidSubscription  = errors.New("registry: invalid Web Push subscription")
)

type WebPushKeys struct{ PrivateKey, PublicKey string }

// WebPushSubscription contains confidential delivery capabilities, not public
// API output. HTTP handlers return only ID and enabled state.
type WebPushSubscription struct {
	ID                     uuid.UUID
	Endpoint, P256DH, Auth string
}

type WebPushDelivery struct {
	ID, LeaseID, EventID, SessionID uuid.UUID
	Subscription                    WebPushSubscription
	Attempt                         int
	ExpiresAt                       time.Time
}

// EnsureWebPushKeys is race-safe across gateway processes. Empty candidates
// only read the existing key, allowing the sender to avoid repeated generation.
func (s *Store) EnsureWebPushKeys(ctx context.Context, privateKey, publicKey string) (WebPushKeys, error) {
	if privateKey != "" || publicKey != "" {
		privateBytes, a := base64.RawURLEncoding.DecodeString(privateKey)
		publicBytes, b := base64.RawURLEncoding.DecodeString(publicKey)
		private, c := ecdh.P256().NewPrivateKey(privateBytes)
		if a != nil || b != nil || c != nil || !bytes.Equal(private.PublicKey().Bytes(), publicBytes) {
			return WebPushKeys{}, ErrWebPushInvalidSubscription
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO webpush_keys(singleton,private_key,public_key) VALUES(1,$1,$2) ON CONFLICT(singleton) DO NOTHING`, privateKey, publicKey); err != nil {
			return WebPushKeys{}, err
		}
	}
	var keys WebPushKeys
	err := s.pool.QueryRow(ctx, `SELECT private_key,public_key FROM webpush_keys WHERE singleton=1`).Scan(&keys.PrivateKey, &keys.PublicKey)
	if errors.Is(err, sql.ErrNoRows) {
		return WebPushKeys{}, ErrWebPushKeysMissing
	}
	return keys, err
}

func validateWebPushSubscription(input WebPushSubscription) bool {
	u, err := url.Parse(input.Endpoint)
	if err != nil || len(input.Endpoint) > 4096 || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	public, err := base64.RawURLEncoding.DecodeString(input.P256DH)
	if err != nil || len(public) != 65 {
		return false
	}
	if _, err = ecdh.P256().NewPublicKey(public); err != nil {
		return false
	}
	auth, err := base64.RawURLEncoding.DecodeString(input.Auth)
	return err == nil && len(auth) == 16
}

// Cookie expiry does not revoke an explicit background notification opt-in.
// Enrolment and mutation require a fresh login; explicit sign-out and credential
// removal revoke the enrolled devices in their own authorization transaction.
func webPushOwner(ctx context.Context, tx *dbTx, token string) ([]byte, uuid.UUID, error) {
	var credential []byte
	var session uuid.UUID
	err := tx.QueryRow(ctx, `SELECT c.credential_id,a.session_id FROM admin_sessions a JOIN admin_credentials c ON c.credential_id=a.credential_id
        WHERE a.token_hash=$1 AND a.expires_at>`+sqliteNow+` AND a.revoked_at IS NULL AND c.revoked_at IS NULL`, hashSecret(token)).Scan(&credential, &session)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, uuid.Nil, ErrAdminSessionInvalid
	}
	return credential, session, err
}

func (s *Store) UpsertWebPushSubscription(ctx context.Context, token string, input WebPushSubscription) (WebPushSubscription, error) {
	if !validateWebPushSubscription(input) {
		return WebPushSubscription{}, ErrWebPushInvalidSubscription
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebPushSubscription{}, err
	}
	defer tx.Rollback(ctx)
	credential, session, err := webPushOwner(ctx, tx, token)
	if err != nil {
		return WebPushSubscription{}, err
	}
	var old WebPushSubscription
	var owner []byte
	err = tx.QueryRow(ctx, `SELECT subscription_id,credential_id,p256dh,auth FROM webpush_subscriptions WHERE endpoint=$1`, input.Endpoint).Scan(&old.ID, &owner, &old.P256DH, &old.Auth)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WebPushSubscription{}, err
	}
	if err == nil {
		if !bytes.Equal(owner, credential) {
			return WebPushSubscription{}, ErrWebPushInvalidSubscription
		}
		input.ID = old.ID
		if old.P256DH != input.P256DH || old.Auth != input.Auth {
			if _, err = tx.Exec(ctx, `DELETE FROM webpush_deliveries WHERE subscription_id=$1`, input.ID); err != nil {
				return WebPushSubscription{}, err
			}
		}
	} else {
		var total, owned int
		if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(credential_id=$1),0) FROM webpush_subscriptions`, credential).Scan(&total, &owned); err != nil {
			return WebPushSubscription{}, err
		}
		if total >= 64 || owned >= 16 {
			return WebPushSubscription{}, ErrWebPushSubscriptionLimit
		}
		input.ID = uuid.New()
	}
	_, err = tx.Exec(ctx, `INSERT INTO webpush_subscriptions(subscription_id,credential_id,admin_session_id,endpoint,p256dh,auth,created_at,updated_at)
        VALUES($1,$2,$3,$4,$5,$6,`+sqliteNow+`,`+sqliteNow+`)
        ON CONFLICT(subscription_id) DO UPDATE SET admin_session_id=excluded.admin_session_id,p256dh=excluded.p256dh,auth=excluded.auth,
          created_at=CASE WHEN webpush_subscriptions.p256dh<>excluded.p256dh OR webpush_subscriptions.auth<>excluded.auth THEN excluded.created_at ELSE webpush_subscriptions.created_at END,
          updated_at=excluded.updated_at`, input.ID, credential, session, input.Endpoint, input.P256DH, input.Auth)
	if err != nil {
		return WebPushSubscription{}, err
	}
	return input, tx.Commit(ctx)
}

func (s *Store) GetWebPushSubscription(ctx context.Context, token string, id uuid.UUID) (WebPushSubscription, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebPushSubscription{}, err
	}
	defer tx.Rollback(ctx)
	credential, _, err := webPushOwner(ctx, tx, token)
	if err != nil {
		return WebPushSubscription{}, err
	}
	var sub WebPushSubscription
	err = tx.QueryRow(ctx, `SELECT subscription_id,endpoint,p256dh,auth FROM webpush_subscriptions WHERE subscription_id=$1 AND credential_id=$2`, id, credential).Scan(&sub.ID, &sub.Endpoint, &sub.P256DH, &sub.Auth)
	if errors.Is(err, sql.ErrNoRows) {
		return WebPushSubscription{}, ErrWebPushSubscriptionNotFound
	}
	if err != nil {
		return WebPushSubscription{}, err
	}
	return sub, tx.Commit(ctx)
}

func (s *Store) DeleteWebPushSubscription(ctx context.Context, token string, id uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	credential, _, err := webPushOwner(ctx, tx, token)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM webpush_subscriptions WHERE subscription_id=$1 AND credential_id=$2`, id, credential); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type webPushHub struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
	closed      bool
}

// SubscribeWebPushNotifications provides coalescing post-commit wakeups. The
// durable queue remains authoritative and should also be checked for retries.
func (s *Store) SubscribeWebPushNotifications() (<-chan struct{}, func()) {
	h := &s.webpush
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{}, 1)
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	if h.subscribers == nil {
		h.subscribers = make(map[chan struct{}]struct{})
	}
	h.subscribers[ch] = struct{}{}
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subscribers[ch]; ok {
				delete(h.subscribers, ch)
				close(ch)
			}
		})
	}
}
func (s *Store) notifyWebPush() {
	h := &s.webpush
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (s *Store) closeWebPush() {
	h := &s.webpush
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.subscribers {
		delete(h.subscribers, ch)
		close(ch)
	}
}
