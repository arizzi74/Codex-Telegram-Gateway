package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIFileOpenConfinesWorkspace(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	cwd := filepath.Join(allowed, "project")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, "report name.pdf")
	content := []byte("artifact\x00\xffdata")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path, "./report name.pdf", "~/project/report name.pdf", (&url.URL{Scheme: "file", Path: path, Fragment: "L12"}).String(), path + ":12"} {
		file, info, err := openWebUIFile(cwd, allowed, target, []string{allowed}, nil)
		if err != nil {
			t.Fatalf("open %q: %v", target, err)
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(got, content) || info.Name != "report name.pdf" || info.Path != path || info.Size != int64(len(content)) {
			t.Fatalf("open %q = %#v, %x, %v", target, info, got, err)
		}
	}
	for _, target := range []string{"..", "../report name.pdf", "nested/../report name.pdf", "~/project/../project/report name.pdf", filepath.Join(allowed, "sibling.pdf"), filepath.Join(outside, "file"), cwd, "~"} {
		if file, _, err := openWebUIFile(cwd, allowed, target, []string{allowed}, nil); err == nil {
			file.Close()
			t.Errorf("out-of-scope target accepted: %q", target)
		}
	}
}

func TestWebUIFileOpenRejectsSymlinksCredentialsAndSpecialFiles(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	for _, directory := range []string{"regular", ".ssh", ".codex"} {
		if err := os.Mkdir(filepath.Join(cwd, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"regular/file", ".ssh/id_rsa", ".codex/auth.json", "token", "state.db", ".env", ".ENV.local"} {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"inside-link": "regular/file", "outside-link": outside, "parent-link": "regular", "credential-link": ".ssh/id_rsa"} {
		if err := os.Symlink(target, filepath.Join(cwd, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(cwd, "token"), filepath.Join(cwd, "token-alias")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(cwd, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(cwd, "large"))
	if err != nil {
		t.Fatal(err)
	}
	err = large.Truncate(protocol.MaxWebUIFileBytes + 1)
	large.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"inside-link", "outside-link/file", "parent-link/file", "credential-link", ".ssh/id_rsa", ".codex/auth.json", ".env", ".ENV.local", "token", "token-alias", "state.db", "regular", "large", "fifo"} {
		started := time.Now()
		file, _, err := openWebUIFile(cwd, cwd, target, []string{cwd}, []string{filepath.Join(cwd, "token"), filepath.Join(cwd, "state.db")})
		if err == nil {
			file.Close()
			t.Errorf("unsafe file accepted: %q", target)
		}
		if time.Since(started) > time.Second {
			t.Fatalf("opening %q blocked", target)
		}
	}
}

func TestWebUIFileOpenRejectsConcurrentSymlinkReplacement(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(cwd, "parent")
	backup := filepath.Join(cwd, "backup")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "file"), []byte("allowed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(parent, backup) != nil {
				continue
			}
			_ = os.Symlink(outside, parent)
			_ = os.Remove(parent)
			_ = os.Rename(backup, parent)
		}
	}()
	defer func() { close(stop); <-done }()
	for range 200 {
		file, _, err := openWebUIFile(cwd, cwd, "parent/file", []string{cwd}, nil)
		if err != nil {
			continue
		}
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil || string(got) != "allowed" {
			t.Fatalf("concurrent parent replacement escaped workspace: %q, %v", got, err)
		}
	}
}

func TestWebUIFileOpenRejectsReplacedWorkspaceRoot(t *testing.T) {
	allowed := t.TempDir()
	cwd := filepath.Join(allowed, "project")
	credentialDirectory := filepath.Join(allowed, ".codex")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(credentialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDirectory, "auth.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// cwd is the saved canonical path from before the symlink swap. The
	// allowed root is still valid, and the symlink stays within that root.
	if err := os.Rename(cwd, cwd+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".codex", cwd); err != nil {
		t.Fatal(err)
	}
	if file, _, err := openWebUIFile(cwd, allowed, "auth.json", []string{allowed}, nil); err == nil {
		file.Close()
		t.Fatal("workspace root symlink exposed credentials before the next session fence")
	}
}

func TestWebUIFileOpenRejectsHardLinkAliases(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "project")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{filepath.Join(home, ".codex", "auth.json"), filepath.Join(cwd, "ordinary-artifact")} {
		if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(cwd, "innocent-artifact")
		if err := os.Link(source, alias); err != nil {
			t.Fatal(err)
		}
		file, _, err := openWebUIFile(cwd, home, alias, []string{home}, nil)
		if err == nil {
			file.Close()
			t.Fatalf("hardlink alias accepted: %s", source)
		}
		if !strings.Contains(err.Error(), "multiple hard links") {
			t.Fatalf("hardlink alias error: %v", err)
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
	}
}

type webUIFileFixture struct {
	pool    *webUIRelays
	frames  chan protocol.WebUIFrame
	runtime protocol.Runtime
	session protocol.Session
	mu      sync.Mutex
}

func newWebUIFileFixture(t *testing.T) *webUIFileFixture {
	t.Helper()
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	cfg.AllowedWorkspaceRoots = []string{t.TempDir()}
	fixture := &webUIFileFixture{frames: make(chan protocol.WebUIFrame, 32), runtime: protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1, State: "running"}}
	var err error
	fixture.session, err = store.UpsertSession(protocol.Session{RuntimeID: fixture.runtime.ID, ThreadID: "artifact-thread", CWD: cfg.AllowedWorkspaceRoots[0]})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writes := make(chan outbound, 32)
	c := &Connection{cfg: cfg, store: store, snapshot: func() []protocol.Runtime {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return []protocol.Runtime{fixture.runtime}
	}, onCommand: func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		t.Error("file download dispatched a durable command")
		return protocol.CommandAck{}, nil
	}, observeWebUI: func(context.Context, protocol.Runtime, protocol.Session) error {
		t.Error("file download started an app-server observer")
		return nil
	}}
	fixture.pool = newWebUIRelays(c, ctx, writes)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case output := <-writes:
				frame, err := protocol.Payload[protocol.WebUIFrame](output.envelope)
				output.done <- err
				if err == nil {
					select {
					case fixture.frames <- frame:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); fixture.pool.close(); <-done; store.Close() })
	return fixture
}

