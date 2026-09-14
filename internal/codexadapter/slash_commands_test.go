package codexadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInitializeOptsIntoTypedExperimentalMethods(t *testing.T) {
	client, fake := newFake(t)
	done := make(chan error, 1)
	go func() { done <- client.Initialize(context.Background()) }()
	request := fake.next(t)
	if got := method(t, request); got != "initialize" {
		t.Fatalf("method = %q", got)
	}
	var capabilities struct {
		ExperimentalAPI bool `json:"experimentalApi"`
	}
	if err := json.Unmarshal(params(t, request)["capabilities"], &capabilities); err != nil || !capabilities.ExperimentalAPI {
		t.Fatalf("capabilities = %s (%v)", params(t, request)["capabilities"], err)
	}
	fake.respond(t, request, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-cli/0.154.0"})
	if got := method(t, fake.next(t)); got != "initialized" {
		t.Fatalf("notification = %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSlashSettingsUseTypedRPCParams(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)

	done := make(chan error, 1)
	model, effort := "gpt-5.4", "high"
	go func() {
		done <- client.UpdateThreadSettings(context.Background(), "thread-1", ThreadSettingsUpdate{Model: &model, Effort: &effort, SandboxMode: "workspace-write"})
	}()
	request := fake.next(t)
	if got := method(t, request); got != "thread/settings/update" {
		t.Fatalf("method = %q", got)
	}
	p := params(t, request)
	if string(p["threadId"]) != `"thread-1"` || string(p["model"]) != `"gpt-5.4"` || string(p["effort"]) != `"high"` {
		t.Fatalf("settings params = %s", request["params"])
	}
	var sandbox map[string]any
	if err := json.Unmarshal(p["sandboxPolicy"], &sandbox); err != nil || sandbox["type"] != "workspaceWrite" || sandbox["networkAccess"] != false {
		t.Fatalf("sandbox policy = %s (%v)", p["sandboxPolicy"], err)
	}
	fake.respond(t, request, map[string]any{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSlashStatusReadsRealConfigAndUsage(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)

	type result struct {
		config EffectiveConfig
		usage  TokenUsage
		err    error
	}
	done := make(chan result, 1)
	go func() {
		cfg, err := client.ReadEffectiveConfig(context.Background(), "/work")
		if err != nil {
			done <- result{err: err}
			return
		}
		usage, err := client.ReadTokenUsage(context.Background(), "thread-1")
		done <- result{config: cfg, usage: usage, err: err}
	}()

	configRequest := fake.next(t)
	if got := method(t, configRequest); got != "config/read" {
		t.Fatalf("config method = %q", got)
	}
	fake.respond(t, configRequest, map[string]any{"config": map[string]any{
		"model": "gpt-5.4", "model_reasoning_effort": "high", "approval_policy": "on-request",
		"sandbox_mode": "workspace-write", "model_context_window": 200000,
	}, "origins": map[string]any{}})
	usageRequest := fake.next(t)
	if got := method(t, usageRequest); got != "account/usage/read" {
		t.Fatalf("usage method = %q", got)
	}
	if got := string(params(t, usageRequest)["threadId"]); got != `"thread-1"` {
		t.Fatalf("usage threadId = %s", got)
	}
	fake.respond(t, usageRequest, map[string]any{"summary": map[string]any{"lifetimeTokens": 12345}, "threadUsage": map[string]any{"threadId": "thread-1", "estimatedUsageCreditsMicros": 2200}})

	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.config.Model != "gpt-5.4" || got.config.ReasoningEffort != "high" || got.config.ApprovalPolicy != "on-request" || got.config.ModelContextWindow != 200000 {
		t.Fatalf("config = %#v", got.config)
	}
	if got.usage.LifetimeTokens == nil || *got.usage.LifetimeTokens != 12345 || got.usage.ThreadCreditsMicros == nil || *got.usage.ThreadCreditsMicros != 2200 {
		t.Fatalf("usage = %#v", got.usage)
	}
}

func TestForkUsesColdThreadDirectly(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)

	type result struct {
		thread Thread
		err    error
	}
	done := make(chan result, 1)
	go func() {
		thread, err := client.ForkThread(context.Background(), "cold-thread")
		done <- result{thread, err}
	}()
	request := fake.next(t)
	if got := method(t, request); got != "thread/fork" {
		t.Fatalf("method = %q; cold fork must not resume", got)
	}
	p := params(t, request)
	if string(p["threadId"]) != `"cold-thread"` || string(p["excludeTurns"]) != "true" {
		t.Fatalf("fork params = %s", request["params"])
	}
	fake.respond(t, request, map[string]any{"thread": map[string]any{"id": "forked-thread", "sessionId": "forked-thread", "cwd": "/work", "status": "idle"}})
	got := <-done
	if got.err != nil || got.thread.ID != "forked-thread" {
		t.Fatalf("fork = %#v, %v", got.thread, got.err)
	}
}

func TestReviewNeverForwardsSlashTextAsPrompt(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() {
		_, err := client.StartReview(context.Background(), "thread-1", "focus on races")
		done <- err
	}()
	request := fake.next(t)
	if got := method(t, request); got != "review/start" {
		t.Fatalf("method = %q, want review/start (never turn/start)", got)
	}
	fake.respond(t, request, map[string]any{"reviewThreadId": "thread-1", "turn": map[string]any{"id": "review-turn", "status": "inProgress", "items": []any{}}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestModelListDecodesSchemaReasoningField(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type result struct {
		models []ModelInfo
		err    error
	}
	done := make(chan result, 1)
	go func() {
		models, err := client.ListModels(context.Background())
		done <- result{models, err}
	}()
	request := fake.next(t)
	if got := method(t, request); got != "model/list" {
		t.Fatalf("method = %q", got)
	}
	fake.respond(t, request, map[string]any{"data": []any{map[string]any{
		"id": "gpt-5.4", "displayName": "GPT-5.4", "description": "test", "defaultReasoningEffort": "medium",
		"isDefault": true, "supportsPersonality": true,
		"supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high", "description": "deeper"}},
		"serviceTiers":              []any{map[string]any{"id": "fast", "name": "Fast", "description": "faster"}},
	}}})
	got := <-done
	if got.err != nil || len(got.models) != 1 {
		t.Fatalf("models = %#v, %v", got.models, got.err)
	}
	if model := got.models[0]; model.ID != "gpt-5.4" || len(model.ReasoningEfforts) != 1 || model.ReasoningEfforts[0] != "high" || len(model.ServiceTiers) != 1 || model.ServiceTiers[0].ID != "fast" {
		t.Fatalf("model = %#v", model)
	}
}

func TestLargeCatalogResponseDoesNotCloseAdapter(t *testing.T) {
	client, fake := newFake(t)
	client.config.RequestTimeout = 10 * time.Second
	initialize(t, client, fake)
	type result struct {
		plugins []PluginInfo
		err     error
	}
	done := make(chan result, 1)
	go func() {
		plugins, _, err := client.ListPlugins(context.Background(), "/work")
		done <- result{plugins, err}
	}()
	request := fake.next(t)
	if got := method(t, request); got != "plugin/list" {
		t.Fatalf("method = %q", got)
	}
	// app-server 0.154 can return a catalog frame larger than 8 MiB. The
	// adapter must consume it without killing unrelated session RPCs.
	fake.respond(t, request, map[string]any{"marketplaces": []any{}, "catalogPadding": strings.Repeat("x", 9<<20)})
	got := <-done
	if got.err != nil || len(got.plugins) != 0 {
		t.Fatalf("plugins = %#v, %v", got.plugins, got.err)
	}

	next := make(chan error, 1)
	go func() { _, err := client.ReadEffectiveConfig(context.Background(), "/work"); next <- err }()
	request = fake.next(t)
	if got := method(t, request); got != "config/read" {
		t.Fatalf("method after large response = %q", got)
	}
	fake.respond(t, request, map[string]any{"config": map[string]any{}, "origins": map[string]any{}})
	if err := <-next; err != nil {
		t.Fatalf("adapter did not survive large response: %v", err)
	}
}

func TestGlobalCatalogReadOmitsColdThreadID(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() { _, err := client.ListApps(context.Background(), ""); done <- err }()
	request := fake.next(t)
	if got := method(t, request); got != "app/installed" {
		t.Fatalf("method = %q", got)
	}
	if _, present := params(t, request)["threadId"]; present {
		t.Fatalf("cold global app/installed included threadId: %s", request["params"])
	}
	fake.respond(t, request, map[string]any{"apps": []any{}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
