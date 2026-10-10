package registry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testAdminPassword = "  correct horse battery staple  "

func passwordTestAdmin(t *testing.T, s *Store) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	bootstrap, err := s.BootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	handle := []byte("password-test-singleton-owner")
	ceremony := newAdminTestCeremony(t, s, ctx, handle)
	id := []byte(uuid.NewString())
	if err = s.CompleteBootstrapCredential(ctx, ceremony.ID, ceremony.Binding, bootstrap,
		AdminCredential{ID: id, UserHandle: handle, CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)}); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAdminSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return token, id
}

func passwordTestEnable(t *testing.T, s *Store, token string) {
	t.Helper()
	if err := s.SetAdminPassword(context.Background(), token, "  operator  ", testAdminPassword); err != nil {
		t.Fatal(err)
	}
}

func passwordTestLogin(t *testing.T, s *Store, predecessor string) string {
	t.Helper()
	token, err := s.LoginAdminPassword(context.Background(), "operator", testAdminPassword, predecessor, "Mozilla/5.0 Firefox/100.0 (Linux)")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAdminPasswordPolicyDisabledAndPasskeyIsolation(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token, passkey := passwordTestAdmin(t, s)
	settings, err := s.AdminPasswordSettings(ctx)
	if err != nil || settings.Enabled || settings.Username != "" {
		t.Fatalf("initial settings: %+v %v", settings, err)
	}
	if _, err = s.LoginAdminPassword(ctx, "operator", testAdminPassword, "", ""); !errors.Is(err, ErrAdminPasswordInvalid) {
		t.Fatalf("disabled login: %v", err)
	}
	for _, invalid := range []struct{ username, password string }{
		{"", testAdminPassword}, {"   ", testAdminPassword}, {"bad\nname", testAdminPassword},
		{"operator\n", testAdminPassword}, {strings.Repeat("x", 65), testAdminPassword},
		{string([]byte{0xff}), testAdminPassword}, {"operator", strings.Repeat("x", 11)},
		{"operator", strings.Repeat("x", 257)},
	} {
		if err = s.SetAdminPassword(ctx, token, invalid.username, invalid.password); !errors.Is(err, ErrAdminPasswordPolicy) {
			t.Fatalf("invalid policy accepted username=%q length=%d: %v", invalid.username, len(invalid.password), err)
		}
	}
	passwordTestEnable(t, s, token)
	settings, err = s.AdminPasswordSettings(ctx)
	if err != nil || !settings.Enabled || settings.Username != "operator" {
		t.Fatalf("enabled settings: %+v %v", settings, err)
	}
	for _, invalid := range []struct{ username, password string }{
		{"unknown", testAdminPassword}, {"operator", "incorrect password"}, {"operator", strings.TrimSpace(testAdminPassword)},
	} {
		if _, err = s.LoginAdminPassword(ctx, invalid.username, invalid.password, "", ""); !errors.Is(err, ErrAdminPasswordInvalid) {
			t.Fatalf("wrong credentials accepted: %v", err)
		}
	}
	passwordToken := passwordTestLogin(t, s, "")
	credential, err := s.ValidateAdminSession(ctx, passwordToken)
	if err != nil || credential.Kind != "password" || string(credential.CredentialJSON) != "{}" {
		t.Fatalf("password session: %+v %v", credential, err)
	}
	info, err := s.AdminSessionInfo(ctx, passwordToken)
	if err != nil || info.AuthenticationMethod != "password" || info.BrowserLabel != "Firefox on Linux" || info.ExpiresAt.Sub(info.CreatedAt) != 8*time.Hour {
		t.Fatalf("password session metadata: %+v %v", info, err)
	}
	list, err := s.AdminCredentials(ctx)
	if err != nil || len(list) != 1 || string(list[0].ID) != string(passkey) {
		t.Fatalf("password leaked into passkeys: %+v %v", list, err)
	}
	if _, err = s.ActiveAdminCredential(ctx, credential.ID); !errors.Is(err, ErrAdminCredentialGone) {
		t.Fatalf("password credential available to WebAuthn: %v", err)
	}
	if err = s.TouchAdminCredential(ctx, credential); !errors.Is(err, ErrAdminCredentialGone) {
		t.Fatalf("password credential mutable through WebAuthn: %v", err)
	}
	ceremony := adminLoginCeremony(t, s, "")
	if _, err = s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, credential); !errors.Is(err, ErrAdminCredentialGone) {
		t.Fatalf("password accepted as passkey login: %v", err)
	}
	if err = s.RevokeAdminCredential(ctx, passkey); err == nil {
		t.Fatal("password enabled removal of final passkey")
	}
	var encoded string
	if err = s.pool.QueryRow(ctx, `SELECT password_hash FROM admin_password_login`).Scan(&encoded); err != nil || strings.Contains(encoded, testAdminPassword) || !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("password not stored as Argon2id hash: %v", err)
	}
	listSessions, err := s.AdminBrowserSessions(ctx, passwordToken)
	if err != nil || len(listSessions) != 2 {
		t.Fatalf("browser sessions not shared across methods: %+v %v", listSessions, err)
	}
}

