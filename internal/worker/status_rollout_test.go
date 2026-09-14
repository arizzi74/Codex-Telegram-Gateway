package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRolloutStatusUsesSelectedThreadAndLatestCounters(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "sessions", "status.jsonl")
	data := `{"type":"session_meta","payload":{"id":"thread-a"}}
{"type":"turn_context","payload":{"approval_policy":"on-request","sandbox_policy":{"type":"workspace-write","writable_roots":["/work"]}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":100},"last_token_usage":{"total_tokens":25},"model_context_window":200}}}
{"type":"response_item","payload":{"secret":"DO NOT INCLUDE PROMPTS"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":150},"last_token_usage":{"total_tokens":50},"model_context_window":200}}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(readRolloutStatus(home, path, "thread-a"), "\n")
	for _, expected := range []string{"50 / 200 tokens (25% used)", "Session tokens: 150", "on-request", "workspace-write", "/work"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in %q", expected, text)
		}
	}
	if strings.Contains(text, "DO NOT INCLUDE") {
		t.Fatal("non-status content disclosed")
	}
	if got := readRolloutStatus(home, path, "thread-b"); len(got) != 0 {
		t.Fatalf("read another thread's status: %v", got)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readRolloutStatus(home, outside, "thread-a"); len(got) != 0 {
		t.Fatal("read external transcript")
	}
	link := filepath.Join(home, "sessions", "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if got := readRolloutStatus(home, link, "thread-a"); len(got) != 0 {
		t.Fatal("followed external symlink")
	}
}
