package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// All stored dates use the same sortable representation, including SQL defaults
// and dates imported from PostgreSQL. Fixed precision is necessary: RFC3339Nano
// omits trailing zeroes and would not preserve chronological TEXT ordering.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"
const sqliteNow = "(strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')"

// Each Store serializes access through one connection. IMMEDIATE transactions
// acquire the database's writer reservation before any reads, so read/modify/
// write operations cannot race or fail while upgrading a stale read snapshot.
// The busy timeout also coordinates separate gateway/admin processes. WAL keeps
// their reads independent of an active writer; FULL sync makes commits durable.
type dbPool struct {
	db   *sql.DB
	path string
}

type dbTx struct{ tx *sql.Tx }
type dbRows struct{ *sql.Rows }
type dbRow struct{ row *sql.Row }
type dbCommandTag struct{ count int64 }
type TxOptions struct{}

func (t dbCommandTag) RowsAffected() int64 { return t.count }

// Serialize new-file creation in this process. Closing an unrelated descriptor
// for a SQLite file can release POSIX file locks held by another connection.
var sqliteOpenMu sync.Mutex

func openSQLite(ctx context.Context, path string) (*dbPool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("registry: database path is required")
	}
	if path == ":memory:" || strings.HasPrefix(path, "file:") || strings.Contains(path, "://") {
		return nil, errors.New("registry: database must be a local filesystem path, not a URI or memory database")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("registry: resolve database path: %w", err)
	}
	if err := prepareSQLiteFile(path); err != nil {
		return nil, err
	}
	// Existing WAL/SHM files may survive an interrupted process. Protect those
	// too; newly created sidecars inherit the database's private file mode.
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("registry: inspect SQLite sidecar: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("registry: SQLite sidecar must be a regular file")
		}
		if err := os.Chmod(path+suffix, 0600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("registry: secure SQLite sidecar permissions: %w", err)
		}
	}
	params := url.Values{}
	params.Set("_txlock", "immediate")
	params.Set("_busy_timeout", "10000")
	params.Set("_foreign_keys", "on")
	params.Set("_journal_mode", "WAL")
	params.Set("_synchronous", "FULL")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("registry: open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	pool := &dbPool{db: db, path: path}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("registry: initialize SQLite: %w", err)
	}
	var mode string
	if err := pool.QueryRow(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		pool.Close()
		if err != nil {
			return nil, fmt.Errorf("registry: verify SQLite WAL: %w", err)
		}
		return nil, fmt.Errorf("registry: SQLite WAL unavailable (journal mode %q)", mode)
	}
	return pool, nil
}

func prepareSQLiteFile(path string) error {
	sqliteOpenMu.Lock()
	defer sqliteOpenMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("registry: create database directory: %w", err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if createErr == nil {
			if err := file.Close(); err != nil {
				return fmt.Errorf("registry: close new database: %w", err)
			}
			return nil
		}
		if !errors.Is(createErr, os.ErrExist) {
			return fmt.Errorf("registry: create database: %w", createErr)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("registry: inspect database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("registry: database path must refer to a regular file, not a symlink or directory")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("registry: secure database permissions: %w", err)
	}
	return nil
}

func (p *dbPool) Close()                         { _ = p.db.Close() }
func (p *dbPool) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *dbPool) Begin(ctx context.Context) (*dbTx, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &dbTx{tx: tx}, nil
}
func (p *dbPool) BeginTx(ctx context.Context, _ TxOptions) (*dbTx, error) {
	return p.Begin(ctx)
}
func (p *dbPool) Exec(ctx context.Context, query string, args ...any) (dbCommandTag, error) {
	result, err := p.db.ExecContext(ctx, sqlitePlaceholders(query), sqliteArgs(args)...)
	return sqliteCommandTag(result, err)
}
func (p *dbPool) Query(ctx context.Context, query string, args ...any) (*dbRows, error) {
	rows, err := p.db.QueryContext(ctx, sqlitePlaceholders(query), sqliteArgs(args)...)
	if err != nil {
		return nil, err
	}
	return &dbRows{Rows: rows}, nil
}
func (p *dbPool) QueryRow(ctx context.Context, query string, args ...any) *dbRow {
	return &dbRow{row: p.db.QueryRowContext(ctx, sqlitePlaceholders(query), sqliteArgs(args)...)}
}
func (t *dbTx) Exec(ctx context.Context, query string, args ...any) (dbCommandTag, error) {
	result, err := t.tx.ExecContext(ctx, sqlitePlaceholders(query), sqliteArgs(args)...)
	return sqliteCommandTag(result, err)
}
func (t *dbTx) Query(ctx context.Context, query string, args ...any) (*dbRows, error) {
	rows, err := t.tx.QueryContext(ctx, sqlitePlaceholders(query), sqliteArgs(args)...)
	if err != nil {
		return nil, err
	}
	return &dbRows{Rows: rows}, nil
}
func (t *dbTx) QueryRow(ctx context.Context, query string, args ...any) *dbRow {
	return &dbRow{row: t.tx.QueryRowContext(ctx, sqlitePlaceholders(query), sqliteArgs(args)...)}
}
func (t *dbTx) Commit(_ context.Context) error   { return t.tx.Commit() }
func (t *dbTx) Rollback(_ context.Context) error { return t.tx.Rollback() }
func (r *dbRows) Scan(dest ...any) error         { return r.Rows.Scan(sqliteScanTargets(dest)...) }
func (r *dbRow) Scan(dest ...any) error          { return r.row.Scan(sqliteScanTargets(dest)...) }

func sqliteCommandTag(result sql.Result, err error) (dbCommandTag, error) {
	if err != nil {
		return dbCommandTag{}, err
	}
	count, err := result.RowsAffected()
	return dbCommandTag{count: count}, err
}

func sqliteArgs(args []any) []any {
	values := make([]any, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case time.Time:
			values[i] = v.UTC().Format(sqliteTimeLayout)
		case *time.Time:
			if v != nil {
				values[i] = v.UTC().Format(sqliteTimeLayout)
			}
		case json.RawMessage:
			if v != nil {
				values[i] = string(v)
			}
		case *json.RawMessage:
			if v != nil && *v != nil {
				values[i] = string(*v)
			}
		default:
			values[i] = arg
		}
	}
	return values
}

