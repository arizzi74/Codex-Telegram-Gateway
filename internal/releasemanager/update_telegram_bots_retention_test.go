package releasemanager

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPruneGatewayBackupsRetainsThreeVersionsWithOptionalTelegramBotKeys(t *testing.T) {
	l := retentionFixture(t, "gateway")
	key := bytes.Repeat([]byte{0x42}, 32)
	var retained []string
	for index, version := range []string{"v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0", "v2.0.0"} {
		name := retentionSnapshot(t, l, index+1, version)
		if index != 1 { // A valid older backup has no bot key.
			if err := os.WriteFile(filepath.Join(l.Backups, name, "gateway.db.telegram-bots.key"), key, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if index >= 1 && index <= 3 {
			retained = append(retained, name)
		}
	}
	if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 2 || err != nil {
		t.Fatalf("prune with bot keys = %d, %v; want oldest and current snapshots removed", removed, err)
	}
	if got := retentionNames(t, l); !reflect.DeepEqual(got, retained) {
		t.Fatalf("remaining snapshots = %v, want %v", got, retained)
	}
	for _, name := range retained[1:] {
		path := filepath.Join(l.Backups, name, "gateway.db.telegram-bots.key")
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, key) {
			t.Fatalf("retained bot key changed: %v", err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("retained bot key permissions changed: %v", err)
		}
	}
}

func TestPruneGatewayBackupsPreservesInvalidTelegramBotKeySnapshots(t *testing.T) {
	for _, kind := range []string{"short", "public", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			l := retentionFixture(t, "gateway")
			name := retentionSnapshot(t, l, 1, "v2.0.0")
			path := filepath.Join(l.Backups, name, "gateway.db.telegram-bots.key")
			sentinel := filepath.Join(t.TempDir(), "outside-key")
			key := bytes.Repeat([]byte{0x42}, 32)
			if err := os.WriteFile(sentinel, key, 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "short":
				err = os.WriteFile(path, key[:31], 0o600)
			case "public":
				err = os.WriteFile(path, key, 0o600)
				if err == nil {
					err = os.Chmod(path, 0o644)
				}
			case "symlink":
				err = os.Symlink(sentinel, path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if removed, err := pruneUpdateBackups(l, "v2.0.0"); removed != 0 || err == nil {
				t.Fatalf("invalid key snapshot prune = %d, %v", removed, err)
			}
			if got := retentionNames(t, l); !reflect.DeepEqual(got, []string{name}) {
				t.Fatalf("invalid key snapshot was deleted: %v", got)
			}
			if data, err := os.ReadFile(sentinel); err != nil || !bytes.Equal(data, key) {
				t.Fatalf("outside key changed: %v", err)
			}
		})
	}
}
