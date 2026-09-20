// Package workerdb provides the worker's embedded SQLite ledger. Its small
// transactional bucket interface keeps command outcomes and durable events in
// the same commit while indexing every lookup and ordered cursor in SQLite.
package workerdb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const applicationID = 0x574b4442 // WKDB

var ErrTimeout = errors.New("worker SQLite store: another worker holds the database lock")

type Options struct {
	Timeout  time.Duration
	WorkerID string
}

type DB struct {
	mu     sync.RWMutex
	db     *sql.DB
	path   string
	lock   *os.File
	closed bool
}

// Open acquires an exclusive worker lock before opening or migrating a private
// state file. SQLite itself remains readable by diagnostic connections in WAL
// mode, but a second worker cannot share its command ledger.
func Open(path string, mode os.FileMode, options *Options) (_ *DB, err error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.Contains(path, "://") {
		return nil, errors.New("worker SQLite store: database must be a local filesystem path")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if mode.Perm()&0o077 != 0 {
		return nil, errors.New("worker SQLite store: database permissions must be 0600 or stricter")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Canonicalize the directory so alternate symlink spellings share a lock.
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(directory, filepath.Base(path))
	optionsValue := Options{Timeout: time.Second}
	if options != nil {
		optionsValue = *options
	}
	lock, err := acquireLock(path+".lock", optionsValue.Timeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	if err = ensurePrivateFile(path); err != nil {
		return nil, err
	}
	format, err := fileFormat(path)
	if err != nil {
		return nil, err
	}
	if format == "legacy" {
		if err = migrateLegacy(path, optionsValue); err != nil {
			return nil, err
		}
		format = "sqlite"
	}
	db, err := openSQL(path, format == "empty", "WAL")
	if err != nil {
		return nil, err
	}
	return &DB{db: db, path: path, lock: lock}, nil
}

func ensurePrivateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr == nil {
			return file.Close()
		}
		if !errors.Is(createErr, os.ErrExist) {
			return createErr
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("worker SQLite store: state and sidecar files must be regular private files (0600 or stricter)")
	}
	return nil
}

func acquireLock(path string, timeout time.Duration) (*os.File, error) {
	if err := ensurePrivateFile(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			_ = file.Close()
			return nil, err
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, ErrTimeout
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}

func fileFormat(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	header := make([]byte, 16)
	n, err := io.ReadFull(file, header)
	if n == 0 && err == io.EOF {
		return "empty", nil
	}
	if err == nil && bytes.Equal(header, []byte("SQLite format 3\x00")) {
		return "sqlite", nil
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return "legacy", nil
}

func openSQL(path string, initialize bool, journal string) (_ *sql.DB, err error) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, statErr := os.Lstat(path + suffix); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			return nil, statErr
		}
		if err := ensurePrivateFile(path + suffix); err != nil {
			return nil, err
		}
	}
	params := url.Values{}
	params.Set("_txlock", "immediate")
	params.Add("_pragma", "busy_timeout(10000)")
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "synchronous(FULL)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err = db.Ping(); err != nil {
		return nil, err
	}
	var id, version int
	if err = db.QueryRow("PRAGMA application_id").Scan(&id); err != nil {
		return nil, err
	}
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if !initialize && id == 0 && version == 0 {
		// A crash after SQLite creates its header but before our schema
		// transaction commits leaves an empty, unmarked file. It is safe to
		// retry initialization only when no application objects exist.
		var objects int
		if err = db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&objects); err != nil {
			return nil, err
		}
		initialize = objects == 0
	}
	if !initialize && (id != applicationID || version != 1) {
		return nil, errors.New("worker SQLite store: unsupported database format or schema")
	}
	var mode string
	if err = db.QueryRow("PRAGMA journal_mode=" + journal).Scan(&mode); err != nil {
		return nil, err
	}
	if !strings.EqualFold(mode, journal) {
		return nil, errors.New("worker SQLite store: requested journal mode is unavailable")
	}
	if initialize {
		tx, beginErr := db.Begin()
		if beginErr != nil {
			return nil, beginErr
		}
		defer tx.Rollback()
		_, err = tx.Exec(`CREATE TABLE buckets (name BLOB NOT NULL PRIMARY KEY) WITHOUT ROWID;
CREATE TABLE entries (bucket BLOB NOT NULL, key BLOB NOT NULL, value BLOB NOT NULL,
PRIMARY KEY(bucket, key), FOREIGN KEY(bucket) REFERENCES buckets(name)) WITHOUT ROWID;
PRAGMA application_id=1464550466;
PRAGMA user_version=1;`)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	return db, nil
}

func (db *DB) Path() string { return db.path }

func (db *DB) Close() error {
	if db == nil {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	db.closed = true
	err := db.db.Close()
	return errors.Join(err, db.lock.Close())
}

func (db *DB) View(fn func(*Tx) error) error   { return db.transaction(false, fn) }
func (db *DB) Update(fn func(*Tx) error) error { return db.transaction(true, fn) }

func (db *DB) transaction(write bool, fn func(*Tx) error) error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closed {
		return errors.New("worker SQLite store: database is closed")
	}
	sqlTx, err := db.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: !write})
	if err != nil {
		return err
	}
	defer sqlTx.Rollback()
	tx := &Tx{tx: sqlTx, writable: write}
	if err := fn(tx); err != nil {
		return err
	}
	if tx.err != nil {
		return tx.err
	}
	return sqlTx.Commit()
}

// Reads retain their first SQL error because the ledger's existing Get/Cursor
// interface has no error return. View and Update always return it; Update never
// commits writes made after a failed read.
type Tx struct {
	tx       *sql.Tx
	writable bool
	err      error
}

func (tx *Tx) fail(err error) error {
	if tx.err == nil && err != nil {
		tx.err = err
	}
	return err
}

