//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
)

// Native compatibility coverage uses an isolated home and unreachable provider:
// no production credentials, model turns, terminal UI, or existing runtime.
func TestWebUICommandsNativeAppServer(t *testing.T) {
	if os.Getenv("CODEX_WEBUI_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_WEBUI_NATIVE_TEST=1 to check official command compatibility")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	dir := attachmentTestDir(t)
	settings := fmt.Sprintf("model_provider = \"isolated\"\nmodel = \"webui-test\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Isolated command test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	threadID := nativeSavedFixture(t, dir)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	shared, err := codexadapter.StartShared(ctx, codexadapter.Config{
		Command: binary, Args: []string{"app-server"}, WorkingDirectory: dir,
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + dir, "TERM=xterm-256color"}, Stderr: io.Discard,
	}, filepath.Join(dir, "commands.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	workerID := uuid.NewString()
	store, err := OpenStore(filepath.Join(dir, "worker.db"), workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := NewAgent(config.WorkerConfig{WorkerID: workerID, AllowedWorkspaceRoots: []string{dir}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	a.ctx = ctx
	defer func() { cancel(); a.group.Wait() }()
	runtime, err := store.BeginRuntime("native-web", "Native web", dir)
	if err != nil {
		t.Fatal(err)
	}
	runtime.State = "running"
	a.manager.install(runtime, shared.Client)
	thread, err := shared.ResumeThread(ctx, threadID, codexadapter.ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.UpsertSession(sessionFromThread(runtime, thread, true))
	if err != nil {
		t.Fatal(err)
	}
	a.onSession(runtime, session)
	for _, name := range []string{"pwd", "permissions"} {
		result, err := a.executeWebUICommand(ctx, runtime, session, name, "")
		if err != nil || result.Text == "" || (name == "permissions" && result.Permissions == nil) {
			t.Fatalf("native /%s = %#v, %v", name, result, err)
		}
	}
	args, _ := json.Marshal(map[string]string{"name": "Web native session", "cwd": dir})
	created, err := a.executeWebUICommand(ctx, runtime, session, "new", string(args))
	if err != nil || created.Session == nil || created.Session.Name != "Web native session" {
		t.Fatalf("native /new = %#v, %v", created, err)
	}
	if resumed, err := shared.ResumeThread(ctx, created.Session.ThreadID, codexadapter.ThreadOptions{}); err != nil || resumed.ID != created.Session.ThreadID {
		t.Fatalf("new empty session could not be attached before first prompt: %#v, %v", resumed, err)
	}
	for _, command := range []struct{ name, args string }{{"rename", "Renamed through Web UI"}, {"delete", "confirm " + created.Session.ThreadID}} {
		result, err := a.executeWebUICommand(ctx, runtime, *created.Session, command.name, command.args)
		if err != nil || result.State != "completed" {
			t.Fatalf("native /%s = %#v, %v", command.name, result, err)
		}
		if command.name == "delete" && (result.Session == nil || !result.Session.Deleted) {
			t.Fatal("native deletion did not retire session")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal("native session deletion removed workspace")
	}
	if commands, err := store.PendingCommands(); err != nil || len(commands) != 0 {
		t.Fatal("browser-only command entered durable replay queue")
	}
}
