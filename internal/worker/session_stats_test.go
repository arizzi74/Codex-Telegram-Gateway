package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func writeStatsRollout(t *testing.T, records ...string) (string, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "sessions", "metrics.jsonl")
	data := `{"type":"session_meta","payload":{"id":"thread-a","timestamp":"2026-01-01T10:00:00Z"}}` + "\n" + strings.Join(records, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func assertStatsCount(t *testing.T, name string, value *int64, want int64) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("%s = %v, want %d", name, value, want)
	}
}

func TestSessionStatsCountsVisibleMessagesAndLatestUsage(t *testing.T) {
	home, path := writeStatsRollout(t,
		`{"timestamp":"2026-01-01T10:01:00Z","type":"event_msg","payload":{"type":"user_message","message":"CLI prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"CLI prompt"}]}}`,
		`{"timestamp":"2026-01-01T10:01:10Z","type":"event_msg","payload":{"type":"agent_message","message":"Commentary is transient","phase":"commentary"}}`,
		`{"timestamp":"2026-01-01T10:01:20Z","type":"event_msg","payload":{"type":"agent_reasoning","text":"private reasoning"}}`,
		`{"timestamp":"2026-01-01T10:02:00Z","type":"event_msg","payload":{"type":"agent_message","message":"Final answer","phase":"final_answer"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":100}}}}`,
		`{"timestamp":"2026-01-01T10:03:00Z","type":"event_msg","payload":{"type":"task_started","turn_id":"active-a","started_at":1767261780}}`,
		`{"type":"turn_context","payload":{"model":"example-model","effort":"high"}}`,
		`{"timestamp":"2026-01-01T10:03:00Z","type":"event_msg","payload":{"type":"user_message","message":"Telegram prompt SECRET_VALUE"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":150,"input_tokens":100,"cached_input_tokens":20,"output_tokens":50,"reasoning_output_tokens":30},"last_token_usage":{"total_tokens":75},"model_context_window":200}}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"tool output must not appear"}}`,
	)
	redactor, err := auth.NewRedactor([]string{"SECRET_VALUE"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cache := &rolloutStatsCache{}
	stats := cache.read(home, path, "thread-a", "active-a", redactor)
	if stats == nil || !stats.HistoryComplete {
		t.Fatalf("stats = %+v", stats)
	}
	assertStatsCount(t, "prompts", stats.PromptCount, 2)
	assertStatsCount(t, "answers", stats.AssistantMessageCount, 1)
	assertStatsCount(t, "total", stats.TotalTokens, 150)
	assertStatsCount(t, "input", stats.InputTokens, 100)
	assertStatsCount(t, "cached", stats.CachedInputTokens, 20)
	assertStatsCount(t, "output", stats.OutputTokens, 50)
	assertStatsCount(t, "reasoning", stats.ReasoningOutputTokens, 30)
	assertStatsCount(t, "context", stats.ContextTokens, 75)
	assertStatsCount(t, "window", stats.ContextWindow, 200)
	if stats.CreatedAt == nil || stats.CreatedAt.Format(time.RFC3339) != "2026-01-01T10:00:00Z" || stats.ActiveSince == nil || stats.LastMessageAt == nil {
		t.Fatalf("missing dates: %+v", stats)
	}
	if stats.Model != "example-model" || stats.ReasoningEffort != "high" || stats.LastMessageRole != "user" || stats.LastMessage != "Telegram prompt [REDACTED]" {
		t.Fatalf("last state = %+v", stats)
	}
	if again := cache.read(home, path, "thread-a", "active-a", redactor); !reflect.DeepEqual(stats, again) {
		t.Fatal("unchanged file produced different statistics")
	}
	if inactive := cache.read(home, path, "thread-a", "different-turn", redactor); inactive.ActiveSince != nil {
		t.Fatal("timestamp was associated with a different active turn")
	}
	if inactive := cache.read(home, path, "thread-a", "", redactor); inactive.ActiveSince != nil {
		t.Fatal("idle session has active-since timestamp")
	}
}

func TestSessionStatsAppendPartialLineAndReplaceFile(t *testing.T) {
	home, path := writeStatsRollout(t, `{"type":"event_msg","payload":{"type":"user_message","message":"First"}}`)
	cache := &rolloutStatsCache{}
	first := cache.read(home, path, "thread-a", "", nil)
	appendStats := func(value string) {
		t.Helper()
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(value); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendStats(`{"type":"event_msg","payload":{"type":"user_message","message":"Sec`)
	partial := cache.read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "partial prompts", partial.PromptCount, 1)
	if partial.HistoryComplete {
		t.Fatal("incomplete append was reported complete")
	}
	appendStats("ond\"}}\n")
	complete := cache.read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "complete prompts", complete.PromptCount, 2)
	assertStatsCount(t, "previous immutable snapshot", first.PromptCount, 1)
	if !complete.HistoryComplete || complete.LastMessage != "Second" || !complete.ObservedAt.After(first.ObservedAt) {
		t.Fatalf("append = %+v", complete)
	}
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"thread-a"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reset := cache.read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "reset prompts", reset.PromptCount, 0)
	if reset.LastMessage != "" || reset.TotalTokens != nil {
		t.Fatalf("replacement retained old data: %+v", reset)
	}
}