func TestAdminPasswordSettingsRequireCurrentRecentBootstrappedSession(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	beforeBootstrap, _ := webPushTestLogin(t, s)
	if err := s.SetAdminPassword(ctx, beforeBootstrap, "operator", testAdminPassword); !errors.Is(err, ErrAdminCredentialGone) {
		t.Fatalf("unbootstrapped settings accepted: %v", err)
	}
	token, _ := passwordTestAdmin(t, s)
	for _, created := range []time.Time{time.Now().Add(-6 * time.Minute), time.Now().Add(time.Minute)} {
		if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET created_at=$1 WHERE token_hash=$2`, created, hashSecret(token)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAdminPassword(ctx, token, "operator", testAdminPassword); !errors.Is(err, ErrAdminReauthenticationRequired) {
			t.Fatalf("non-recent setting accepted: %v", err)
		}
		if err := s.DisableAdminPassword(ctx, token); !errors.Is(err, ErrAdminReauthenticationRequired) {
			t.Fatalf("non-recent disable accepted: %v", err)
		}
	}
	if err := s.RevokeAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminPassword(ctx, token, "operator", testAdminPassword); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("revoked setting accepted: %v", err)
	}
	if err := s.DisableAdminPassword(ctx, token); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("revoked disable accepted: %v", err)
	}
}

func TestAdminPasswordRotationMigratesPushAndDraftsAcrossMethods(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	old, passkey := passwordTestAdmin(t, s)
	passwordTestEnable(t, s, old)
	sub := webPushTestRegister(t, s, old)
	draftID := uuid.New()
	if _, err := s.PutAdminDraft(ctx, old, draftID, 1, draftCiphertext('p')); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1 WHERE token_hash=$2`, time.Now().Add(-time.Minute), hashSecret(old)); err != nil {
		t.Fatal(err)
	}
	passwordToken := passwordTestLogin(t, s, old)
	if _, err := s.ValidateAdminSession(ctx, old); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("expired predecessor not revoked: %v", err)
	}
	assertPasswordSessionData(t, s, passwordToken, sub.ID, draftID)
	ceremony := adminLoginCeremony(t, s, passwordToken)
	passkeyToken, err := s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, adminLoginCredential(passkey))
	if err != nil {
		t.Fatal(err)
	}
	assertPasswordSessionData(t, s, passkeyToken, sub.ID, draftID)
	if _, err = s.ValidateAdminSession(ctx, passwordToken); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("password predecessor still valid: %v", err)
	}
	// A cookie revoked before the login request creates a fresh session, with
	// no ability to recapture already-migrated subscriptions or drafts.
	fresh := passwordTestLogin(t, s, passwordToken)
	assertPasswordSessionData(t, s, passkeyToken, sub.ID, draftID)
	if err = s.RevokeAdminSession(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	assertPasswordSessionData(t, s, passkeyToken, sub.ID, draftID)
}

