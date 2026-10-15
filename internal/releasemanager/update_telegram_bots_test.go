package releasemanager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGatewayUpdateBacksUpPrivateTelegramBotKey(t *testing.T) {
	l, _, _ := updateFixture(t, "gateway")
	database, err := openGatewayDatabase(l)
	if err != nil {
		t.Fatal(err)
	}
	defer database.root.Close()
	backup := t.TempDir()
	if err := backupTelegramBotKey(database, backup); err != nil {
		t.Fatal(err)
	}
	keyPath := database.path + ".telegram-bots.key"
	key := bytes.Repeat([]byte{0x42}, 32)
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backupTelegramBotKey(database, backup); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(backup, "gateway.db.telegram-bots.key")
	data, err := os.ReadFile(copyPath)
	if err != nil || !bytes.Equal(data, key) {
		t.Fatal("backup did not preserve the encryption key")
	}
	info, err := os.Stat(copyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("backup key permissions are not private")
	}
	if err := os.Chmod(keyPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := backupTelegramBotKey(database, t.TempDir()); err != nil {
		t.Fatalf("private read-only key rejected: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(copyPath, keyPath); err != nil {
		t.Fatal(err)
	}
	if err := backupTelegramBotKey(database, t.TempDir()); err == nil {
		t.Fatal("key symlink was followed")
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := backupTelegramBotKey(database, t.TempDir()); err == nil {
		t.Fatal("nonprivate key accepted")
	}
}
