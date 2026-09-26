package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func adminLoginCeremony(t *testing.T, s *Store, token string) AdminCeremony {
	t.Helper()
	c, err := s.NewAdminLoginCeremony(context.Background(), []byte(`{"challenge":"dGVzdC1yZW5ld2FsLWNoYWxsZW5nZQ"}`), token, "Mozilla/5.0 (iPhone) Version/18.0 Safari/604.1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func adminLoginCredential(id []byte) AdminCredential {
	return AdminCredential{ID: id, CredentialJSON: []byte(`{"authenticator":{"signCount":2}}`)}
}

func TestAdminSessionAbsoluteExpiryAndActivity(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token, _ := webPushTestLogin(t, s)
	before, err := s.AdminSessionInfo(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if before.ExpiresAt.Sub(before.CreatedAt) != 8*time.Hour || !before.ReauthenticatedAt.Equal(before.CreatedAt) {
		t.Fatalf("invalid fixed expiry metadata: %+v", before)
	}
	if _, err = s.ValidateAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	after, err := s.AdminSessionInfo(ctx, token)
	if err != nil || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("validation extended expiry: %+v %v", after, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1 WHERE token_hash=$2`, time.Now().Add(-time.Minute), hashSecret(token)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateAdminSession(ctx, token); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("expired login accepted: %v", err)
	}
	if _, err = s.AdminSessionInfo(ctx, token); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("expired metadata accepted: %v", err)
	}
}

func TestAdminRenewalRotatesAndPreservesNotifications(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "expired"}[expired], func(t *testing.T) {
			s := integrationStore(t)
			ctx := context.Background()
			old, id := webPushTestLogin(t, s)
			sub := webPushTestRegister(t, s, old)
			if expired {
				if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1`, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			ceremony := adminLoginCeremony(t, s, old)
			token, err := s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, adminLoginCredential(id))
			if err != nil {
				t.Fatal(err)
			}
			if token == old {
				t.Fatal("renewal reused cookie token")
			}
			if _, err = s.ValidateAdminSession(ctx, old); !errors.Is(err, ErrAdminSessionInvalid) {
				t.Fatalf("predecessor valid: %v", err)
			}
			info, err := s.AdminSessionInfo(ctx, token)
			if err != nil || info.BrowserLabel != "Safari on iOS" {
				t.Fatalf("renewal metadata: %+v %v", info, err)
			}
			var bound uuid.UUID
			if err = s.pool.QueryRow(ctx, `SELECT admin_session_id FROM webpush_subscriptions WHERE subscription_id=$1`, sub.ID).Scan(&bound); err != nil || bound != info.ID {
				t.Fatalf("notification relationship lost: %v %v", bound, err)
			}
			if _, err = s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, adminLoginCredential(id)); !errors.Is(err, ErrAdminCeremonyInvalid) {
				t.Fatalf("replay succeeded: %v", err)
			}
			if err = s.RevokeAdminSession(ctx, token); err != nil {
				t.Fatal(err)
			}
			if webPushCount(t, s, "webpush_subscriptions") != 0 {
				t.Fatal("logout retained notifications")
			}
		})
	}
}

func TestAdminRenewalRevocationAndConcurrentFinishes(t *testing.T) {
	t.Run("revoked after begin", func(t *testing.T) {
		s := integrationStore(t)
		ctx := context.Background()
		old, id := webPushTestLogin(t, s)
		webPushTestRegister(t, s, old)
		ceremony := adminLoginCeremony(t, s, old)
		if err := s.RevokeAdminSession(ctx, old); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, adminLoginCredential(id)); !errors.Is(err, ErrAdminSessionInvalid) {
			t.Fatalf("revoked predecessor accepted: %v", err)
		}
		if webPushCount(t, s, "admin_sessions") != 1 || webPushCount(t, s, "webpush_subscriptions") != 0 {
			t.Fatal("revoked relationships resurrected")
		}
	})
	t.Run("two simultaneous ceremonies", func(t *testing.T) {
		s := integrationStore(t)
		ctx := context.Background()
		old, id := webPushTestLogin(t, s)
		webPushTestRegister(t, s, old)
		ceremonies := []AdminCeremony{adminLoginCeremony(t, s, old), adminLoginCeremony(t, s, old)}
		results := make(chan error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, c := range ceremonies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := s.CompleteAdminLogin(ctx, c.ID, c.Binding, adminLoginCredential(id))
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		success, invalid := 0, 0
		for err := range results {
			if err == nil {
				success++
			} else if errors.Is(err, ErrAdminSessionInvalid) {
				invalid++
			} else {
				t.Fatal(err)
			}
		}
		if success != 1 || invalid != 1 || webPushCount(t, s, "admin_sessions") != 2 || webPushCount(t, s, "webpush_subscriptions") != 1 {
			t.Fatalf("concurrent rotation success=%d invalid=%d", success, invalid)
		}
	})
	t.Run("revoked before begin is fresh login", func(t *testing.T) {
		s := integrationStore(t)
		ctx := context.Background()
		old, id := webPushTestLogin(t, s)
		webPushTestRegister(t, s, old)
		if err := s.RevokeAdminSession(ctx, old); err != nil {
			t.Fatal(err)
		}
		c := adminLoginCeremony(t, s, old)
		if _, err := s.CompleteAdminLogin(ctx, c.ID, c.Binding, adminLoginCredential(id)); err != nil {
			t.Fatal(err)
		}
		if webPushCount(t, s, "webpush_subscriptions") != 0 {
			t.Fatal("fresh login resurrected old notification opt-in")
		}
	})
}

