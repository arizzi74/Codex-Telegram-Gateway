package worker

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func attachmentTestDir(t *testing.T) string {
	t.Helper()
	path, err := os.MkdirTemp("", "attach-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	return path
}

func TestAttachmentSocketIdentity(t *testing.T) {
	state := filepath.Join(attachmentTestDir(t), "worker.db")
	first, err := attachmentSocketPath(state, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct{ state, profile string }{{state, "other"}, {state + ".other", "main"}} {
		other, err := attachmentSocketPath(input.state, input.profile)
		if err != nil || other == first {
			t.Fatalf("different worker/profile shares a socket: %q, %v", other, err)
		}
	}
	canonical, err := attachmentSocketPath(filepath.Dir(state)+"/./worker.db", "main")
	if err != nil || canonical != first {
		t.Fatalf("canonical path changed identity: %q, %v", canonical, err)
	}
	for _, input := range []struct{ state, profile string }{{"relative.db", "main"}, {state, ""}, {"/" + strings.Repeat("a", 100) + "/worker.db", "main"}} {
		if _, err := attachmentSocketPath(input.state, input.profile); err == nil {
			t.Fatalf("accepted invalid socket identity: %#v", input)
		}
	}
}

func TestAttachmentRebindJoinsPreviousProxy(t *testing.T) {
	dir := attachmentTestDir(t)
	m := &RuntimeManager{cfg: config.WorkerConfig{StateFile: filepath.Join(dir, "worker.db")}, attachments: make(map[string]*attachmentProxy)}
	runtime := protocol.Runtime{ID: "runtime", ProfileID: "main", Generation: 1, PID: 100}
	first, path, err := m.openAttachment(runtime, filepath.Join(dir, "first-backend.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.close)
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions: %#v, %v", info, err)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || parent.Mode().Perm() != 0o700 {
		t.Fatalf("parent permissions: %#v, %v", parent, err)
	}
	runtime.Generation, runtime.PID = 2, 200
	second, secondPath, err := m.openAttachment(runtime, filepath.Join(dir, "second-backend.sock"))
	if err != nil || secondPath != path {
		t.Fatalf("runtime restart changed public socket: %q -> %q, %v", path, secondPath, err)
	}
	t.Cleanup(second.close)
	// The former app-server's Done goroutine may call close a second time.
	// It must never remove the current generation's endpoint.
	first.close()
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("late previous close removed replacement: %v", err)
	}
	connection.Close()
	second.close()
	// A fresh manager after a worker process restart gets the same endpoint.
	restarted := &RuntimeManager{cfg: m.cfg, attachments: make(map[string]*attachmentProxy)}
	third, thirdPath, err := restarted.openAttachment(runtime, filepath.Join(dir, "third-backend.sock"))
	if err != nil || thirdPath != path {
		t.Fatalf("worker restart changed public socket: %q, %v", thirdPath, err)
	}
	t.Cleanup(third.close)
}

func TestAttachmentSocketRefusesForeignPaths(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "live socket", "public directory", "symlink directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := attachmentTestDir(t)
			parent := filepath.Join(dir, "runtimes")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "attach.sock")
			switch kind {
			case "file":
				if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(dir, "elsewhere"), path); err != nil {
					t.Fatal(err)
				}
			case "live socket":
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			case "public directory":
				if err := os.Chmod(parent, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink directory":
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir, parent); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.Lstat(path)
			if err := prepareAttachmentSocket(path); err == nil {
				t.Fatal("foreign or unsafe path accepted")
			}
			if beforeErr == nil {
				after, err := os.Lstat(path)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("existing path was altered: %v", err)
				}
			}
		})
	}
}

func TestAttachmentReclaimsStaleSocket(t *testing.T) {
	dir := attachmentTestDir(t)
	path := filepath.Join(dir, "attach.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareAttachmentSocket(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("stale socket still prevents restart: %v", err)
	}
	replacement.Close()
}