func TestSessionStatsLargeImagesDoNotStopOrDuplicateCounting(t *testing.T) {
	large := strings.Repeat("A", statsLineBytes+100)
	home, path := writeStatsRollout(t,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,`+large+`"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"What is this?","images":["data:image/jpeg;base64,`+large+`"]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","phase":"final_answer","message":"An example image."}}`,
	)
	stats := (&rolloutStatsCache{}).read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "image prompt", stats.PromptCount, 1)
	assertStatsCount(t, "image answer", stats.AssistantMessageCount, 1)
	if stats.HistoryComplete || stats.LastMessage != "An example image." {
		t.Fatalf("image metrics = %+v", stats)
	}
}

func TestSessionStatsLargeHistoryMakesIncrementalProgressWithRecentTail(t *testing.T) {
	home, path := writeStatsRollout(t,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Old prompt"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"`+strings.Repeat("x", statsScanBytes+100)+`"}}`,
		`{"timestamp":"2026-01-01T11:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"Latest prompt"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":500}}}}`,
	)
	cache := &rolloutStatsCache{}
	first := cache.read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "partial prompts", first.PromptCount, 1)
	assertStatsCount(t, "latest tokens during scan", first.TotalTokens, 500)
	if first.HistoryComplete || first.LastMessage != "Latest prompt" {
		t.Fatalf("bounded first scan = %+v", first)
	}
	second := cache.read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "full prompts", second.PromptCount, 2)
	if !second.HistoryComplete || second.LastMessage != "Latest prompt" {
		t.Fatalf("incremental scan = %+v", second)
	}
}

func TestSessionStatsRejectsWrongThreadAndExternalPaths(t *testing.T) {
	home, path := writeStatsRollout(t)
	cache := &rolloutStatsCache{}
	if got := cache.read(home, path, "thread-b", "", nil); got != nil {
		t.Fatal("read a different thread")
	}
	otherHome, otherPath := writeStatsRollout(t)
	if got := cache.read(home, otherPath, "thread-a", "", nil); got != nil {
		t.Fatal("read outside home")
	}
	link := filepath.Join(home, "sessions", "outside.jsonl")
	if err := os.Symlink(otherPath, link); err != nil {
		t.Fatal(err)
	}
	if got := cache.read(home, link, "thread-a", "", nil); got != nil {
		t.Fatal("followed external symlink")
	}
	if got := cache.read(otherHome, path, "thread-a", "", nil); got != nil {
		t.Fatal("cache bypassed home restriction")
	}
}