func assertPasswordSessionData(t *testing.T, s *Store, token string, subscription, draft uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	info, err := s.AdminSessionInfo(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []struct {
		sql string
		id  uuid.UUID
	}{
		{`SELECT admin_session_id FROM webpush_subscriptions WHERE subscription_id=$1`, subscription},
		{`SELECT admin_session_id FROM admin_drafts WHERE recovery_id=$1 AND deleted=0`, draft},
	} {
		var session uuid.UUID
		if err = s.pool.QueryRow(ctx, query.sql, query.id).Scan(&session); err != nil || session != info.ID {
			t.Fatalf("browser data not owned by renewed session: %s %v", session, err)
		}
	}
}

func TestAdminPasswordReplacementAndDisableInvalidateSessionsAndData(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace", true: "disable"}[disable], func(t *testing.T) {
			s := integrationStore(t)
			ctx := context.Background()
			passkeyToken, _ := passwordTestAdmin(t, s)
			passwordTestEnable(t, s, passkeyToken)
			passwordToken := passwordTestLogin(t, s, "")
			webPushTestRegister(t, s, passwordToken)
			draft := uuid.New()
			if _, err := s.PutAdminDraft(ctx, passwordToken, draft, 1, draftCiphertext('p')); err != nil {
				t.Fatal(err)
			}
			var err error
			if disable {
				err = s.DisableAdminPassword(ctx, passwordToken)
			} else {
				err = s.SetAdminPassword(ctx, passwordToken, "updated", "another strong pass phrase")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ValidateAdminSession(ctx, passwordToken); !errors.Is(err, ErrAdminSessionInvalid) {
				t.Fatalf("old password session still valid: %v", err)
			}
			if _, err = s.ValidateAdminSession(ctx, passkeyToken); err != nil {
				t.Fatal("passkey session revoked", err)
			}
			if webPushCount(t, s, "webpush_subscriptions") != 0 {
				t.Fatal("password revocation retained push subscriptions")
			}
			if _, err = s.GetAdminDraft(ctx, passkeyToken, draft); !errors.Is(err, ErrAdminDraftNotFound) {
				t.Fatalf("password revocation retained draft ciphertext: %v", err)
			}
			if _, err = s.LoginAdminPassword(ctx, "operator", testAdminPassword, "", ""); !errors.Is(err, ErrAdminPasswordInvalid) {
				t.Fatalf("old password still accepted: %v", err)
			}
			if disable {
				if webPushCount(t, s, "admin_password_login") != 0 {
					t.Fatal("disable retained password hash")
				}
			} else if _, err = s.LoginAdminPassword(ctx, "updated", "another strong pass phrase", "", ""); err != nil {
				t.Fatal("replacement not accepted", err)
			}
		})
	}
}

func TestAdminPasswordLoginFencesChangedHashAndRevokedPredecessor(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	token, _ := passwordTestAdmin(t, s)
	passwordTestEnable(t, s, token)
	stale, err := s.adminPasswordLoginSnapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	passwordTestEnable(t, s, token)
	if _, err = s.completeAdminPasswordLogin(ctx, stale, "Browser"); !errors.Is(err, ErrAdminPasswordInvalid) {
		t.Fatalf("password changed during verification accepted: %v", err)
	}
	first, err := s.adminPasswordLoginSnapshot(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	if _, err = s.completeAdminPasswordLogin(ctx, first, "Browser"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.completeAdminPasswordLogin(ctx, second, "Browser"); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("simultaneous stale predecessor replay accepted: %v", err)
	}
	current := passwordTestLogin(t, s, "")
	revoked, err := s.adminPasswordLoginSnapshot(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeAdminSession(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err = s.completeAdminPasswordLogin(ctx, revoked, "Browser"); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("predecessor revoked during verification accepted: %v", err)
	}
}

func TestAdminPasswordReplacementRollbackPreservesExistingLogin(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	passkeyToken, _ := passwordTestAdmin(t, s)
	passwordTestEnable(t, s, passkeyToken)
	passwordToken := passwordTestLogin(t, s, "")
	sub := webPushTestRegister(t, s, passwordToken)
	draft := uuid.New()
	if _, err := s.PutAdminDraft(ctx, passwordToken, draft, 1, draftCiphertext('p')); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `CREATE TRIGGER password_write_failure BEFORE INSERT ON admin_credentials
 WHEN NEW.kind='password' BEGIN SELECT RAISE(FAIL,'injected password write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminPassword(ctx, passkeyToken, "updated", "another strong pass phrase"); err == nil {
		t.Fatal("injected password replacement unexpectedly succeeded")
	}
	assertPasswordSessionData(t, s, passwordToken, sub.ID, draft)
	settings, err := s.AdminPasswordSettings(ctx)
	if err != nil || !settings.Enabled || settings.Username != "operator" {
		t.Fatalf("failed replacement changed settings: %+v %v", settings, err)
	}
	if _, err = s.LoginAdminPassword(ctx, "operator", testAdminPassword, "", ""); err != nil {
		t.Fatal("failed replacement changed old password", err)
	}
}
