package codexadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This acceptance test uses only fabricated session logs in a private Codex
// home and an offline provider. It never sends a turn or uses real credentials.
func TestNativeDeleteThreadPreservesWorkingDirectory(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_DELETE_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_DELETE_TEST=1 to test the installed Codex deletion RPC")
	}
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	configuration := fmt.Sprintf("model_provider = \"isolated\"\nmodel = \"delete-test\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Offline deletion test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(project, "preserve.txt")
	if err := os.WriteFile(marker, []byte("preserved project data"), 0600); err != nil {
		t.Fatal(err)
	}
	threadID := uuid.NewString()
	now := time.Now().UTC()
	dir := filepath.Join(home, "sessions", now.Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+threadID+".jsonl")
	file, err := os.OpenFile(rollout, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": threadID, "timestamp": now.Format(time.RFC3339Nano), "cwd": project, "originator": "codex_cli_rs", "cli_version": "0.155.1", "source": "cli", "model_provider": "isolated", "base_instructions": map[string]any{"text": "Offline deletion test fixture.", "version": 1}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Fabricated offline deletion fixture."}}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Recorded offline."}}, "phase": "final"}},
	} {
		record["timestamp"] = now.Format(time.RFC3339Nano)
		if err := json.NewEncoder(file).Encode(record); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{WorkingDirectory: project, ClientInfo: ClientInfo{Name: "telegramgw-isolated-delete-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	thread, err := client.ReadThreadState(ctx, threadID)
	if err != nil || thread.CWD != project || thread.ActiveTurnID != "" {
		t.Fatalf("read fabricated thread: %#v, %v", thread, err)
	}
	if err := client.DeleteThread(ctx, threadID); err != nil {
		t.Fatalf("delete fabricated thread: %v", err)
	}
	if _, err := client.ReadThread(ctx, threadID, false); err == nil {
		t.Fatal("deleted thread is still readable")
	}
	if _, err := os.Stat(rollout); !os.IsNotExist(err) {
		t.Fatalf("persisted thread log remains after delete: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserved project data" {
		t.Fatalf("working directory changed: %q, %v", data, err)
	}
	// A just-created Telegram session has no model turns yet. App-server must
	// still be able to delete that session through the same supported RPC.
	empty, err := client.StartThread(ctx, ThreadOptions{CWD: project})
	if err != nil {
		t.Fatalf("start empty offline thread: %v", err)
	}
	state, err := client.ReadThreadState(ctx, empty.ID)
	if err != nil || state.ID != empty.ID || state.CWD != project || state.Status != "idle" || state.ActiveTurnID != "" {
		t.Fatalf("inspect empty thread for inventory and update safety: %#v, %v", state, err)
	}
	if _, err := client.ReadThreadForDeletion(ctx, empty.ID); err != nil {
		t.Fatalf("read empty offline thread before deletion: %v", err)
	}
	if err := client.DeleteThread(ctx, empty.ID); err != nil {
		t.Fatalf("delete empty offline thread: %v", err)
	}
	for {
		select {
		case event := <-client.Events():
			if event.Kind == "thread_deleted" && event.ThreadID == threadID {
				return
			}
		case <-ctx.Done():
			t.Fatal("missing thread/deleted notification")
		}
	}
}
