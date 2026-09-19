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

// The optional native test has a private Codex home, offline provider, and no
// credentials. It never starts a model turn or touches an existing session.
func TestNativePermissionPresetsApplyToIsolatedThread(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_PERMISSIONS_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_PERMISSIONS_TEST=1 to verify native permission presets")
	}
	codexHome, project := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	configuration := fmt.Sprintf("model_provider = \"isolated\"\nmodel = \"permissions-test\"\ncheck_for_update_on_startup = false\n[features]\nguardian_approval = true\n[model_providers.isolated]\nname = \"Offline permissions test\"\nbase_url = \"http://127.0.0.1:1\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = \"trusted\"\n", project)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configuration), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := Start(ctx, Config{WorkingDirectory: project, ClientInfo: ClientInfo{Name: "telegramgw-isolated-permissions-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	profiles, err := client.ListPermissionProfiles(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{":workspace", ":read-only", ":danger-full-access"} {
		found := false
		for _, profile := range profiles {
			if profile.ID == id && profile.Allowed {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing allowed native profile %s: %#v", id, profiles)
		}
	}
	if _, err := client.ReadPermissionRequirements(ctx); err != nil {
		t.Fatal(err)
	}
	thread, err := client.StartThread(ctx, ThreadOptions{CWD: project, Sandbox: "workspace-write", ApprovalPolicy: "on-request"})
	if err != nil {
		t.Fatal(err)
	}
	if enabled, err := client.FeatureEnabled(ctx, thread.ID, "guardian_approval"); err != nil || !enabled {
		t.Fatalf("guardian feature = %v, %v", enabled, err)
	}
	for _, preset := range []struct{ profile, policy, reviewer, sandbox string }{
		{":read-only", "on-request", "user", "readOnly"},
		{":workspace", "on-request", "user", "workspaceWrite"},
		{":workspace", "on-request", "auto_review", "workspaceWrite"},
		{":danger-full-access", "never", "user", "dangerFullAccess"},
	} {
		if err := client.UpdateThreadSettings(ctx, thread.ID, ThreadSettingsUpdate{PermissionProfile: &preset.profile, ApprovalPolicy: &preset.policy, ApprovalsReviewer: &preset.reviewer}); err != nil {
			t.Fatalf("apply %s/%s: %v", preset.profile, preset.reviewer, err)
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
						ApprovalPolicy    string `json:"approvalPolicy"`
						ApprovalsReviewer string `json:"approvalsReviewer"`
						ActiveProfile     struct {
							ID string `json:"id"`
						} `json:"activePermissionProfile"`
						SandboxPolicy struct {
							Type string `json:"type"`
						} `json:"sandboxPolicy"`
					} `json:"threadSettings"`
				}
				if err := json.Unmarshal(event.Params, &notification); err != nil {
					t.Fatal(err)
				}
				if notification.ThreadID != thread.ID || notification.Settings.ApprovalPolicy != preset.policy || notification.Settings.ApprovalsReviewer != preset.reviewer || notification.Settings.ActiveProfile.ID != preset.profile || notification.Settings.SandboxPolicy.Type != preset.sandbox {
					t.Fatalf("native applied settings differ: %s", event.Params)
				}
			case <-ctx.Done():
				t.Fatalf("missing native settings notification: %v", ctx.Err())
			}
			break
		}
	}
}