func (fixture *webUIFileFixture) open(t *testing.T, path string) string {
	t.Helper()
	id := uuid.NewString()
	data, _ := json.Marshal(protocol.WebUIFileRequest{Path: path})
	fixture.mu.Lock()
	runtime := fixture.runtime
	fixture.mu.Unlock()
	fixture.pool.receive(protocol.WebUIFrame{ID: id, Action: "file_open", RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: fixture.session.ID, Data: data})
	return id
}

func (fixture *webUIFileFixture) credit(id string, offset int64) {
	data, _ := json.Marshal(protocol.WebUIFileRead{Offset: offset})
	fixture.pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: data})
}

func TestWebUIFileTransferExactBytesAndHeldHandle(t *testing.T) {
	for _, size := range []int{0, 13, protocol.WebUIFileChunkBytes, protocol.WebUIFileChunkBytes*2 + 31} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			fixture := newWebUIFileFixture(t)
			content := make([]byte, size)
			for index := range content {
				content[index] = byte(index % 256)
			}
			path := filepath.Join(fixture.session.CWD, "artifact.bin")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			redactor, err := newWorkerRedactor([]string{"AAAA", "artifact"})
			if err != nil || fixture.pool.c.store.configureRedactor(redactor) != nil {
				t.Fatal("configure redactor", err)
			}
			id := fixture.open(t, "artifact.bin")
			ready := nextWebUIFrame(t, fixture.frames)
			var info protocol.WebUIFileInfo
			if ready.ID != id || ready.Action != "ready" || json.Unmarshal(ready.Data, &info) != nil || info.Name != "artifact.bin" || info.Path != path || info.Size != int64(size) {
				t.Fatalf("metadata = %#v (%s)", ready, ready.Data)
			}
			// Replacing the path must not redirect an already admitted transfer.
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			var got []byte
			for {
				fixture.credit(id, int64(len(got)))
				frame := nextWebUIFrame(t, fixture.frames)
				var chunk protocol.WebUIFileChunk
				if frame.ID != id || frame.Action != "output" || json.Unmarshal(frame.Data, &chunk) != nil || chunk.Offset != int64(len(got)) || len(chunk.Data) > protocol.WebUIFileChunkBytes {
					t.Fatalf("chunk = %#v (%s)", frame, frame.Data)
				}
				got = append(got, chunk.Data...)
				if chunk.EOF {
					break
				}
			}
			if !bytes.Equal(got, content) {
				t.Fatal("file bytes changed across relay or were redacted")
			}
			if events, err := fixture.pool.c.store.OutboxAfter(0); err != nil || len(events) != 0 {
				t.Fatalf("ephemeral transfer persisted events: %#v, %v", events, err)
			}
		})
	}
}

