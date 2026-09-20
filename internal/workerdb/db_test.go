package workerdb

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	legacybolt "go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"), 0o600, &Options{Timeout: time.Second, WorkerID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteTransactionsIndexedCursorsAndEmptyValues(t *testing.T) {
	db := openTestDB(t)
	name := []byte("events")
	if err := db.Update(func(tx *Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return err
		}
		for _, key := range [][]byte{{0}, {0, 0}, {0, 1}, {0xff}} {
			if err := bucket.Put(key, key); err != nil {
				return err
			}
		}
		return bucket.Put([]byte("empty"), nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error {
		bucket := tx.Bucket(name)
		if bucket.Get([]byte("empty")) == nil {
			t.Fatal("empty value became a missing key")
		}
		if bucket.Get([]byte("missing")) != nil {
			t.Fatal("missing value became an empty record")
		}
		cursor := bucket.Cursor()
		key, _ := cursor.Seek([]byte{0, 0})
		if !bytes.Equal(key, []byte{0, 0}) {
			t.Fatalf("seek = %x", key)
		}
		if err := cursor.Delete(); err != nil {
			return err
		}
		key, _ = cursor.Next()
		if !bytes.Equal(key, []byte{0, 1}) {
			t.Fatalf("delete skipped next key: %x", key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	forced := errors.New("rollback")
	if err := db.Update(func(tx *Tx) error {
		if err := tx.Bucket(name).Put([]byte("rolled-back"), []byte("value")); err != nil {
			return err
		}
		return forced
	}); !errors.Is(err, forced) {
		t.Fatalf("rollback = %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		if tx.Bucket(name).Get([]byte("rolled-back")) != nil {
			t.Fatal("rolled-back entry persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Tx) error { return tx.Bucket(name).Put([]byte("write"), nil) }); err == nil {
		t.Fatal("read transaction allowed mutation")
	}
	var mode string
	var synchronous, foreignKeys int
	if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q %v", mode, err)
	}
	if err := db.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("synchronous %d %v", synchronous, err)
	}
	if err := db.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys %d %v", foreignKeys, err)
	}
	var id, parent, unused int
	var plan string
	if err := db.db.QueryRow("EXPLAIN QUERY PLAN SELECT key,value FROM entries WHERE bucket=? AND key>=? ORDER BY key LIMIT 1", name, []byte{0}).Scan(&id, &parent, &unused, &plan); err != nil || !strings.Contains(plan, "PRIMARY KEY") || strings.Contains(plan, "SCAN") {
		t.Fatalf("cursor must seek indexed records: %s %v", plan, err)
	}
}

func TestSQLiteReadErrorsRollBackIgnoredMutation(t *testing.T) {
	db := openTestDB(t)
	if err := db.Update(func(tx *Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("test"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *Tx) error {
		bucket := tx.Bucket([]byte("test"))
		if _, err := tx.tx.Exec("DROP TABLE entries"); err != nil {
			return err
		}
		_ = bucket.Get([]byte("missing")) // Existing callers cannot get a read error here.
		return nil
	}); err == nil {
		t.Fatal("read error was silently committed")
	}
	if err := db.Update(func(tx *Tx) error { return tx.Bucket([]byte("test")).Put([]byte("ok"), []byte("yes")) }); err != nil {
		t.Fatalf("failed transaction did not roll back schema mutation: %v", err)
	}
}

func TestSQLiteWorkerLockPrivacyAndReadOnlyDiagnostics(t *testing.T) {
	db := openTestDB(t)
	if _, err := Open(db.Path(), 0o600, &Options{Timeout: 25 * time.Millisecond}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("concurrent worker open = %v", err)
	}
	if err := db.Update(func(tx *Tx) error { _, err := tx.CreateBucketIfNotExists([]byte("diagnostic")); return err }); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := sql.Open("sqlite", "file:"+db.Path()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = diagnostic.QueryRow("SELECT count(*) FROM buckets").Scan(&count)
	_ = diagnostic.Close()
	if err != nil || count != 1 {
		t.Fatalf("read-only diagnostics failed: count=%d err=%v", count, err)
	}
	for _, suffix := range []string{"", ".lock", "-wal", "-shm"} {
		info, err := os.Lstat(db.Path() + suffix)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("insecure sidecar %q: %v %v", suffix, info, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(db.Path(), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
}

func createLegacy(t *testing.T, path string, nested bool) map[string]map[string][]byte {
	t.Helper()
	values := map[string]map[string][]byte{
		"meta":     {"schema_version": []byte("1"), "worker_id": []byte("worker"), "next_event_seq": {0, 0, 0, 0, 0, 0, 0, 7}},
		"sessions": {}, "commands": {}, "outbox": {}, "async_questions": {}, "future-flat-bucket": {"binary": {0, 0xff, 0, 3}, "empty": {}},
	}
	for index := 0; index < 1000; index++ {
		values["sessions"][fmt.Sprintf("runtime\x00%08d", index)] = []byte(fmt.Sprintf(`{"name":"Session %d"}`, index))
	}
	legacy, err := legacybolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	err = legacy.Update(func(tx *legacybolt.Tx) error {
		for name, entries := range values {
			bucket, err := tx.CreateBucketIfNotExists([]byte(name))
			if err != nil {
				return err
			}
			for key, value := range entries {
				if err := bucket.Put([]byte(key), value); err != nil {
					return err
				}
			}
		}
		if nested {
			_, err := tx.Bucket([]byte("sessions")).CreateBucket([]byte("nested"))
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func TestLegacyMigrationPreservesEveryRecordAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	want := createLegacy(t, path, false)
	db, err := Open(path, 0o600, &Options{Timeout: time.Second, WorkerID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if format, err := fileFormat(path); err != nil || format != "sqlite" {
		t.Fatalf("format = %s %v", format, err)
	}
	if err := db.View(func(tx *Tx) error {
		for name, entries := range want {
			bucket := tx.Bucket([]byte(name))
			if bucket == nil {
				t.Fatalf("missing bucket %s", name)
			}
			for key, value := range entries {
				if got := bucket.Get([]byte(key)); !bytes.Equal(got, value) || got == nil {
					t.Fatalf("migrated %q/%q = %x want %x", name, key, got, value)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	backup, err := legacybolt.Open(path+".bbolt-backup", 0o600, &legacybolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = backup.View(func(tx *legacybolt.Tx) error {
		for name, entries := range want {
			for key, value := range entries {
				if !bytes.Equal(tx.Bucket([]byte(name)).Get([]byte(key)), value) {
					t.Fatalf("backup differs at %q/%q", name, key)
				}
			}
		}
		return nil
	})
	_ = backup.Close()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path + ".bbolt-backup")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v %v", info, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, 0o600, &Options{WorkerID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
	backups, err := filepath.Glob(path + ".bbolt-backup*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("migration repeated on reopen: %v %v", backups, err)
	}
}

func TestLegacyMigrationFailuresLeaveOriginalUntouched(t *testing.T) {
	for _, test := range []struct {
		name, identity string
		nested, active bool
	}{
		{name: "wrong_identity", identity: "other"}, {name: "nested_bucket", identity: "worker", nested: true}, {name: "active_old_worker", identity: "worker", active: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			createLegacy(t, path, test.nested)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if test.active {
				legacy, err := legacybolt.Open(path, 0o600, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer legacy.Close()
			}
			if db, err := Open(path, 0o600, &Options{Timeout: 25 * time.Millisecond, WorkerID: test.identity}); err == nil {
				_ = db.Close()
				t.Fatal("invalid or active legacy state migrated")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed migration modified source: %v", err)
			}
			if files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".worker-sqlite-migration-*")); len(files) != 0 {
				t.Fatalf("failed migration leaked temporary state: %v", files)
			}
		})
	}
}

func TestSQLiteRejectsUnsafeStatePathsAndForeignDatabases(t *testing.T) {
	for _, test := range []string{"symlink", "public", "foreign_sqlite"} {
		t.Run(test, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			switch test {
			case "symlink":
				if err := os.WriteFile(path+"-target", nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-target", path); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "foreign_sqlite":
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				foreign, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = foreign.Exec("CREATE TABLE unrelated(id INTEGER)")
				_ = foreign.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if db, err := Open(path, 0o600, nil); err == nil {
				_ = db.Close()
				t.Fatal("unsafe or foreign state accepted")
			}
		})
	}
}

func TestSQLiteRecoversInterruptedEmptyInitialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	interrupted, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = interrupted.Exec("VACUUM")
	_ = interrupted.Close()
	if err != nil {
		t.Fatal(err)
	}
	if format, err := fileFormat(path); err != nil || format != "sqlite" {
		t.Fatalf("expected an initialized SQLite header: %s %v", format, err)
	}
	db, err := Open(path, 0o600, nil)
	if err != nil {
		t.Fatalf("empty interrupted initialization could not recover: %v", err)
	}
	defer db.Close()
	if err := db.Update(func(tx *Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("recovered"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
