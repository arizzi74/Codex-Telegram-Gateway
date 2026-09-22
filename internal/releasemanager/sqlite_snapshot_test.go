package releasemanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestRootedSQLiteSnapshotPreservesWALWithoutTouchingSource(t *testing.T) {
	l, _, _ := updateFixture(t, "gateway")
	path := filepath.Join(l.DataRoot, "gateway.db")
	db := openUpdateDB(t, path)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO data(rowid,value) VALUES (123,'committed in WAL')"); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		before[suffix] = updateRead(t, path+suffix)
	}
	if len(before["-wal"]) == 0 {
		t.Fatal("test requires uncheckpointed WAL contents")
	}
	database, err := openGatewayDatabase(l)
	if err != nil {
		t.Fatal(err)
	}
	defer database.root.Close()
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := backupSQLiteFromRoot(t.Context(), database, dest); err != nil {
		t.Fatal(err)
	}
	copy := openUpdateDB(t, dest)
	var value string
	if err := copy.QueryRow("SELECT value FROM data WHERE rowid=123").Scan(&value); err != nil || value != "committed in WAL" {
		t.Fatalf("snapshot WAL row = %q, %v", value, err)
	}
	for suffix, contents := range before {
		if updateRead(t, path+suffix) != contents {
			t.Fatalf("snapshot modified source %q", suffix)
		}
	}
	if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions = %v, %v", info, err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(dest), ".sqlite-snapshot-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("private snapshot staging leaked: %v, %v", leftovers, err)
	}
}

func TestRootedSQLiteSnapshotRejectsReplacedPaths(t *testing.T) {
	for _, scenario := range []string{"database-symlink", "wal-symlink", "journal-symlink", "database-hardlink", "wal-hardlink", "journal-hardlink", "database-fifo", "root-symlink", "root-replaced", "parent-symlink"} {
		t.Run(scenario, func(t *testing.T) {
			l, _, _ := updateFixture(t, "gateway")
			path := filepath.Join(l.DataRoot, "gateway.db")
			if scenario == "parent-symlink" {
				nested := filepath.Join(l.DataRoot, "nested")
				if err := os.Mkdir(nested, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, filepath.Join(nested, "gateway.db")); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(nested, "gateway.db")
				if err := WriteJSON(l.Config, map[string]any{"database_path": path}); err != nil {
					t.Fatal(err)
				}
			}
			database, err := openGatewayDatabase(l)
			if err != nil {
				t.Fatal(err)
			}
			defer database.root.Close()
			outside := filepath.Join(t.TempDir(), "private-file")
			const sentinel = "private file must not be copied or changed"
			if err := os.WriteFile(outside, []byte(sentinel), 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "database-symlink", "wal-symlink", "journal-symlink", "database-hardlink", "wal-hardlink", "journal-hardlink":
				target := path
				if strings.HasPrefix(scenario, "wal-") {
					target += "-wal"
				}
				if strings.HasPrefix(scenario, "journal-") {
					target += "-journal"
				}
				if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				if strings.HasSuffix(scenario, "-hardlink") {
					err = os.Link(outside, target)
				} else {
					err = os.Symlink(outside, target)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "database-fifo":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "root-symlink", "root-replaced":
				if err := os.Rename(l.DataRoot, l.DataRoot+"-original"); err != nil {
					t.Fatal(err)
				}
				if scenario == "root-symlink" {
					err = os.Symlink(filepath.Dir(outside), l.DataRoot)
				} else {
					err = os.Mkdir(l.DataRoot, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				parent := filepath.Dir(path)
				if err := os.Rename(parent, parent+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), parent); err != nil {
					t.Fatal(err)
				}
			}
			dest := filepath.Join(t.TempDir(), "backup.db")
			if err := backupSQLiteFromRoot(t.Context(), database, dest); err == nil {
				t.Fatal("unsafe source path accepted")
			}
			if got := updateRead(t, outside); got != sentinel {
				t.Fatalf("snapshot changed unrelated source: %q", got)
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe source created a backup: %v", err)
			}
		})
	}
}

func TestRootedSQLiteSnapshotHonorsCancellation(t *testing.T) {
	l, _, _ := updateFixture(t, "gateway")
	database, err := openGatewayDatabase(l)
	if err != nil {
		t.Fatal(err)
	}
	defer database.root.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := backupSQLiteFromRoot(ctx, database, dest); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backup = %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled snapshot created a backup: %v", err)
	}
}

type snapshotMutationContext struct {
	context.Context
	once   sync.Once
	mutate func()
}

func (c *snapshotMutationContext) Err() error {
	// The snapshot checks cancellation after opening all source descriptors.
	// Mutate there to exercise the race deterministically without live services.
	c.once.Do(c.mutate)
	return c.Context.Err()
}

func TestRootedSQLiteSnapshotRejectsRecoveryFileAppearingDuringCopy(t *testing.T) {
	for _, suffix := range []string{"-wal", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			l, _, _ := updateFixture(t, "gateway")
			database, err := openGatewayDatabase(l)
			if err != nil {
				t.Fatal(err)
			}
			defer database.root.Close()
			ctx := &snapshotMutationContext{Context: t.Context(), mutate: func() {
				if err := os.WriteFile(filepath.Join(l.DataRoot, "gateway.db"+suffix), []byte("new recovery data"), 0600); err != nil {
					t.Fatal(err)
				}
			}}
			dest := filepath.Join(t.TempDir(), "backup.db")
			if err := backupSQLiteFromRoot(ctx, database, dest); err == nil || !strings.Contains(err.Error(), "recovery files changed") {
				t.Fatalf("new recovery file silently omitted: %v", err)
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("torn snapshot created a backup: %v", err)
			}
		})
	}
}
