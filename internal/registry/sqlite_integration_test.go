package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/migrations"
)

func TestSQLiteSQLArgumentsPreserveTypesAndPlaceholderOrder(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	stamp := time.Date(2026, 9, 15, 14, 3, 2, 123456789, time.FixedZone("test", 7200))
	id := uuid.New()
	payload := json.RawMessage(`{"answer":42}`)
	blob := []byte{0, 1, 255}
	var gotTime time.Time
	var optionalTime, nullTime *time.Time
	var gotID uuid.UUID
	var optionalID, nullID *uuid.UUID
	var gotJSON, nullJSON json.RawMessage
	var gotBlob []byte
	var jsonType, blobType, storedTime string
	err := store.pool.QueryRow(ctx, `SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,typeof($7),typeof($9),$1`,
		stamp, &stamp, (*time.Time)(nil), id, &id, (*uuid.UUID)(nil), payload, json.RawMessage(nil), blob,
	).Scan(&gotTime, &optionalTime, &nullTime, &gotID, &optionalID, &nullID, &gotJSON, &nullJSON, &gotBlob, &jsonType, &blobType, &storedTime)
	if err != nil {
		t.Fatal(err)
	}
	if !gotTime.Equal(stamp) || optionalTime == nil || !optionalTime.Equal(stamp) || nullTime != nil || storedTime != "2026-09-15T12:03:02.123456789Z" {
		t.Fatalf("timestamp normalization/precision/null roundtrip failed: %v %v %v %q", gotTime, optionalTime, nullTime, storedTime)
	}
	if gotID != id || optionalID == nil || *optionalID != id || nullID != nil {
		t.Fatalf("UUID/null roundtrip failed: %v %v %v", gotID, optionalID, nullID)
	}
	if !bytes.Equal(gotJSON, payload) || nullJSON != nil || !bytes.Equal(gotBlob, blob) || jsonType != "text" || blobType != "blob" {
		t.Fatalf("JSON/blob/null roundtrip failed: %s %s %v %q %q", gotJSON, nullJSON, gotBlob, jsonType, blobType)
	}
	var first, second, repeated, literal string
	if err := store.pool.QueryRow(ctx, `SELECT $2,$1,$2,'$1' /* $3 */ -- $4
        `, "first", "second").Scan(&first, &second, &repeated, &literal); err != nil {
		t.Fatal(err)
	}
	if first != "second" || second != "first" || repeated != "second" || literal != "$1" {
		t.Fatalf("SQL positional arguments changed meaning: %q %q %q %q", first, second, repeated, literal)
	}
}

func TestSQLiteCancelledTransactionCannotLeakUncommittedChangesIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("cwk_sqlite_cancel_test"))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "committed", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	transactionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	tx, err := store.pool.Begin(transactionContext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(transactionContext, "UPDATE workers SET name='cancelled' WHERE worker_id=$1", worker.ID); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := tx.Commit(transactionContext); err == nil {
		t.Fatal("cancelled transaction committed")
	}
	_ = tx.Rollback(transactionContext)
	readContext, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	var name string
	if err := store.pool.QueryRow(readContext, "SELECT name FROM workers WHERE worker_id=$1", worker.ID).Scan(&name); err != nil || name != "committed" {
		t.Fatalf("cancelled transaction leaked changes or poisoned connection: %q, %v", name, err)
	}
	// database/sql may replace a cancelled transaction's connection. Settings
	// must also hold on that replacement connection, especially foreign keys.
	var foreignKeys int
	if err := store.pool.QueryRow(readContext, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("replacement connection lost foreign keys: %d, %v", foreignKeys, err)
	}
}

func TestSQLiteDurabilityAndPrivateFilesIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 10000} {
		var got int
		if err := store.pool.QueryRow(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %d, want %d: %v", pragma, got, want, err)
		}
	}
	var mode string
	if err := store.pool.QueryRow(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, want wal: %v", mode, err)
	}
	// The WAL contains credentials and queued prompts just like the database.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(store.pool.path + suffix)
		if err != nil {
			t.Fatalf("stat database%s: %v", suffix, err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("database%s mode = %04o, want 0600", suffix, got)
		}
	}
}

func TestSQLiteOpenRequiresPersistentFile(t *testing.T) {
	for _, path := range []string{"", " ", ":memory:", "file::memory:?cache=shared", "file:gateway.db", "postgres://localhost/gateway"} {
		t.Run(path, func(t *testing.T) {
			store, err := Open(context.Background(), path)
			if err == nil {
				store.Close()
				t.Fatal("non-file database configuration accepted")
			}
		})
	}
}

