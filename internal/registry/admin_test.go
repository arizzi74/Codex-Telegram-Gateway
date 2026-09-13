package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestAdminBootstrapCeremonyReplayAndExpiryIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	bootstrap, err := store.BootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CheckAdminBootstrap(ctx, bootstrap); err != nil {
		t.Fatal(err)
	}
	handle := make([]byte, 32)
	for i := range handle {
		handle[i] = byte(i + 1)
	}
	first := newAdminTestCeremony(t, store, ctx, handle)
	credential := AdminCredential{ID: []byte("credential-one"), UserHandle: handle, CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)}
	if err = store.CompleteBootstrapCredential(ctx, first.ID, first.Binding, bootstrap, credential); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteBootstrapCredential(ctx, first.ID, first.Binding, bootstrap, credential); !errors.Is(err, ErrAdminBootstrapInvalid) {
		t.Fatalf("bootstrap replay = %v", err)
	}
	_, credentials, err := store.AdminUser(ctx)
	if err != nil || len(credentials) != 1 {
		t.Fatalf("admin user = %d credentials, %v", len(credentials), err)
	}
	second := newAdminTestCeremony(t, store, ctx, handle)
	additional := AdminCredential{ID: []byte("credential-two"), UserHandle: handle, CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)}
	if err = store.CompleteAdminCredential(ctx, second.ID, second.Binding, additional); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteAdminCredential(ctx, second.ID, second.Binding, additional); !errors.Is(err, ErrAdminCeremonyInvalid) {
		t.Fatalf("ceremony replay = %v", err)
	}
	expired := newAdminTestCeremony(t, store, ctx, handle)
	if _, err = store.pool.Exec(ctx, `UPDATE admin_challenges SET expires_at=now()-interval '1 second' WHERE challenge_id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReadAdminCeremony(ctx, expired.ID, "registration", expired.Binding); !errors.Is(err, ErrAdminCeremonyInvalid) {
		t.Fatalf("expired ceremony = %v", err)
	}
}

func newAdminTestCeremony(t *testing.T, store *Store, ctx context.Context, handle []byte) AdminCeremony {
	t.Helper()
	challenge := make([]byte, 32)
	for i := range challenge {
		challenge[i] = byte(i + 10)
	}
	session := []byte(`{"challenge":"` + base64.RawURLEncoding.EncodeToString(challenge) + `"}`)
	c, err := store.NewAdminCeremony(ctx, "registration", handle, session)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == uuid.Nil {
		t.Fatal("missing ceremony id")
	}
	return c
}
