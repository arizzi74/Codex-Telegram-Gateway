package workerdb

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func bulkDB(t testing.TB) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"), 0o600, &Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Update(func(tx *Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("bulk"))
		if err != nil {
			return err
		}
		for index := 9999; index >= 0; index-- {
			key := []byte(fmt.Sprintf("%08d", index))
			if err := bucket.Put(key, bytes.Repeat(key, 8)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReadOnlyBulkScanOrderingEarlyReturnAndErrors(t *testing.T) {
	db := bulkDB(t)
	count := 0
	if err := db.View(func(tx *Tx) error {
		return tx.Bucket([]byte("bulk")).ForEach(func(key, value []byte) error {
			want := []byte(fmt.Sprintf("%08d", count))
			if !bytes.Equal(key, want) || !bytes.Equal(value, bytes.Repeat(want, 8)) {
				t.Fatalf("bulk stream record %d is incorrect: %q %q", count, key, value)
			}
			count++
			return nil
		})
	}); err != nil || count != 10000 {
		t.Fatalf("bulk scan count=%d err=%v", count, err)
	}
	stop := errors.New("stop after first result")
	count = 0
	if err := db.View(func(tx *Tx) error {
		return tx.Bucket([]byte("bulk")).ForEach(func(_, _ []byte) error {
			count++
			return stop
		})
	}); !errors.Is(err, stop) || count != 1 {
		t.Fatalf("callback failure was not propagated: count=%d err=%v", count, err)
	}
	if err := db.View(func(tx *Tx) error {
		bucket := tx.Bucket([]byte("bulk"))
		return bucket.ForEach(func(key, value []byte) error {
			// Even ignoring a rejected mutation must fail the outer View.
			_ = bucket.Put(key, value)
			return nil
		})
	}); err == nil {
		t.Fatal("read-only stream allowed a callback to mutate state")
	}
	// A failed query cannot be mistaken for a successful empty bucket scan.
	if _, err := db.db.Exec("ALTER TABLE entries RENAME TO unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *Tx) error {
		_ = tx.Bucket([]byte("bulk")).ForEach(func(_, _ []byte) error { return nil })
		return nil
	}); err == nil {
		t.Fatal("bulk stream silently ignored an SQL read error")
	}
}

func BenchmarkSQLiteBulkScan(b *testing.B) {
	db := bulkDB(b)
	for _, writable := range []bool{false, true} {
		name := "read_only_stream"
		transaction := db.View
		if writable {
			name, transaction = "writable_keyset", db.Update
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				count := 0
				err := transaction(func(tx *Tx) error {
					return tx.Bucket([]byte("bulk")).ForEach(func(_, _ []byte) error {
						count++
						return nil
					})
				})
				if err != nil || count != 10000 {
					b.Fatalf("count=%d err=%v", count, err)
				}
			}
		})
	}
}