func (tx *Tx) Bucket(name []byte) *Bucket {
	if tx.err != nil {
		return nil
	}
	var exists int
	err := tx.tx.QueryRow("SELECT 1 FROM buckets WHERE name=?", nonnil(name)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		tx.fail(err)
		return nil
	}
	return &Bucket{tx: tx, name: bytes.Clone(name)}
}

func (tx *Tx) CreateBucketIfNotExists(name []byte) (*Bucket, error) {
	if !tx.writable {
		return nil, tx.fail(errors.New("worker SQLite store: read-only transaction"))
	}
	if len(name) == 0 {
		return nil, tx.fail(errors.New("worker SQLite store: bucket name is required"))
	}
	if tx.err != nil {
		return nil, tx.err
	}
	_, err := tx.tx.Exec("INSERT INTO buckets(name) VALUES(?) ON CONFLICT(name) DO NOTHING", name)
	if err != nil {
		return nil, tx.fail(err)
	}
	return &Bucket{tx: tx, name: bytes.Clone(name)}, nil
}

type Bucket struct {
	tx   *Tx
	name []byte
}

func (b *Bucket) Get(key []byte) []byte {
	if b == nil || b.tx.err != nil {
		return nil
	}
	var value []byte
	err := b.tx.tx.QueryRow("SELECT value FROM entries WHERE bucket=? AND key=?", b.name, nonnil(key)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		b.tx.fail(err)
		return nil
	}
	return nonnil(value)
}

func (b *Bucket) Put(key, value []byte) error {
	if b == nil {
		return errors.New("worker SQLite store: bucket does not exist")
	}
	if !b.tx.writable || len(key) == 0 {
		return b.tx.fail(errors.New("worker SQLite store: write requires a writable transaction and nonempty key"))
	}
	if b.tx.err != nil {
		return b.tx.err
	}
	_, err := b.tx.tx.Exec(`INSERT INTO entries(bucket,key,value) VALUES(?,?,?)
ON CONFLICT(bucket,key) DO UPDATE SET value=excluded.value WHERE entries.value != excluded.value`, b.name, key, nonnil(value))
	return b.tx.fail(err)
}

func (b *Bucket) Delete(key []byte) error {
	if b == nil {
		return errors.New("worker SQLite store: bucket does not exist")
	}
	if !b.tx.writable {
		return b.tx.fail(errors.New("worker SQLite store: read-only transaction"))
	}
	if b.tx.err != nil {
		return b.tx.err
	}
	_, err := b.tx.tx.Exec("DELETE FROM entries WHERE bucket=? AND key=?", b.name, nonnil(key))
	return b.tx.fail(err)
}

func (b *Bucket) ForEach(fn func(key, value []byte) error) error {
	if b == nil {
		return errors.New("worker SQLite store: bucket does not exist")
	}
	if b.tx.err != nil {
		return b.tx.err
	}
	if !b.tx.writable {
		// Inventory and command-ledger scans can contain thousands of rows.
		// One ordered SQLite statement streams them without an N+1 query
		// loop or retaining the complete bucket in Go memory.
		rows, err := b.tx.tx.Query("SELECT key,value FROM entries WHERE bucket=? ORDER BY key", b.name)
		if err != nil {
			return b.tx.fail(err)
		}
		defer rows.Close()
		for rows.Next() {
			var key, value []byte
			if err := rows.Scan(&key, &value); err != nil {
				return b.tx.fail(err)
			}
			if err := fn(key, nonnil(value)); err != nil {
				return err
			}
			if b.tx.err != nil {
				return b.tx.err
			}
		}
		return b.tx.fail(rows.Err())
	}
	// Writable callbacks may replace or delete the current entry. Keyset
	// iteration closes each statement before invoking those callbacks.
	cursor := b.Cursor()
	for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
		if err := fn(key, value); err != nil {
			return err
		}
	}
	return b.tx.err
}

// Cursor performs indexed keyset reads and retains only one record. Deleting
// its current key does not advance or skip the next key.
type Cursor struct {
	bucket *Bucket
	key    []byte
	ended  bool
}

func (b *Bucket) Cursor() *Cursor { return &Cursor{bucket: b} }

func (c *Cursor) First() ([]byte, []byte)          { return c.read("", nil) }
func (c *Cursor) Seek(key []byte) ([]byte, []byte) { return c.read(" AND key>=?", nonnil(key)) }
func (c *Cursor) Next() ([]byte, []byte) {
	if c.ended {
		return nil, nil
	}
	if c.key == nil {
		return c.First()
	}
	return c.read(" AND key>?", c.key)
}

func (c *Cursor) read(condition string, start []byte) ([]byte, []byte) {
	if c.bucket == nil || c.bucket.tx.err != nil {
		return nil, nil
	}
	arguments := []any{c.bucket.name}
	if condition != "" {
		arguments = append(arguments, start)
	}
	var key, value []byte
	err := c.bucket.tx.tx.QueryRow("SELECT key,value FROM entries WHERE bucket=?"+condition+" ORDER BY key LIMIT 1", arguments...).Scan(&key, &value)
	if errors.Is(err, sql.ErrNoRows) {
		c.ended = true
		return nil, nil
	}
	if err != nil {
		c.bucket.tx.fail(err)
		return nil, nil
	}
	c.key, c.ended = bytes.Clone(key), false
	return key, nonnil(value)
}

func (c *Cursor) Delete() error {
	if c.key == nil || c.ended {
		return errors.New("worker SQLite store: cursor is not positioned")
	}
	return c.bucket.Delete(c.key)
}

func nonnil(value []byte) []byte {
	if value == nil {
		return []byte{}
	}
	return value
}

func syncDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("worker SQLite store: sync state directory: %w", err)
	}
	return nil
}
