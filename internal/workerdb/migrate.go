package workerdb

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	legacybolt "go.etcd.io/bbolt"
)

// migrateLegacy never publishes partially copied state. The exclusive legacy
// lock also prevents migrating a file still used by an older running worker.
// A private bbolt backup is retained before the atomic SQLite replacement.
func migrateLegacy(path string, options Options) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return errors.New("worker SQLite store: legacy state has unexpected SQLite sidecars; original database was kept")
		}
	}
	legacy, err := legacybolt.Open(path, 0o600, &legacybolt.Options{Timeout: options.Timeout})
	if err != nil {
		return fmt.Errorf("worker SQLite store: open legacy state for migration (stop other workers first): %w", err)
	}
	defer legacy.Close()
	if err := legacy.View(func(tx *legacybolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if meta == nil || string(meta.Get([]byte("schema_version"))) != "1" {
			return errors.New("worker SQLite store: unsupported legacy schema")
		}
		if options.WorkerID == "" || string(meta.Get([]byte("worker_id"))) != options.WorkerID {
			return errors.New("worker SQLite store: legacy worker_id does not match enrolled identity")
		}
		return nil
	}); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".worker-sqlite-migration-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	if err := file.Close(); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(temporary)
		_ = os.Remove(temporary + "-journal")
	}()
	// DELETE mode produces one self-contained file for the atomic rename. WAL
	// is enabled only after publication; no SQLite sidecars are renamed.
	destination, err := openSQL(temporary, true, "DELETE")
	if err != nil {
		return fmt.Errorf("worker SQLite store: create migration database: %w", err)
	}
	defer destination.Close()
	if err := copyLegacy(legacy, destination); err != nil {
		return fmt.Errorf("worker SQLite store: copy legacy database: %w", err)
	}
	var integrity string
	if err := destination.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("worker SQLite store: migrated database failed integrity verification")
	}
	if err := destination.Close(); err != nil {
		return err
	}
	file, err = os.OpenFile(temporary, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	if err := backupLegacy(legacy, path); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("worker SQLite store: publish migration: %w", err)
	}
	return syncDirectory(path)
}

func copyLegacy(source *legacybolt.DB, destination *sql.DB) error {
	tx, err := destination.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	buckets, err := tx.Prepare("INSERT INTO buckets(name) VALUES(?)")
	if err != nil {
		return err
	}
	defer buckets.Close()
	entries, err := tx.Prepare("INSERT INTO entries(bucket,key,value) VALUES(?,?,?)")
	if err != nil {
		return err
	}
	defer entries.Close()
	var bucketCount, entryCount int64
	err = source.View(func(legacy *legacybolt.Tx) error {
		return legacy.ForEach(func(name []byte, bucket *legacybolt.Bucket) error {
			if _, err := buckets.Exec(name); err != nil {
				return err
			}
			bucketCount++
			return bucket.ForEach(func(key, value []byte) error {
				if value == nil {
					return errors.New("nested legacy buckets are unsupported; original database was kept")
				}
				if _, err := entries.Exec(name, key, nonnil(value)); err != nil {
					return err
				}
				entryCount++
				return nil
			})
		})
	})
	if err != nil {
		return err
	}
	var actualBuckets, actualEntries int64
	if err := tx.QueryRow("SELECT count(*) FROM buckets").Scan(&actualBuckets); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT count(*) FROM entries").Scan(&actualEntries); err != nil {
		return err
	}
	if actualBuckets != bucketCount || actualEntries != entryCount {
		return errors.New("migrated bucket or record count does not match legacy state")
	}
	// Verify every byte before replacing the source. This also covers opaque
	// future flat buckets and update-result deduplication markers.
	err = source.View(func(legacy *legacybolt.Tx) error {
		return legacy.ForEach(func(name []byte, bucket *legacybolt.Bucket) error {
			return bucket.ForEach(func(key, value []byte) error {
				var copied []byte
				if err := tx.QueryRow("SELECT value FROM entries WHERE bucket=? AND key=?", name, key).Scan(&copied); err != nil {
					return err
				}
				if !bytes.Equal(value, copied) {
					return errors.New("migrated record differs from legacy state")
				}
				return nil
			})
		})
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func backupLegacy(source *legacybolt.DB, path string) error {
	backupPath := path + ".bbolt-backup"
	var backup *os.File
	for attempt := 0; ; attempt++ {
		candidate := backupPath
		if attempt > 0 {
			candidate = fmt.Sprintf("%s.%d", backupPath, attempt)
		}
		var err error
		backup, err = os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("worker SQLite store: create legacy backup: %w", err)
		}
		backupPath = candidate
		break
	}
	complete := false
	defer func() {
		_ = backup.Close()
		if !complete {
			_ = os.Remove(backupPath)
		}
	}()
	if err := source.View(func(tx *legacybolt.Tx) error {
		_, err := tx.WriteTo(backup)
		return err
	}); err != nil {
		return err
	}
	if err := backup.Sync(); err != nil {
		return err
	}
	if err := backup.Close(); err != nil {
		return err
	}
	if err := syncDirectory(backupPath); err != nil {
		return err
	}
	complete = true
	return nil
}
