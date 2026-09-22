package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAdminCeremonyCapAtomicAcrossConnections(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	other, err := Open(ctx, store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	seedAdminChallenges(t, store, maxAdminCeremonies-1, 0, 0)
	var admitted atomic.Int32
	var calls sync.WaitGroup
	for i := range 16 {
		calls.Go(func() {
			s := store
			if i%2 == 1 {
				s = other
			}
			_, err := s.NewAdminCeremony(ctx, "authentication", nil, []byte(`{"challenge":"YQ"}`))
			if err == nil {
				admitted.Add(1)
			} else if !errors.Is(err, ErrAdminCeremonyLimit) {
				t.Errorf("admission error: %v", err)
			}
		})
	}
	calls.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted %d ceremonies at cap", admitted.Load())
	}
	assertAdminChallengeCount(t, store, maxAdminCeremonies)
	if _, err := store.pool.Exec(ctx, `UPDATE admin_challenges SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE challenge_id=(SELECT challenge_id FROM admin_challenges LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewAdminCeremony(ctx, "authentication", nil, []byte(`{"challenge":"YQ"}`)); err != nil {
		t.Fatalf("expired row did not release capacity: %v", err)
	}
	assertAdminChallengeCount(t, store, maxAdminCeremonies)
}

func TestAdminCeremonyCleanupBoundedAndIndexed(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	const live, expired, consumed = 3, 600, 600
	seedAdminChallenges(t, store, live, expired, consumed)
	n, err := store.PruneAdminCeremonies(ctx)
	if err != nil || n != 2*adminCeremonyPruneBatch {
		t.Fatalf("first prune = %d, %v", n, err)
	}
	assertAdminChallengeCount(t, store, live+expired+consumed-int(n))
	for range 3 {
		if _, err := store.PruneAdminCeremonies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	assertAdminChallengeCount(t, store, live)
	if _, err := store.NewAdminCeremony(ctx, "authentication", nil, []byte(`{"challenge":"YQ"}`)); err != nil {
		t.Fatal(err)
	}
	assertAdminChallengeCount(t, store, live+1)
	for _, query := range []struct{ sql, index string }{
		{`SELECT challenge_id FROM admin_challenges WHERE consumed_at IS NULL AND expires_at <= ` + sqliteNow + ` ORDER BY expires_at LIMIT 256`, "admin_challenges_active_idx"},
		{`SELECT challenge_id FROM admin_challenges WHERE consumed_at IS NOT NULL ORDER BY consumed_at LIMIT 256`, "admin_challenges_consumed_idx"},
		{`SELECT count(*) FROM (SELECT 1 FROM admin_challenges WHERE consumed_at IS NULL AND expires_at > ` + sqliteNow + ` LIMIT 1024)`, "admin_challenges_active_idx"},
	} {
		rows, err := store.pool.Query(ctx, "EXPLAIN QUERY PLAN "+query.sql)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.String(), query.index) || strings.Contains(plan.String(), "USE TEMP B-TREE") {
			t.Fatalf("cleanup/cap query lost bounded index access: %s", plan.String())
		}
	}
}

func TestAdminCeremonyClaimRollsBackWithFailedCredentialWrite(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	handle := []byte("01234567890123456789012345678901")
	bootstrap, err := store.BootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := newAdminTestCeremony(t, store, ctx, handle)
	credential := AdminCredential{ID: []byte("credential"), UserHandle: handle, CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)}
	if err := store.CompleteBootstrapCredential(ctx, c.ID, c.Binding, bootstrap, credential); err != nil {
		t.Fatal(err)
	}
	assertAdminChallengeCount(t, store, 0)
	c = newAdminTestCeremony(t, store, ctx, handle)
	if err := store.CompleteAdminCredential(ctx, c.ID, c.Binding, credential); err == nil {
		t.Fatal("duplicate credential accepted")
	}
	if _, err := store.ReadAdminCeremony(ctx, c.ID, "registration", c.Binding); err != nil {
		t.Fatalf("failed transaction consumed ceremony: %v", err)
	}
	credential.ID = []byte("additional")
	if err := store.CompleteAdminCredential(ctx, c.ID, c.Binding, credential); err != nil {
		t.Fatal(err)
	}
	assertAdminChallengeCount(t, store, 0)
	if _, err := store.ReadAdminCeremony(ctx, c.ID, "registration", c.Binding); !errors.Is(err, ErrAdminCeremonyInvalid) {
		t.Fatalf("successful ceremony replay = %v", err)
	}
	if err := store.CompleteAdminCredential(ctx, c.ID, c.Binding, credential); !errors.Is(err, ErrAdminCeremonyInvalid) {
		t.Fatalf("successful claim replay = %v", err)
	}
}

func seedAdminChallenges(t *testing.T, store *Store, live, expired, consumed int) {
	t.Helper()
	_, err := store.pool.Exec(context.Background(), `WITH RECURSIVE n(i) AS (
        SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<$1
    ) INSERT INTO admin_challenges(challenge_id,purpose,challenge_hash,expires_at,consumed_at,session_data)
    SELECT printf('%08x-0000-0000-0000-000000000000', i), 'authentication', zeroblob(32),
      CASE WHEN i>$2 AND i<=$2+$3 THEN '2000-01-01T00:00:00.000000000Z'
        ELSE (strftime('%Y-%m-%dT%H:%M:%f','now','+5 minutes') || '000000Z') END,
      CASE WHEN i>$2+$3 THEN '2000-01-01T00:00:00.000000000Z' ELSE NULL END,
      '{"challenge":"YQ"}' FROM n`, live+expired+consumed, live, expired)
	if err != nil {
		t.Fatal(err)
	}
}

func assertAdminChallengeCount(t *testing.T, store *Store, want int) {
	t.Helper()
	var got int
	if err := store.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_challenges`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("challenge rows = %d, want %d", got, want)
	}
}
