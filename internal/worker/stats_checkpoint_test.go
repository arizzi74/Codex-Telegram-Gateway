package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestSQLiteStatsCheckpointsAvoidRereadingEvictedAndRestartedHistory(t *testing.T) {
	const sessions = statsCacheSize + 32
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, sessions)
	ids := make([]string, sessions)
	for i := range sessions {
		ids[i] = fmt.Sprintf("thread-%d", i)
		paths[i] = filepath.Join(home, "sessions", ids[i]+".jsonl")
		data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"First\"}}\n", ids[i])
		if err := os.WriteFile(paths[i], []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state, worker := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	cache := &rolloutStatsCache{store: store, namespace: "redaction-v1"}
	readAll := func(c *rolloutStatsCache) {
		t.Helper()
		for i, path := range paths {
			stats := c.read(home, path, ids[i], "", nil)
			if stats == nil || !stats.HistoryComplete {
				t.Fatalf("missing history checkpoint for %s: %+v", ids[i], stats)
			}
			assertStatsCount(t, "saved prompts", stats.PromptCount, 1)
		}
	}
	readAll(cache)
	firstRead := cache.bytesRead
	if firstRead == 0 || len(cache.entries) != statsCacheSize {
		t.Fatal("fixture did not exceed memory cache")
	}
	readAll(cache)
	if cache.bytesRead != firstRead {
		t.Fatalf("eviction rescanned completed history: bytes %d -> %d", firstRead, cache.bytesRead)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted := &rolloutStatsCache{store: store, namespace: "redaction-v1"}
	readAll(restarted)
	if restarted.bytesRead != 0 {
		t.Fatalf("restart reread %d history bytes", restarted.bytesRead)
	}
	appendix := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Second\"}}\n"
	f, err := os.OpenFile(paths[0], os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(appendix)
	if closeErr := f.Close(); err != nil || closeErr != nil {
		t.Fatalf("append: %v %v", err, closeErr)
	}
	stats := restarted.read(home, paths[0], ids[0], "", nil)
	if stats == nil {
		t.Fatal("appended history unavailable")
	}
	assertStatsCount(t, "appended prompts", stats.PromptCount, 2)
	if restarted.bytesRead != int64(len(appendix)) {
		t.Fatalf("append reread old history: %d vs new %d", restarted.bytesRead, len(appendix))
	}
	t.Logf("%d sessions: initial %d bytes, unchanged/restart 0 bytes, append %d bytes", sessions, firstRead, restarted.bytesRead)
}

func TestSQLiteStatsCheckpointKeepsPartialLinesAndInvalidatesChangedPolicy(t *testing.T) {
	home, path := writeStatsRollout(t, `{"type":"event_msg","payload":{"type":"user_message","message":"First"}}`)
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	appendText := func(text string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(text)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	appendText(`{"type":"event_msg","payload":{"type":"user_message","message":"Sec`)
	cache := &rolloutStatsCache{store: store, namespace: "policy-1"}
	first := cache.read(home, path, "thread-a", "", nil)
	if first == nil || first.HistoryComplete {
		t.Fatal("partial line was lost")
	}
	appendix := "ond\"}}\n"
	appendText(appendix)
	restarted := &rolloutStatsCache{store: store, namespace: "policy-1"}
	stats := restarted.read(home, path, "thread-a", "", nil)
	if stats == nil || !stats.HistoryComplete {
		t.Fatal("partial checkpoint did not resume")
	}
	assertStatsCount(t, "completed partial line", stats.PromptCount, 2)
	if restarted.bytesRead != int64(len(appendix)) {
		t.Fatal("partial checkpoint reread old bytes")
	}
	changed := &rolloutStatsCache{store: store, namespace: "policy-2"}
	if changed.read(home, path, "thread-a", "", nil) == nil || changed.bytesRead == 0 {
		t.Fatal("changed redaction policy reused cached text")
	}
}
