package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func installModelMenuCatalog(t *testing.T, server *codextest.Server) {
	t.Helper()
	if err := server.SetMethodResult("model/list", map[string]any{"data": []map[string]any{
		{"id": "model-a", "model": "native-a", "displayName": "Model A", "isDefault": true, "defaultReasoningEffort": "high", "supportedReasoningEfforts": []map[string]string{{"reasoningEffort": "low"}, {"reasoningEffort": "high"}}},
		{"id": "model-b", "displayName": "Model B"},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestModelMenuSelectsReasoningBeforeApplyingBothSettings(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	installModelMenuCatalog(t, server)
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "model-picker", CWD: runtime.DefaultCWD, State: "not_loaded"}}
	session := actor.session
	client, _, _ := a.manager.Client(runtime.ID)
	for _, args := range []string{"", "--menu model-a", "--menu", "--cancel"} {
		result, err := actor.executeCodexCommand(t.Context(), client, "model", args)
		if err != nil {
			t.Fatalf("browse %q: %v", args, err)
		}
		switch args {
		case "":
			if !modelOptionPresent(result, "--menu model-a", "Model A (default)") || !modelOptionPresent(result, "--cancel", "Cancel") {
				t.Fatalf("model choices: %#v", result)
			}
		case "--menu model-a":
			if !modelOptionPresent(result, "model-a high", "High (default)") || !modelOptionPresent(result, "model-a low", "Low") || !modelOptionPresent(result, "--menu", "Back to models") {
				t.Fatalf("reasoning choices: %#v", result)
			}
		case "--cancel":
			if result.ModelMenu != nil || !strings.Contains(result.Text, "cancelled") {
				t.Fatalf("cancel: %#v", result)
			}
		}
	}
	if hasCall(server.Calls(), "thread/resume") || hasCall(server.Calls(), "thread/settings/update") || hasCall(server.Calls(), "turn/start") {
		t.Fatal("browsing or cancellation changed the session")
	}
	result, err := actor.executeCodexCommand(t.Context(), client, "model", "model-a high")
	if err != nil || result.ModelMenu != nil || !strings.Contains(result.Text, "native-a with high reasoning") {
		t.Fatalf("apply: %#v %v", result, err)
	}
	if countCall(server.Calls(), "thread/settings/update") != 1 || countCall(server.Calls(), "thread/resume") != 1 || hasCall(server.Calls(), "turn/start") {
		t.Fatalf("incorrect apply lifecycle: %#v", server.Calls())
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/settings/update" {
			var params map[string]string
			if err := json.Unmarshal(call.Params, &params); err != nil || params["model"] != "native-a" || params["effort"] != "high" || params["threadId"] != session.ThreadID {
				t.Fatalf("model and effort not applied together: %s %v", call.Params, err)
			}
		}
	}
}

func TestModelMenuRevalidatesCatalogAndProtectsActiveTurns(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	installModelMenuCatalog(t, server)
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "model-active", CWD: runtime.DefaultCWD, State: "running", ActiveTurnID: "active-turn", Loaded: true}}
	client, _, _ := a.manager.Client(runtime.ID)
	for _, args := range []string{"", "--menu model-a", "--menu model-b", "--menu", "--cancel", "--page 0"} {
		if codexCommandNeedsIdle("model", args) {
			t.Fatalf("browse %q needs idle", args)
		}
		if _, err := actor.executeCodexCommand(context.Background(), client, "model", args); err != nil {
			t.Fatalf("browse %q: %v", args, err)
		}
	}
	for _, args := range []string{"model-a", "model-a high", "model-a unsupported", "--menu missing", "--page invalid"} {
		if !strings.HasPrefix(args, "--menu") && !codexCommandNeedsIdle("model", args) {
			t.Fatalf("apply %q allowed active turn", args)
		}
		if _, err := actor.executeCodexCommand(t.Context(), client, "model", args); err == nil {
			t.Fatalf("invalid/active choice accepted: %q", args)
		}
	}
	actor.session.ActiveTurnID, actor.session.State = "", "idle"
	result, err := actor.executeCodexCommand(t.Context(), client, "model", "--menu model-b")
	if err != nil || !modelOptionPresent(result, "model-b", "Apply model") {
		t.Fatalf("model without efforts: %#v %v", result, err)
	}
	if err := server.SetMethodResult("model/list", map[string]any{"data": []map[string]any{{"id": "model-b"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.executeCodexCommand(t.Context(), client, "model", "model-a high"); err == nil {
		t.Fatal("removed model was applied")
	}
	if hasCall(server.Calls(), "thread/settings/update") || hasCall(server.Calls(), "thread/resume") {
		t.Fatal("active or invalid selection changed settings")
	}
	if result, err := actor.executeCodexCommand(t.Context(), client, "model", "model-b"); err != nil || result.ModelMenu != nil {
		t.Fatalf("text one-argument form no longer applies: %#v %v", result, err)
	}
}

func TestModelMenuPaginatesEntireAdvertisedCatalog(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	var models []map[string]any
	for i := range 23 {
		models = append(models, map[string]any{"id": fmt.Sprintf("model-%d", i), "displayName": strings.Repeat("界", 200)})
	}
	if err := server.SetMethodResult("model/list", map[string]any{"data": models}); err != nil {
		t.Fatal(err)
	}
	actor := &sessionActor{agent: a, runtime: runtime, session: protocol.Session{RuntimeID: runtime.ID, ThreadID: "many-models", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}}
	client, _, _ := a.manager.Client(runtime.ID)
	seen := map[string]bool{}
	for page := range 3 {
		result, err := actor.executeCodexCommand(t.Context(), client, "model", fmt.Sprintf("--page %d", page))
		if err != nil || result.ModelMenu.Validate() != nil {
			t.Fatalf("page %d: %#v %v", page, result, err)
		}
		for _, option := range result.ModelMenu.Options {
			if strings.HasPrefix(option.Args, "--menu ") {
				if seen[option.Args] {
					t.Fatalf("duplicate model across pages: %s", option.Args)
				}
				seen[option.Args] = true
			}
		}
	}
	if len(seen) != len(models) {
		t.Fatalf("catalog omitted models: %d of %d", len(seen), len(models))
	}
}

func modelOptionPresent(result protocol.Result, args, label string) bool {
	if result.ModelMenu != nil {
		for _, option := range result.ModelMenu.Options {
			if option.Args == args && option.Label == label {
				return true
			}
		}
	}
	return false
}