func TestWebUIFileTransferRejectsOffsetsAndMutation(t *testing.T) {
	for _, change := range []string{"wrong-offset", "oversized-credit", "truncate", "grow", "hardlink-after-ready", "archive", "delete", "retarget-cwd", "runtime-generation"} {
		t.Run(change, func(t *testing.T) {
			fixture := newWebUIFileFixture(t)
			path := filepath.Join(fixture.session.CWD, "artifact")
			if err := os.WriteFile(path, []byte("artifact"), 0o600); err != nil {
				t.Fatal(err)
			}
			id := fixture.open(t, path)
			if frame := nextWebUIFrame(t, fixture.frames); frame.Action != "ready" {
				t.Fatalf("not ready: %#v", frame)
			}
			offset := int64(0)
			switch change {
			case "wrong-offset":
				offset = 1
			case "oversized-credit":
				fixture.pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: json.RawMessage(`{"offset":0,"padding":"` + strings.Repeat("x", 256) + `"}`)})
			case "truncate":
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			case "grow":
				if err := os.Truncate(path, 100); err != nil {
					t.Fatal(err)
				}
			case "hardlink-after-ready":
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatal(err)
				}
			case "archive", "delete", "retarget-cwd":
				session := fixture.session
				if change == "archive" {
					session.Archived = true
				} else if change == "delete" {
					session.Deleted = true
				} else {
					session.CWD = filepath.Join(session.CWD, "different")
					if err := os.Mkdir(session.CWD, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := fixture.pool.c.store.UpsertSession(session); err != nil {
					t.Fatal(err)
				}
			case "runtime-generation":
				fixture.mu.Lock()
				fixture.runtime.Generation++
				fixture.mu.Unlock()
			}
			if change != "oversized-credit" {
				fixture.credit(id, offset)
			}
			if frame := nextWebUIFrame(t, fixture.frames); frame.Action != "error" || frame.Error == "" || len(frame.Data) != 0 {
				t.Fatalf("unsafe transfer continued: %#v", frame)
			}
		})
	}
}

func TestWebUIFileTransferLimitsAndCancellation(t *testing.T) {
	fixture := newWebUIFileFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.session.CWD, "artifact"), []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range maxWebUIFileTransfers {
		ids = append(ids, fixture.open(t, "artifact"))
		if frame := nextWebUIFrame(t, fixture.frames); frame.Action != "ready" {
			t.Fatalf("admitted transfer failed: %#v", frame)
		}
	}
	rejected := fixture.open(t, "artifact")
	if frame := nextWebUIFrame(t, fixture.frames); frame.Action != "error" || frame.ID != rejected {
		t.Fatalf("unbounded transfers admitted: %#v", frame)
	}
	for _, id := range ids {
		fixture.pool.receive(protocol.WebUIFrame{ID: id, Action: "close"})
	}
	done := make(chan struct{})
	go func() { fixture.pool.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled transfer retained its file handle")
	}
	fixture.pool.mu.Lock()
	defer fixture.pool.mu.Unlock()
	if len(fixture.pool.files) != 0 || len(fixture.pool.clients) != 0 {
		t.Fatal("file cancellation leaked transfer or app-server relay")
	}
}
