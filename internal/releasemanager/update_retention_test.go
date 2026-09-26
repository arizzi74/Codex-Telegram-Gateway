package releasemanager

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func retentionFixture(t *testing.T, component string) *Layout {
	t.Helper()
	l := &Layout{Component: component, Backups: filepath.Join(t.TempDir(), "updates")}
	if err := os.MkdirAll(l.Backups, 0o700); err != nil {
		t.Fatal(err)
	}
	return l
}

func retentionSnapshot(t *testing.T, l *Layout, day int, version string) string {
	t.Helper()
	name := fmt.Sprintf("202609%02dT120000Z-%d", day, day)
	dir := filepath.Join(l.Backups, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(dir, "00-update.json"), map[string]any{
		"schema": 1, "component": l.Component, "repo": DefaultRepo, "version": version,
	}); err != nil {
		t.Fatal(err)
	}
	files := []string{"02-codex-" + l.Component, "03-codex-telegramgw"}
	if l.Component == "worker" {
		files = append(files, "04-codex-local")
	} else {
		files = append(files, "gateway.db")
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("snapshot "+version), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

func retentionNames(t *testing.T, l *Layout) []string {
	t.Helper()
	entries, err := os.ReadDir(l.Backups)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	return names
}

func TestPruneUpdateBackupsRetainsThreeDistinctPreviousVersions(t *testing.T) {
	l := retentionFixture(t, "worker")
	v19 := retentionSnapshot(t, l, 1, "v1.9.0")
	retentionSnapshot(t, l, 2, "v1.10.0")
	v111 := retentionSnapshot(t, l, 3, "v1.11.0")
	retentionSnapshot(t, l, 4, "v2.0.0")
	// The latest timestamp is not necessarily the highest previous version.
	retentionSnapshot(t, l, 9, "v1.0.0")
	v110 := retentionSnapshot(t, l, 7, "1.10.0")
	incomplete := retentionSnapshot(t, l, 8, "v1.12.0")
	if err := os.Remove(filepath.Join(l.Backups, incomplete, "04-codex-local")); err != nil {
		t.Fatal(err)
	}
	pending := retentionSnapshot(t, l, 10, "v1.11.0")
	if err := WriteJSON(filepath.Join(l.Backups, pending, "00-update.json"), map[string]any{
		"schema": 1, "component": l.Component, "repo": DefaultRepo, "version": "v1.11.0", "pending": true,
	}); err != nil {
		t.Fatal(err)
	}
	empty := retentionSnapshot(t, l, 11, "v1.10.0")
	if err := os.WriteFile(filepath.Join(l.Backups, empty, "02-codex-worker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := pruneUpdateBackups(l, "v2.0.0")
	if err != nil || removed != 6 {
		t.Fatalf("prune = %d, %v; wanted 6 removed", removed, err)
	}
	want := []string{v19, v110, v111}
	sort.Strings(want)
	if got := retentionNames(t, l); !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining snapshots = %v, want %v", got, want)
	}
	if removed, err := pruneUpdateBackups(l, "2.0.0"); err != nil || removed != 0 {
		t.Fatalf("second prune = %d, %v", removed, err)
	}
}

func TestPruneUpdateBackupsPreservesUnrecognizedEntries(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"malformed metadata", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), "{invalid")
		}},
		{"missing metadata", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "00-update.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown file", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "notes.txt"), "keep me")
		}},
		{"directory instead of file", func(t *testing.T, dir string) {
			path := filepath.Join(dir, "04-codex-local")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"future version", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), `{"schema":1,"component":"worker","repo":"owner/repo","version":"v3.0.0"}`)
		}},
		{"wrong component", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), `{"schema":1,"component":"gateway","repo":"owner/repo","version":"v1.0.0"}`)
		}},
		{"unknown schema", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), `{"schema":2,"component":"worker","repo":"owner/repo","version":"v1.0.0"}`)
		}},
		{"invalid repository", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), `{"schema":1,"component":"worker","repo":"../repo","version":"v1.0.0"}`)
		}},
		{"invalid version", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), `{"schema":1,"component":"worker","repo":"owner/repo","version":"v1.0.0-beta.1"}`)
		}},
		{"invalid pending", func(t *testing.T, dir string) {
			retentionWrite(t, filepath.Join(dir, "00-update.json"), `{"schema":1,"component":"worker","repo":"owner/repo","version":"v1.0.0","pending":"false"}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			l := retentionFixture(t, "worker")
			preserved := retentionSnapshot(t, l, 1, "v1.0.0")
			test.mutate(t, filepath.Join(l.Backups, preserved))
			retentionSnapshot(t, l, 2, "v2.0.0")
			removed, err := pruneUpdateBackups(l, "v2.0.0")
			if removed != 1 || err == nil || !strings.Contains(err.Error(), preserved) {
				t.Fatalf("prune = %d, %v; expected one removal and preservation warning", removed, err)
			}
			if got := retentionNames(t, l); !reflect.DeepEqual(got, []string{preserved}) {
				t.Fatalf("remaining snapshots = %v", got)
			}
		})
	}
}

func TestPruneUpdateBackupsDoesNotFollowSymlinks(t *testing.T) {
	l := retentionFixture(t, "worker")
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	retentionWrite(t, sentinel, "untouched")
	linkedDir := "20260901T120000Z-1"
	if err := os.Symlink(outside, filepath.Join(l.Backups, linkedDir)); err != nil {
		t.Fatal(err)
	}
	for day, file := range map[int]string{2: "00-update.json", 3: "02-codex-worker"} {
		name := retentionSnapshot(t, l, day, "v1.0.0")
		path := filepath.Join(l.Backups, name, file)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sentinel, path); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 0 || err == nil {
		t.Fatalf("prune = %d, %v; expected all symlink entries preserved", removed, err)
	}
	if data := updateRead(t, sentinel); data != "untouched" {
		t.Fatalf("external sentinel changed: %q", data)
	}
	linkRoot := filepath.Join(t.TempDir(), "updates")
	if err := os.Symlink(l.Backups, linkRoot); err != nil {
		t.Fatal(err)
	}
	l.Backups = linkRoot
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 0 || err == nil {
		t.Fatalf("symlinked root prune = %d, %v", removed, err)
	}
}

func TestPruneUpdateBackupsValidatesTimestampNames(t *testing.T) {
	l := retentionFixture(t, "worker")
	want := []string{"20260230T120000Z-1", "notes", "20260901T120000Z-not-a-random-suffix"}
	for _, name := range want {
		if err := os.Mkdir(filepath.Join(l.Backups, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 0 || err == nil {
		t.Fatalf("prune = %d, %v", removed, err)
	}
	sort.Strings(want)
	if got := retentionNames(t, l); !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining entries = %v", got)
	}
}

func TestPruneUpdateBackupsGatewayRequiresDatabaseSnapshot(t *testing.T) {
	l := retentionFixture(t, "gateway")
	complete := retentionSnapshot(t, l, 1, "v1.0.0")
	for day, version := range map[int]string{2: "v1.0.0", 3: "v1.1.0"} {
		name := retentionSnapshot(t, l, day, version)
		if err := os.Remove(filepath.Join(l.Backups, name, "gateway.db")); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 2 || err != nil {
		t.Fatalf("prune = %d, %v", removed, err)
	}
	if got := retentionNames(t, l); !reflect.DeepEqual(got, []string{complete}) {
		t.Fatalf("remaining entries = %v; want complete gateway backup", got)
	}
}

func TestPruneUpdateBackupsGatewaySQLiteSidecars(t *testing.T) {
	l := retentionFixture(t, "gateway")
	source := filepath.Join(t.TempDir(), "source.db")
	db := openUpdateDB(t, source)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE data(value TEXT); INSERT INTO data VALUES ('committed in WAL')"); err != nil {
		t.Fatal(err)
	}
	if data := updateRead(t, source+"-wal"); len(data) == 0 {
		t.Fatal("test requires a live nonempty WAL")
	}
	var want []string
	preservedFiles := make(map[string]string)
	for day, version := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0", "v1.2.0", "v2.0.0"} {
		name := retentionSnapshot(t, l, day+1, version)
		dir := filepath.Join(l.Backups, name)
		backup := filepath.Join(dir, "gateway.db")
		if err := os.Remove(backup); err != nil {
			t.Fatal(err)
		}
		if err := sqliteBackup(t.Context(), source, backup); err != nil {
			t.Fatal(err)
		}
		// Match the real gateway snapshots, including sidecars emitted by the
		// SQLite integrity check rather than only synthetic companion files.
		for _, suffix := range []string{"-shm", "-wal"} {
			if info, err := os.Lstat(backup + suffix); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("SQLite backup did not emit regular %s: %v", suffix, err)
			}
		}
		retentionWrite(t, backup+"-journal", "optional rollback journal")
		if day == 1 || day == 3 || day == 4 {
			want = append(want, name)
			for _, file := range []string{"gateway.db", "gateway.db-shm", "gateway.db-wal", "gateway.db-journal"} {
				path := filepath.Join(dir, file)
				preservedFiles[path] = updateRead(t, path)
			}
		}
	}
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 3 || err != nil {
		t.Fatalf("prune = %d, %v; expected old version, duplicate and current-version copies removed", removed, err)
	}
	if got := retentionNames(t, l); !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining entries = %v, want %v", got, want)
	}
	for path, original := range preservedFiles {
		if data := updateRead(t, path); data != original {
			t.Fatalf("retained database snapshot file changed: %s", path)
		}
	}
}

func TestPruneUpdateBackupsGatewayRejectsUnsafeSidecars(t *testing.T) {
	for _, suffix := range []string{"-shm", "-wal", "-journal"} {
		for _, kind := range []string{"symlink", "directory"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				l := retentionFixture(t, "gateway")
				name := retentionSnapshot(t, l, 1, "v2.0.0")
				path := filepath.Join(l.Backups, name, "gateway.db"+suffix)
				sentinel := filepath.Join(t.TempDir(), "keep")
				retentionWrite(t, sentinel, "untouched")
				if kind == "symlink" {
					if err := os.Symlink(sentinel, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 0 || err == nil {
					t.Fatalf("unsafe sidecar prune = %d, %v", removed, err)
				}
				if got := retentionNames(t, l); !reflect.DeepEqual(got, []string{name}) {
					t.Fatalf("unsafe snapshot removed: %v", got)
				}
				if data := updateRead(t, sentinel); data != "untouched" {
					t.Fatalf("external sentinel changed: %q", data)
				}
			})
		}
	}
}

func TestPruneUpdateBackupsMissingRootAndInvalidCurrentVersion(t *testing.T) {
	l := &Layout{Component: "worker", Backups: filepath.Join(t.TempDir(), "absent")}
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 0 || err != nil {
		t.Fatalf("missing root prune = %d, %v", removed, err)
	}
	if _, err := os.Stat(l.Backups); !os.IsNotExist(err) {
		t.Fatalf("missing backup root was created: %v", err)
	}
	l = retentionFixture(t, "worker")
	name := retentionSnapshot(t, l, 1, "v2.0.0")
	if removed, err := pruneUpdateBackups(l, "invalid"); removed != 0 || err == nil {
		t.Fatalf("invalid version prune = %d, %v", removed, err)
	}
	if got := retentionNames(t, l); !reflect.DeepEqual(got, []string{name}) {
		t.Fatalf("invalid version removed backups: %v", got)
	}
}

func retentionWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
