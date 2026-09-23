package codexadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Verify settings synchronization between independent native clients using a
// temporary home, an offline provider, and no model turns or user threads.
func TestNativeSettingsBroadcastAcrossClients(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_SETTINGS_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_SETTINGS_TEST=1 to verify official cross-client settings broadcasts")
	}
	home, project := t.TempDir(), t.TempDir()
	configuration := fmt.Sprintf("model_provider = \"isolated\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Offline settings test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", project)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	threadID, now := uuid.NewString(), time.Now().UTC()
	rolloutDir := filepath.Join(home, "sessions", now.Format("2006/01/02"))
	if err := os.MkdirAll(rolloutDir, 0700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+threadID+".jsonl")
	file, err := os.OpenFile(rollout, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": threadID, "timestamp": now.Format(time.RFC3339Nano), "cwd": project, "originator": "codex_cli_rs", "cli_version": "0.156.0", "source": "cli", "model_provider": "isolated", "base_instructions": map[string]any{"text": "Offline settings fixture.", "version": 1}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Fabricated offline settings fixture."}}}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Recorded offline."}}, "phase": "final"}},
	} {
		record["timestamp"] = now.Format(time.RFC3339Nano)
		if err := json.NewEncoder(file).Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	fixture := newSocketFixture(t)
	shared, err := StartShared(ctx, Config{WorkingDirectory: project, ClientInfo: ClientInfo{Name: "settings_observer"}, Stderr: io.Discard,
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + home, "TERM=xterm-256color"}}, fixture.alias)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", shared.LocalSocket())
	}}
	defer transport.CloseIdleConnections()
	ws, _, err := websocket.Dial(ctx, "ws://codex.local/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(maxJSONRPCMessageBytes)
	bridge := newJSONLWSBridge(ws)
	other := New(bridge.transport(bridge.Close), Config{ClientInfo: ClientInfo{Name: "settings_other_client"}})
	defer other.Close()
	if err := other.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	models, err := shared.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var choices []ModelInfo
	for _, model := range models {
		if len(model.ReasoningEfforts) > 0 {
			choices = append(choices, model)
		}
		if len(choices) == 2 {
			break
		}
	}
	if len(choices) != 2 {
		t.Fatal("native catalog does not expose two models with reasoning choices")
	}
	thread, err := shared.ResumeThread(ctx, threadID, ThreadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ResumeThread(ctx, thread.ID, ThreadOptions{}); err != nil {
		t.Fatal(err)
	}
	for index, pair := range [][2]*Client{{other, shared.Client}, {shared.Client, other}} {
		choice := choices[index]
		model, effort := choice.Model, choice.ReasoningEfforts[len(choice.ReasoningEfforts)-1]
		if model == "" {
			model = choice.ID
		}
		if err := pair[0].UpdateThreadSettings(ctx, thread.ID, ThreadSettingsUpdate{Model: &model, Effort: &effort}); err != nil {
			t.Fatal(err)
		}
		matched := false
		for !matched {
			select {
			case event := <-pair[1].Events():
				matched = event.Kind == "thread_settings_updated" && event.ThreadID == thread.ID && event.Settings != nil && event.Settings.Model == model && event.Settings.ReasoningEffort == effort
			case <-ctx.Done():
				t.Fatalf("native client %d did not receive the other client's current settings: %v", index, ctx.Err())
			}
		}
	}
}
