package codexadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Verify the advertised choices against the official runtime without starting
// a model turn, using credentials, or changing any existing session.
func TestNativeModelAndEffortApplyTogether(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_MODEL_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_MODEL_TEST=1 to verify native model selection")
	}
	codexHome, project := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	configuration := fmt.Sprintf("model_provider = \"isolated\"\ncheck_for_update_on_startup = false\n[model_providers.isolated]\nname = \"Offline model test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", project)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{WorkingDirectory: project, ClientInfo: ClientInfo{Name: "telegramgw-isolated-model-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	models, err := client.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var choices []ModelInfo
	for _, model := range models {
		if len(model.ReasoningEfforts) != 0 {
			choices = append(choices, model)
		}
		if len(choices) == 2 {
			break
		}
	}
	if len(choices) < 2 {
		t.Fatalf("need two advertised models with reasoning choices; got %d", len(choices))
	}
	thread, err := client.StartThread(ctx, ThreadOptions{CWD: project, Sandbox: "read-only", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range choices {
		model, effort := choice.Model, choice.ReasoningEfforts[len(choice.ReasoningEfforts)-1]
		if model == "" {
			model = choice.ID
		}
		if err := client.UpdateThreadSettings(ctx, thread.ID, ThreadSettingsUpdate{Model: &model, Effort: &effort}); err != nil {
			t.Fatalf("apply %s/%s: %v", model, effort, err)
		}
		for {
			select {
			case event := <-client.Events():
				if event.Method != "thread/settings/updated" {
					continue
				}
				var notification struct {
					ThreadID string `json:"threadId"`
					Settings struct {
						Model  string `json:"model"`
						Effort string `json:"effort"`
					} `json:"threadSettings"`
				}
				if err := json.Unmarshal(event.Params, &notification); err != nil {
					t.Fatal(err)
				}
				if notification.ThreadID != thread.ID || notification.Settings.Model != model || notification.Settings.Effort != effort {
					t.Fatalf("native settings do not match selected model and effort: %s", event.Params)
				}
			case <-ctx.Done():
				t.Fatalf("missing native settings notification: %v", ctx.Err())
			}
			break
		}
	}
}