func TestSQLiteConcurrentMigrationsAndChecksumValidationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	stores := make([]*Store, 2)
	for i := range stores {
		var err error
		stores[i], err = Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stores[i].Close)
	}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	for _, store := range stores {
		go func() {
			<-start
			results <- store.Migrate(ctx)
		}()
	}
	close(start)
	for range stores {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, store := range stores {
		if err := store.CheckMigrations(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := stores[0].pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != len(migrations.Files) {
		t.Fatalf("concurrent migrations applied %d versions, want %d: %v", count, len(migrations.Files), err)
	}
	if _, err := stores[0].pool.Exec(ctx, "UPDATE schema_migrations SET checksum=$1", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := stores[1].CheckMigrations(ctx); err == nil {
		t.Fatal("migration checksum change was accepted")
	}
	if err := stores[1].Migrate(ctx); err == nil {
		t.Fatal("migration checksum change was silently overwritten")
	}
}

func TestSQLiteRollbackAndRestartPreserveCommittedStateIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx := context.Background()
	token := "cwk_sqlite_restart_test"
	hash := sha256.Sum256([]byte(token))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "committed", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "help"}); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "UPDATE workers SET name='uncommitted' WHERE worker_id=$1", worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE telegram_deliveries SET status='sent'"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	path := store.pool.path
	store.Close()
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.CheckMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.AuthenticateWorker(ctx, token)
	if err != nil || got.ID != worker.ID || got.Name != "committed" {
		t.Fatalf("committed enrollment did not survive rollback/restart: %+v, %v", got, err)
	}
	deliveries, err := reopened.ClaimDeliveries(ctx, 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].Attempt != 1 {
		t.Fatalf("committed pending delivery did not survive rollback/restart: %+v, %v", deliveries, err)
	}
}

func TestSQLiteForeignKeysAndImmutableEventLogIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',1,2,0,$1)`, uuid.New()); err == nil {
		t.Fatal("binding accepted a session that does not exist")
	}
	event := env.discovery(t)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"UPDATE events SET kind='changed' WHERE event_id=$1",
		"DELETE FROM events WHERE event_id=$1",
	} {
		if _, err := env.store.pool.Exec(ctx, query, event.ID); err == nil {
			t.Errorf("immutable event log allowed %q", query)
		}
	}
	var count int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE event_id=$1", event.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("immutable event disappeared: %d, %v", count, err)
	}
	var check string
	if err := env.store.pool.QueryRow(ctx, "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("SQLite integrity check = %q: %v", check, err)
	}
}

func TestSQLiteCompetingStoresNeverClaimTheSameDeliveryIntegration(t *testing.T) {
	store := integrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const count = 40
	for i := range count {
		if _, err := store.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: int64(i + 1), UserID: 10, ChatID: 20, Action: "help"}); err != nil {
			t.Fatal(err)
		}
	}
	second, err := Open(ctx, store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	testCompetingClaims(t, []*Store{store, second}, count, func(s *Store) ([]string, error) {
		rows, err := s.ClaimDeliveries(ctx, 3)
		var ids []string
		for _, row := range rows {
			if row.Attempt != 1 {
				return nil, fmt.Errorf("delivery %s attempt=%d, want 1", row.ID, row.Attempt)
			}
			ids = append(ids, row.ID)
		}
		return ids, err
	})
}

func TestSQLiteCompetingStoresNeverClaimTheSameCleanupIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const count = 12
	for i := range count {
		progressEvent(t, env, uint64(i+2), "agent_progress_message", "turn-a", "")
		delivery := claimProgress(t, env.store, 1)[0]
		if _, err := env.store.PrepareDeliveryChunks(ctx, delivery.ID, []json.RawMessage{json.RawMessage(`{"text":"temporary"}`)}); err != nil {
			t.Fatal(err)
		}
		if err := env.store.MarkDeliveryChunkSent(ctx, delivery.ID, 0, int64(i+100), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	progressEvent(t, env, count+2, "turn_completed", "turn-a", "")
	final := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, final.ID, 200, "", "", ""); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	testCompetingClaims(t, []*Store{env.store, second}, count, func(s *Store) ([]string, error) {
		rows, err := s.ClaimTelegramDeletions(ctx, 2)
		var ids []string
		for _, row := range rows {
			if row.Attempt != 1 {
				return nil, fmt.Errorf("deletion %s attempt=%d, want 1", row.ID, row.Attempt)
			}
			ids = append(ids, row.ID)
		}
		return ids, err
	})
}

func testCompetingClaims(t *testing.T, stores []*Store, count int, claim func(*Store) ([]string, error)) {
	t.Helper()
	start := make(chan struct{})
	results := make(chan []string, len(stores))
	errors := make(chan error, len(stores))
	var workers sync.WaitGroup
	for _, store := range stores {
		workers.Go(func() {
			<-start
			var all []string
			for {
				ids, err := claim(store)
				if err != nil {
					errors <- err
					return
				}
				all = append(all, ids...)
				if len(ids) == 0 {
					results <- all
					return
				}
			}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("competing claim failed: %v", err)
	}
	seen := make(map[string]bool, count)
	for ids := range results {
		for _, id := range ids {
			if seen[id] {
				t.Errorf("both stores claimed %s before its lease expired", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != count {
		t.Errorf("competing stores claimed %d records, want %d", len(seen), count)
	}
}
