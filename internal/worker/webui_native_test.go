//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// This optional compatibility test starts only an isolated official app-server.
// It never launches a TUI, sends a model turn, or accesses a real Codex home.
// CODEX_WEBUI_NATIVE_TEST=1 go test ./internal/worker -run '^TestWebUINativeAppServer$' -count=1
func TestWebUINativeAppServer(t *testing.T) {
	if os.Getenv("CODEX_WEBUI_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_WEBUI_NATIVE_TEST=1 to test the installed official app-server")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	dir := attachmentTestDir(t)
	config := fmt.Sprintf("model_provider = \"isolated\"\nmodel = \"webui-test\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Isolated web UI test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	thread := nativeSavedFixture(t, dir)
	socket := filepath.Join(dir, "backend.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shared, err := codexadapter.StartShared(ctx, codexadapter.Config{Command: binary, Args: []string{"app-server"}, WorkingDirectory: dir,
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + dir, "TERM=xterm-256color"}, Stderr: io.Discard}, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	guarded := filepath.Join(dir, "guard.sock")
	proxy, err := newAttachmentProxy(guarded, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.close()
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	cfg.AllowedWorkspaceRoots = []string{dir}
	runtime := protocol.Runtime{ID: uuid.NewString(), WorkerID: workerID, Generation: 1, State: "running", LocalSocket: guarded, DefaultCWD: dir}
	session, err := store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: thread, CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	c := &Connection{cfg: cfg, store: store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}
	a, err := NewAgent(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.ctx = ctx
	defer func() { cancel(); a.group.Wait() }()
	a.manager.install(runtime, shared.Client)
	a.onSession(runtime, session)
	c.observeWebUI = a.observeWebUISession
	c.historyWebUI = a.webUIHistory
	writes := make(chan outbound, 32)
	frames := make(chan protocol.WebUIFrame, 128)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case output := <-writes:
				frame, err := protocol.Payload[protocol.WebUIFrame](output.envelope)
				output.done <- err
				if err == nil {
					select {
					case frames <- frame:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	pool := newWebUIRelays(c, ctx, writes)
	defer pool.close()
	for attempt := 0; attempt < 2; attempt++ {
		id := uuid.NewString()
		pool.receive(protocol.WebUIFrame{ID: id, Action: "open", RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: session.ID})
		if frame := nextWebUIFrame(t, frames); frame.Action != "ready" {
			t.Fatalf("open failed: %#v", frame)
		}
		for i, method := range []string{"thread/resume", "account/rateLimits/read", "thread/turns/list", "thread/items/list"} {
			payload, _ := json.Marshal(map[string]any{"id": i + 1, "method": method, "params": map[string]any{}})
			pool.receive(protocol.WebUIFrame{ID: id, Action: "input", Data: payload})
			for {
				frame := nextWebUIFrame(t, frames)
				if frame.Action == "error" {
					t.Fatalf("%s relay failed: %s", method, frame.Error)
				}
				var reply struct {
					ID     int             `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if json.Unmarshal(frame.Data, &reply) != nil || reply.ID != i+1 {
					continue
				}
				if method == "account/rateLimits/read" {
					// The isolated provider has no real account. A native account
					// authentication error proves the method/options were accepted;
					// it must not leak account error details or close the relay.
					var failure struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					}
					if json.Unmarshal(reply.Error, &failure) != nil || failure.Code == 0 || failure.Code == -32601 || failure.Code == -32602 || failure.Message != "Codex usage limits are unavailable." {
						t.Fatalf("official quota read did not reach account authentication: %s", frame.Data)
					}
					break
				}
				if len(reply.Error) > 0 {
					t.Fatalf("official %s rejected web UI request: %s", method, reply.Error)
				}
				if len(reply.Result) == 0 {
					t.Fatalf("official %s omitted result", method)
				}
				if method == "thread/resume" && !strings.Contains(string(reply.Result), thread) {
					t.Fatalf("resume returned another thread: %s", reply.Result)
				}
				if method == "thread/items/list" {
					if _, err := webUIItemPage(reply.Result); err != nil {
						t.Fatalf("official item pagination shape is incompatible: %s, %v", reply.Result, err)
					}
				}
				break
			}
		}
		pool.receive(protocol.WebUIFrame{ID: id, Action: "close"})
		pool.wg.Wait()
	}
	// A guaranteed absent thread exercises the official input decoder without
	// starting model work. The native error must reach thread lookup, not reject
	// the image input schema or inline data URL.
	imageData, _ := webUITestImage(t)
	_, imageErr := shared.StartTurnWithImages(ctx, "00000000-0000-7000-8000-000000000001", "", []protocol.Image{{MIMEType: "image/png", Data: imageData}})
	var imageRPC *codexadapter.RPCError
	if !errors.As(imageErr, &imageRPC) || imageRPC.Code == -32602 || !strings.Contains(strings.ToLower(imageRPC.Message), "thread") {
		t.Fatalf("official runtime rejected inline image schema before thread lookup: %v", imageErr)
	}
	if err := syscall.Kill(shared.PID(), 0); err != nil {
		t.Fatal("browser detach stopped the app-server")
	}
	waitFor(t, func() bool { return proxy.checkIdle() == nil })
	t.Log("Official app-server resume, restricted quota read, bounded history, detach and reconnect succeeded without any CLI process or model turn.")
}
