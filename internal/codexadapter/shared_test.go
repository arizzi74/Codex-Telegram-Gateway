package codexadapter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSharedServerArgsForcePrivateUnixListener(t *testing.T) {
	got, err := sharedServerArgs([]string{"app-server", "--listen", "stdio://", "-c", "model=\"gpt\""}, "/tmp/private.sock")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app-server", "-c", "model=\"gpt\"", "--listen", "unix:///tmp/private.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	if _, err := sharedServerArgs([]string{"exec"}, "/tmp/private.sock"); err == nil {
		t.Fatal("accepted non-app-server args")
	}
}

// TestRealSharedRuntimeSmoke is deliberately opt-in. It never starts a turn or
// creates a thread: it only initializes the shared proxy and reads thread
// metadata from an installed, authenticated local Codex CLI.
func TestRealSharedRuntimeSmoke(t *testing.T) {
	if os.Getenv("CODEX_REAL_TEST") != "1" {
		t.Skip("set CODEX_REAL_TEST=1 to run against the installed Codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runtime, err := StartShared(ctx, Config{WorkingDirectory: t.TempDir(), ClientInfo: ClientInfo{Name: "telegramgw-real-smoke"}}, filepath.Join(t.TempDir(), "private", "app-server.sock"))
	if err != nil {
		t.Fatalf("start shared runtime: %v", err)
	}
	defer runtime.Close()
	if runtime.PID() == 0 || runtime.ProxyPID() == 0 {
		t.Fatalf("missing owned process PID: app=%d proxy=%d", runtime.PID(), runtime.ProxyPID())
	}
	if _, err := runtime.LoadedThreads(ctx, "", 1); err != nil {
		t.Fatalf("loaded thread list: %v", err)
	}
	if _, err := runtime.ListThreads(ctx, "", 1); err != nil {
		t.Fatalf("stored thread list: %v", err)
	}
}

func TestPrepareSocketPathRequiresPrivateNewSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "app.sock")
	got, err := prepareSocketPath(path)
	if err != nil || got != path {
		t.Fatalf("prepare path = %q, %v", got, err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("directory permissions = %v, %v", info.Mode(), err)
	}
	if err := os.WriteFile(path, []byte("not-a-socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSocketPath(path); err == nil {
		t.Fatal("accepted existing path")
	}

	insecure := filepath.Join(t.TempDir(), "insecure")
	if err := os.Mkdir(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSocketPath(filepath.Join(insecure, "app.sock")); err == nil {
		t.Fatal("accepted non-private directory")
	}
}