func sqliteScanTargets(dest []any) []any {
	values := make([]any, len(dest))
	for i, target := range dest {
		switch target.(type) {
		case *time.Time, **time.Time:
			values[i] = sqliteDateScanner{target: target}
		case *json.RawMessage:
			values[i] = sqliteJSONScanner{target: target.(*json.RawMessage)}
		default:
			values[i] = target
		}
	}
	return values
}

type sqliteDateScanner struct{ target any }

func (s sqliteDateScanner) Scan(src any) error {
	if src == nil {
		if target, ok := s.target.(**time.Time); ok {
			*target = nil
			return nil
		}
		return errors.New("registry: NULL in required timestamp")
	}
	var value time.Time
	var err error
	switch v := src.(type) {
	case time.Time:
		value = v.UTC()
	case string:
		value, err = time.Parse(time.RFC3339Nano, v)
	case []byte:
		value, err = time.Parse(time.RFC3339Nano, string(v))
	default:
		return fmt.Errorf("registry: invalid timestamp type %T", src)
	}
	if err != nil {
		return fmt.Errorf("registry: invalid stored timestamp: %w", err)
	}
	value = value.UTC()
	switch target := s.target.(type) {
	case *time.Time:
		*target = value
	case **time.Time:
		*target = &value
	}
	return nil
}

type sqliteJSONScanner struct{ target *json.RawMessage }

func (s sqliteJSONScanner) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*s.target = nil
	case string:
		*s.target = append((*s.target)[:0], v...)
	case []byte:
		*s.target = append((*s.target)[:0], v...)
	default:
		return fmt.Errorf("registry: invalid JSON storage type %T", src)
	}
	return nil
}

// SQLite's numbered positional placeholders are ?N. Translate only placeholders;
// SQL syntax itself is native SQLite and quoted values/comments stay untouched.
func sqlitePlaceholders(query string) string {
	var out strings.Builder
	out.Grow(len(query))
	for i := 0; i < len(query); {
		start := i
		switch {
		case query[i] == '\'' || query[i] == '"' || query[i] == '`':
			quote := query[i]
			i++
			for i < len(query) {
				if query[i] != quote {
					i++
					continue
				}
				i++
				if i < len(query) && query[i] == quote {
					i++
					continue
				}
				break
			}
		case query[i] == '[':
			i++
			for i < len(query) && query[i] != ']' {
				i++
			}
			if i < len(query) {
				i++
			}
		case strings.HasPrefix(query[i:], "--"):
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case strings.HasPrefix(query[i:], "/*"):
			i += 2
			for i < len(query) && !strings.HasPrefix(query[i:], "*/") {
				i++
			}
			if i < len(query) {
				i += 2
			}
		case query[i] == '$' && i+1 < len(query) && query[i+1] >= '1' && query[i+1] <= '9':
			out.WriteByte('?')
			i++
			for i < len(query) && query[i] >= '0' && query[i] <= '9' {
				out.WriteByte(query[i])
				i++
			}
			continue
		default:
			i++
		}
		out.WriteString(query[start:i])
	}
	return out.String()
}
