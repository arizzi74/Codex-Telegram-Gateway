package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func draftLogin(t *testing.T, s *Store, owner string) string {
	t.Helper()
	id := []byte(uuid.NewString())
	if err := insertAdminCredential(context.Background(), s.pool, AdminCredential{ID: id, UserHandle: []byte(owner), CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)}); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAdminSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func draftCiphertext(value byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string(value), 48)))
}

func TestAdminDraftOwnershipAndRevisionTombstones(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	a := draftLogin(t, s, "account-a")
	b := draftLogin(t, s, "account-b")
	id := uuid.New()
	ciphertext := draftCiphertext('x')
	first, err := s.PutAdminDraft(ctx, a, id, 1, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExpiresAt.Before(time.Now().Add(29*time.Minute)) || first.ExpiresAt.After(time.Now().Add(31*time.Minute)) {
		t.Fatal("incorrect expiry")
	}
	for _, call := range []func() error{func() error { _, e := s.GetAdminDraft(ctx, b, id); return e }, func() error { _, e := s.PutAdminDraft(ctx, b, id, 2, ciphertext); return e }, func() error { return s.DeleteAdminDraft(ctx, b, id, 3) }} {
		if err := call(); !errors.Is(err, ErrAdminDraftNotFound) {
			t.Fatalf("cross-account access: %v", err)
		}
	}
	if _, err := s.PutAdminDraft(ctx, a, id, 1, draftCiphertext('y')); !errors.Is(err, ErrAdminDraftConflict) {
		t.Fatalf("conflicting same revision: %v", err)
	}
	if _, err := s.PutAdminDraft(ctx, a, id, 2, ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAdminDraft(ctx, a, id, 1, ciphertext); !errors.Is(err, ErrAdminDraftConflict) {
		t.Fatalf("stale write: %v", err)
	}
	if err := s.DeleteAdminDraft(ctx, a, id, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAdminDraft(ctx, a, id); !errors.Is(err, ErrAdminDraftNotFound) {
		t.Fatalf("deleted draft visible: %v", err)
	}
	for _, revision := range []int64{1, 2, 3, 100} {
		if _, err := s.PutAdminDraft(ctx, a, id, revision, ciphertext); !errors.Is(err, ErrAdminDraftConflict) {
			t.Fatalf("resurrected tombstone at %d: %v", revision, err)
		}
	}
	// DELETE arriving before a delayed first PUT must also leave a tombstone.
	id = uuid.New()
	if err := s.DeleteAdminDraft(ctx, a, id, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAdminDraft(ctx, a, id, 1, ciphertext); !errors.Is(err, ErrAdminDraftConflict) {
		t.Fatal("early delete failed", err)
	}
}

func TestAdminDraftExpiryLoginRecoveryAndRevocation(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	old := draftLogin(t, s, "same-account")
	fresh := draftLogin(t, s, "same-account")
	id := uuid.New()
	text := draftCiphertext('z')
	if _, err := s.PutAdminDraft(ctx, old, id, 1, text); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1 WHERE token_hash=$2`, time.Now().Add(-time.Minute), hashSecret(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAdminDraft(ctx, old, id); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("expired login accepted: %v", err)
	}
	if _, err := s.GetAdminDraft(ctx, fresh, id); err != nil {
		t.Fatal("fresh same-account login cannot recover", err)
	}
	if err := s.RevokeAdminSession(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAdminDraft(ctx, fresh, id, 2, text); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("revoked writer accepted: %v", err)
	}
	fresh = draftLogin(t, s, "same-account")
	if _, err := s.pool.Exec(ctx, `UPDATE admin_drafts SET expires_at=$1`, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAdminDraft(ctx, fresh, id); !errors.Is(err, ErrAdminDraftNotFound) {
		t.Fatalf("expired draft visible: %v", err)
	}
	if err := s.CleanupExpiredAdminDrafts(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM admin_drafts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expiry cleanup: %d %v", count, err)
	}
}

func TestAdminDraftBoundsAndPerAccountLimit(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token := draftLogin(t, s, "bounded-account")
	for _, body := range []string{"", "plaintext", base64.RawURLEncoding.EncodeToString(make([]byte, 28)), base64.RawURLEncoding.EncodeToString(make([]byte, AdminDraftMaxBytes+1)), draftCiphertext('x') + "="} {
		if _, err := s.PutAdminDraft(ctx, token, uuid.New(), 1, body); !errors.Is(err, ErrAdminDraftInvalid) {
			t.Fatalf("invalid envelope accepted length %d: %v", len(body), err)
		}
	}
	for _, revision := range []int64{0, -1, 1 << 53} {
		if _, err := s.PutAdminDraft(ctx, token, uuid.New(), revision, draftCiphertext('x')); !errors.Is(err, ErrAdminDraftInvalid) {
			t.Fatal("invalid revision accepted", err)
		}
	}
	for i := 0; i < adminDraftLimit; i++ {
		if _, err := s.PutAdminDraft(ctx, token, uuid.New(), 1, draftCiphertext('x')); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.PutAdminDraft(ctx, token, uuid.New(), 1, draftCiphertext('x')); !errors.Is(err, ErrAdminDraftLimit) {
		t.Fatal("limit not enforced", err)
	}
	other := draftLogin(t, s, "another-account")
	if _, err := s.PutAdminDraft(ctx, other, uuid.New(), 1, draftCiphertext('x')); err != nil {
		t.Fatal("account limit incorrectly shared", err)
	}
}

func TestAdminDraftNormalSendWorkflowDoesNotExhaustLiveLimit(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token := draftLogin(t, s, "active-account")
	for i := 0; i < 80; i++ {
		id := uuid.New()
		if _, err := s.PutAdminDraft(ctx, token, id, 1, draftCiphertext('x')); err != nil {
			t.Fatal(i, err)
		}
		if err := s.DeleteAdminDraft(ctx, token, id, 2); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := s.PutAdminDraft(ctx, token, uuid.New(), 1, draftCiphertext('x')); err != nil {
		t.Fatal("routine sends exhausted encrypted draft allowance", err)
	}
}

func TestAdminDraftRenewalAndExplicitRevocationPurgeCopies(t *testing.T) {
	for _, operation := range []string{"logout", "credential", "all"} {
		t.Run(operation, func(t *testing.T) {
			s := integrationStore(t)
			ctx := context.Background()
			old, credential := webPushTestLogin(t, s)
			other, _ := webPushTestLogin(t, s)
			id := uuid.New()
			if _, err := s.PutAdminDraft(ctx, old, id, 1, draftCiphertext('x')); err != nil {
				t.Fatal(err)
			}
			ceremony := adminLoginCeremony(t, s, old)
			fresh, err := s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, adminLoginCredential(credential))
			if err != nil {
				t.Fatal(err)
			}
			info, err := s.AdminSessionInfo(ctx, fresh)
			if err != nil {
				t.Fatal(err)
			}
			var bound uuid.UUID
			if err := s.pool.QueryRow(ctx, `SELECT admin_session_id FROM admin_drafts WHERE recovery_id=$1`, id).Scan(&bound); err != nil || bound != info.ID {
				t.Fatalf("renewal failed to migrate draft login: %s %v", bound, err)
			}
			// Revoke without GET rebinding, to exercise the actual login-rotation hook.
			switch operation {
			case "logout":
				err = s.RevokeAdminSession(ctx, fresh)
			case "credential":
				err = s.RevokeAdminCredential(ctx, credential)
			case "all":
				err = s.RevokeAdminBrowserSessions(ctx, other, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			var deleted bool
			var ciphertext any
			if err := s.pool.QueryRow(ctx, `SELECT deleted,ciphertext FROM admin_drafts WHERE recovery_id=$1`, id).Scan(&deleted, &ciphertext); err != nil || !deleted || ciphertext != nil {
				t.Fatalf("revocation retained encrypted draft: deleted=%v ciphertext=%T error=%v", deleted, ciphertext, err)
			}
			if _, err := s.PutAdminDraft(ctx, fresh, id, 2, draftCiphertext('x')); !errors.Is(err, ErrAdminSessionInvalid) {
				t.Fatalf("revoked login resurrected draft: %v", err)
			}
		})
	}
}