func TestAdminBrowserSessionAccountScopeAndRevoke(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token, first := webPushTestLogin(t, s)
	otherToken, second := webPushTestLogin(t, s)
	other, err := s.AdminSessionInfo(ctx, otherToken)
	if err != nil {
		t.Fatal(err)
	}
	webPushTestRegister(t, s, token)
	webPushTestRegister(t, s, otherToken)
	if _, err = s.pool.Exec(ctx, `UPDATE admin_credentials SET user_handle=$1 WHERE credential_id=$2`, []byte("different-account"), second); err != nil {
		t.Fatal(err)
	}
	list, err := s.AdminBrowserSessions(ctx, token)
	if err != nil || len(list) != 1 || !list[0].Current {
		t.Fatalf("account leaked: %+v %v", list, err)
	}
	if err = s.RevokeAdminBrowserSessions(ctx, token, &other.ID); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("cross-account revocation: %v", err)
	}
	if _, err = s.ValidateAdminSession(ctx, otherToken); err != nil {
		t.Fatal("other account revoked")
	}
	ownSecond, err := s.CreateAdminSession(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := s.CreateAdminSession(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	webPushTestRegister(t, s, expired)
	if _, err = s.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1 WHERE token_hash=$2`, time.Now().Add(-time.Hour), hashSecret(expired)); err != nil {
		t.Fatal(err)
	}
	list, err = s.AdminBrowserSessions(ctx, token)
	if err != nil || len(list) != 2 {
		t.Fatalf("listing includes expired: %+v %v", list, err)
	}
	if err = s.RevokeAdminBrowserSessions(ctx, token, nil); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{token, ownSecond} {
		if _, err = s.ValidateAdminSession(ctx, tok); !errors.Is(err, ErrAdminSessionInvalid) {
			t.Fatalf("account session remains valid: %v", err)
		}
	}
	if _, err = s.ValidateAdminSession(ctx, otherToken); err != nil {
		t.Fatal("revoke-all touched different account")
	}
	if webPushCount(t, s, "webpush_subscriptions") != 1 {
		t.Fatal("revoke-all failed to clear expired subscription or touched another account")
	}
}

func TestAdminRenewalRequiresSameAccountAndCanChangePasskey(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	old, first := webPushTestLogin(t, s)
	_, second := webPushTestLogin(t, s)
	sub := webPushTestRegister(t, s, old)
	if _, err := s.pool.Exec(ctx, `UPDATE admin_credentials SET user_handle=$1 WHERE credential_id=$2`, []byte("another-account"), second); err != nil {
		t.Fatal(err)
	}
	c := adminLoginCeremony(t, s, old)
	if _, err := s.CompleteAdminLogin(ctx, c.ID, c.Binding, adminLoginCredential(second)); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("cross-account renewal succeeded: %v", err)
	}
	if _, err := s.ValidateAdminSession(ctx, old); err != nil {
		t.Fatal("failed renewal revoked old session")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE admin_credentials SET user_handle=(SELECT user_handle FROM admin_credentials WHERE credential_id=$1) WHERE credential_id=$2`, first, second); err != nil {
		t.Fatal(err)
	}
	token, err := s.CompleteAdminLogin(ctx, c.ID, c.Binding, adminLoginCredential(second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	var owner []byte
	if err = s.pool.QueryRow(ctx, `SELECT credential_id FROM webpush_subscriptions WHERE subscription_id=$1`, sub.ID).Scan(&owner); err != nil || string(owner) != string(second) {
		t.Fatalf("credential ownership not migrated: %q %v", owner, err)
	}
}

func TestAdminBrowserLabelDiscardsUntrustedData(t *testing.T) {
	for _, ua := range []string{"<script>private device</script>", strings.Repeat("a", 2000), "\x00\r\nprivate"} {
		if label := adminBrowserLabel(ua); label != "Browser" {
			t.Fatalf("raw header leaked in %q", label)
		}
	}
}
