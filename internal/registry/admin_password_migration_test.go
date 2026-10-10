package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/migrations"
)

func TestAdminPasswordMigrationPreservesLegacyPasskeySession(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "legacy-gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// Build the deployed schema independently of Store.Migrate. Do not change
	// the shared migration catalog: other tests may migrate in parallel.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE TABLE schema_migrations (
 version INTEGER NOT NULL PRIMARY KEY,
 checksum BLOB NOT NULL,
 applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')
 ) STRICT`); err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, file := range migrations.Files {
		version, err := migrationVersion(file)
		if err != nil {
			t.Fatal(err)
		}
		if version > 20 {
			continue
		}
		migration, err := migrations.SQL.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply legacy migration %s: %v", file, err)
		}
		checksum := sha256.Sum256(migration)
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, version, checksum[:]); err != nil {
			t.Fatal(err)
		}
		applied++
	}
	if applied != 20 {
		t.Fatalf("built %d legacy migrations, want 20", applied)
	}
	owner := []byte("legacy-admin-singleton-owner")
	credentialID := []byte("legacy-passkey-credential")
	credentialJSON := `{"authenticator":{"signCount":7}}`
	token := "legacy-admin-session-token"
	if _, err = tx.Exec(ctx, `INSERT INTO admin_users(user_id,user_handle) VALUES(1,$1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_credentials
 (credential_id,user_handle,public_key,sign_count,transports,credential_json)
 VALUES($1,$2,$1,7,'[]',$3)`, credentialID, owner, credentialJSON); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_sessions(session_id,credential_id,token_hash,expires_at)
 VALUES($1,$2,$3,$4)`, uuid.New(), credentialID, hashSecret(token), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err = s.Migrate(ctx); err != nil {
		t.Fatal("upgrade legacy gateway", err)
	}
	credential, err := s.ValidateAdminSession(ctx, token)
	if err != nil || credential.Kind != "passkey" || !bytes.Equal(credential.ID, credentialID) ||
		!bytes.Equal(credential.UserHandle, owner) || string(credential.CredentialJSON) != credentialJSON {
		t.Fatalf("legacy session changed during upgrade: %+v %v", credential, err)
	}
	passkeys, err := s.AdminCredentials(ctx)
	if err != nil || len(passkeys) != 1 || passkeys[0].Kind != "passkey" ||
		!bytes.Equal(passkeys[0].ID, credentialID) || string(passkeys[0].CredentialJSON) != credentialJSON {
		t.Fatalf("legacy passkey changed during upgrade: %+v %v", passkeys, err)
	}
	settings, err := s.AdminPasswordSettings(ctx)
	if err != nil || settings.Enabled || settings.Username != "" {
		t.Fatalf("upgrade enabled password authentication: %+v %v", settings, err)
	}
	if err = s.CheckMigrations(ctx); err != nil {
		t.Fatal("upgraded migration ledger invalid", err)
	}
}