func TestSessionStatsUnknownHistoryAndSafeBoundedLastMessage(t *testing.T) {
	t.Run("response only", func(t *testing.T) {
		home, path := writeStatsRollout(t, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"May be injected context"}]}}`)
		stats := (&rolloutStatsCache{}).read(home, path, "thread-a", "", nil)
		if stats.PromptCount == nil || *stats.PromptCount != 1 || !stats.HistoryComplete || stats.TotalTokens != nil {
			t.Fatalf("response-only prompt or unavailable token metrics incorrect: %+v", stats)
		}
	})
	t.Run("redact before clipping", func(t *testing.T) {
		secret := "cwk_" + strings.Repeat("a", 64)
		message := strings.Repeat("🌿", 590) + secret + " more text"
		line, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": message}})
		if err != nil {
			t.Fatal(err)
		}
		home, path := writeStatsRollout(t, string(line))
		redactor, err := auth.NewRedactor([]string{`cwk_[a-fA-F0-9]{64}`}, "")
		if err != nil {
			t.Fatal(err)
		}
		stats := (&rolloutStatsCache{}).read(home, path, "thread-a", "", redactor)
		if len([]rune(stats.LastMessage)) != 601 || strings.Contains(stats.LastMessage, "cwk_") || !strings.HasSuffix(stats.LastMessage, "…") {
			t.Fatalf("unsafe clipping: %q", stats.LastMessage)
		}
	})
}

func TestSessionStatsCASComparesPersistedValues(t *testing.T) {
	store, runtime, session := inventoryStoreFixture(t)
	session.Stats = &protocol.SessionStats{PromptCount: statsNumber(3), HistoryComplete: true, ObservedAt: time.Now().UTC()}
	session, err := store.UpsertSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ArchiveDiscoveredSession(runtime, session); err != nil || !changed {
		t.Fatalf("statistics pointer identity broke archive compare-and-swap: changed=%v error=%v", changed, err)
	}
}

func TestSessionStatsResponseFallbackAcrossRuntimeFormats(t *testing.T) {
	home, path := writeStatsRollout(t,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"old"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Old prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Old prompt"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Old answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Old answer","phase":"final_answer"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"new"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /work\n<INSTRUCTIONS>private configuration</INSTRUCTIONS>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>injected</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"New prompt"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Steering prompt"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Progress"}]}}`,
		`{"type":"response_item","payload":{"type":"agent_message","content":[{"type":"input_text","text":"Private subagent conversation"}]}}`,
		`{"timestamp":"2026-01-01T12:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"New answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","content":"private"}}}`,
	)
	stats := (&rolloutStatsCache{}).read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "mixed prompts", stats.PromptCount, 3)
	assertStatsCount(t, "mixed replies", stats.AssistantMessageCount, 2)
	if !stats.HistoryComplete || stats.LastMessage != "New answer" || stats.LastMessageRole != "assistant" || stats.LastMessageAt == nil {
		t.Fatalf("fallback = %+v", stats)
	}
}

func TestSessionStatsContextualUserFragments(t *testing.T) {
	for _, input := range []string{
		"  <ENVIRONMENT_CONTEXT>hidden</ENVIRONMENT_CONTEXT>  ",
		"# AGENTS.md instructions for /work\n<INSTRUCTIONS>hidden</INSTRUCTIONS>",
		"<user_instructions>hidden</user_instructions>",
		"<skill>hidden</skill>", "<subagent_notification>hidden</subagent_notification>",
		"<recommended_plugins>hidden</recommended_plugins>",
		"<goal_context>hidden</goal_context>", "<external_context>hidden</external_context>",
		`<codex_internal_context source="limit_warning">hidden</codex_internal_context>`,
		`<hook_prompt hook_run_id="run-a">hidden</hook_prompt>`,
	} {
		if !statsContextualUserText(input) {
			t.Errorf("did not filter known contextual fragment %q", input)
		}
	}
	for _, input := range []string{"<project_context>User request</project_context>", "Explain <environment_context> tags", "<environment_context>unterminated example", "# AGENTS.md instructions: explain these"} {
		if statsContextualUserText(input) {
			t.Errorf("filtered real user text %q", input)
		}
	}
}

func TestSessionStatsResponseOnlyImageSalvageIsExplicitlyPartial(t *testing.T) {
	home, path := writeStatsRollout(t,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Identify this: "},{"type":"input_image","image_url":"data:image/jpeg;base64,`+strings.Repeat("A", statsLineBytes+100)+`"}]}}`,
	)
	stats := (&rolloutStatsCache{}).read(home, path, "thread-a", "", nil)
	assertStatsCount(t, "image prompt", stats.PromptCount, 1)
	if stats.HistoryComplete || stats.LastMessage != "Identify this: [Image]" {
		t.Fatalf("oversized response message = %+v", stats)
	}
}
