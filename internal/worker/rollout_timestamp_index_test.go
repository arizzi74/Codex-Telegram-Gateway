package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	bolt "github.com/iaia/telegramgw/internal/workerdb"
)

func timestampEvent(kind, timestamp, text, phase string) string {
	raw, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": timestamp, "payload": map[string]string{"type": kind, "message": text, "phase": phase}})
	return string(raw)
}

func timestampComplete(turn string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":%q}}`, turn)
}

func timestampSequence(parts ...[3]string) string {
	var sequence string
	for _, part := range parts {
		sequence = rolloutTimestampSequenceDigest(sequence, part[0], part[1], rolloutMessageDigest(part[0], part[2]))
	}
	return sequence
}

func timestampSynthetic(turn, role, phase, text, sequence string, messages, count, occurrence int) rolloutTimestampQuery {
	return rolloutTimestampQuery{TurnID: turn, ItemID: "item-1", Role: role, Phase: phase, Digest: rolloutMessageDigest(role, text), Synthetic: true, ExpectedMessages: messages, ExpectedOccurrences: count, Occurrence: occurrence, ExpectedSequenceDigest: sequence}
}

func timestampStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func timestampAppend(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(text)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("append: %v %v", err, closeErr)
	}
}

func TestRolloutTimestampIndexCanonicalEventsPhaseOccurrencesAndCompaction(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampEvent("user_message", "2026-09-20T08:00:00Z", "hello", ""),
		timestampRecord("item-1", "user", "2026-09-20T07:00:00Z", "hello"),
		timestampEvent("agent_message", "2026-09-20T08:01:00Z", "same", "commentary"),
		timestampEvent("agent_message", "2026-09-20T08:02:00Z", "same", "final_answer"),
		timestampRecord("mirror", "assistant", "2026-09-20T07:02:00Z", "same"),
		timestampEvent("agent_message", "2026-09-20T08:03:00Z", "same", "final_answer"),
		timestampEvent("context_compacted", "2026-09-20T08:04:00Z", "", ""), timestampComplete("turn"))
	sequence := timestampSequence([3]string{"user", "", "hello"}, [3]string{"assistant", "commentary", "same"}, [3]string{"assistant", "final_answer", "same"}, [3]string{"assistant", "final_answer", "same"}, [3]string{"compaction", "", ""})
	queries := []rolloutTimestampQuery{
		timestampSynthetic("turn", "user", "", "hello", sequence, 4, 1, 0),
		timestampSynthetic("turn", "assistant", "commentary", "same", sequence, 4, 1, 0),
		timestampSynthetic("turn", "assistant", "final_answer", "same", sequence, 4, 2, 0),
		timestampSynthetic("turn", "assistant", "final_answer", "same", sequence, 4, 2, 1),
		timestampSynthetic("turn", "compaction", "", "", sequence, 4, 1, 0),
		timestampSynthetic("turn", "assistant", "final_answer", "same", sequence, 5, 2, 0),
		timestampSynthetic("turn", "assistant", "final_answer", "same", sequence, 4, 1, 0),
		timestampSynthetic("turn", "assistant", "final_answer", "same", "changed-order", 4, 2, 0),
	}
	cache := &rolloutStatsCache{store: timestampStore(t)}
	results := cache.rolloutTimestampCandidates(home, path, "thread", queries)
	for i, result := range results {
		if i < 5 {
			if !result.Found || result.Timestamp.Format("15:04") != fmt.Sprintf("08:%02d", i) {
				t.Fatalf("canonical query %d: %+v", i, result)
			}
		} else if result.Found {
			t.Fatalf("ambiguous query %d: %+v", i, result)
		}
	}
	before := cache.timestamps.bytesRead
	cache.rolloutTimestampCandidates(home, path, "thread", queries)
	if cache.timestamps.bytesRead != before {
		t.Fatal("unchanged completed transcript was reread")
	}
}

func TestRolloutTimestampIndexExactIdentityLifecycleAndLateTools(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampRecord("duplicate", "user", "2026-09-20T08:00:00Z", "one"),
		timestampRecord("duplicate", "user", "2026-09-20T08:01:00Z", "two"),
		`{"timestamp":"2026-09-20T08:02:00Z","type":"event_msg","payload":{"type":"item_started","turn_id":"turn","item":{"type":"agentMessage","id":"native"}}}`,
		`{"timestamp":"2026-09-20T08:03:00Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"turn","item":{"type":"agentMessage","id":"native"}}}`,
		timestampComplete("turn"), timestampTurn("other"),
		`{"timestamp":"2026-09-20T08:04:00Z","type":"event_msg","payload":{"type":"patch_apply_end","turn_id":"turn","call_id":"patch"}}`,
		timestampRecord("duplicate", "user", "2026-09-20T08:05:00Z", "three"), timestampComplete("other"))
	cache := &rolloutStatsCache{store: timestampStore(t)}
	queries := []rolloutTimestampQuery{
		{TurnID: "turn", ItemID: "duplicate", Role: "user"}, {TurnID: "other", ItemID: "duplicate", Role: "user"},
		{TurnID: "turn", ItemID: "native", Role: "assistant"}, {TurnID: "turn", ItemID: "patch", Role: "tool"},
		{TurnID: "other", ItemID: "patch", Role: "tool"}, {TurnID: "turn", ItemID: "native", Role: "user"},
	}
	results := cache.rolloutTimestampCandidates(home, path, "thread", queries)
	for i, result := range results {
		if result.Found != (i >= 1 && i <= 3) {
			t.Fatalf("exact query %d: %+v", i, result)
		}
	}
	if results[2].Timestamp.Format("15:04") != "08:02" {
		t.Fatal("native completion replaced its start")
	}
}

func TestRolloutTimestampIndexBoundedTailRestartAndEviction(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampEvent("user_message", "2026-09-20T08:00:00Z", "old", ""),
		`{"type":"response_item","payload":{"type":"function_call_output","output":"`+strings.Repeat("x", rolloutTimestampScanBytes+rolloutTimestampTailBytes)+`"}}`,
		timestampComplete("turn"), timestampTurn("latest"),
		timestampEvent("user_message", "2026-09-20T09:00:00Z", "recent", ""),
		timestampEvent("agent_message", "2026-09-20T09:01:00Z", "answer", "final_answer"), timestampComplete("latest"))
	sequence := timestampSequence([3]string{"user", "", "recent"}, [3]string{"assistant", "final_answer", "answer"})
	queries := []rolloutTimestampQuery{timestampSynthetic("latest", "assistant", "final_answer", "answer", sequence, 2, 1, 0)}
	state, worker := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	cache := &rolloutStatsCache{store: store}
	if result := cache.rolloutTimestampCandidates(home, path, "thread", queries)[0]; !result.Found {
		t.Fatal("recent tail did not supply latest message on first bounded scan")
	}
	first := cache.timestamps.bytesRead
	if first > rolloutTimestampScanBytes+rolloutTimestampTailBytes+2*statsLineBytes {
		t.Fatalf("first scan exceeded bounds: %d", first)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted := &rolloutStatsCache{store: store}
	if result := restarted.rolloutTimestampCandidates(home, path, "thread", queries)[0]; !result.Found {
		t.Fatal("restart lost recent tail candidates")
	}
	if restarted.timestamps.bytesRead >= rolloutTimestampScanBytes {
		t.Fatalf("restart replayed historical scan: %d", restarted.timestamps.bytesRead)
	}
	second := restarted.timestamps.bytesRead
	restarted.timestamps.entries = nil // Cache eviction must retain SQLite progress.
	if result := restarted.rolloutTimestampCandidates(home, path, "thread", queries)[0]; !result.Found {
		t.Fatal("eviction lost timestamps")
	}
	if restarted.timestamps.bytesRead != second {
		t.Fatalf("completed eviction reread %d bytes", restarted.timestamps.bytesRead-second)
	}
	if got := store.loadRolloutTimestampPath("", home, "thread"); got != path {
		t.Fatalf("durable path %q", got)
	}
	t.Logf("first scan %d bytes; restart remainder %d; unchanged/evicted 0", first, second)
}

func TestRolloutTimestampIndexPartialPrivacyAndAppend(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread", timestampEvent("user_message", "2026-09-20T08:00:00Z", "private-first-content", ""), `{"type":"event_msg","payload":{"type":"agent_reasoning","text":"private-raw-reasoning"}}`)
	partial := `{"type":"event_msg","timestamp":"2026-09-20T08:01:00Z","payload":{"type":"agent_message","phase":"final_answer","message":"private-partial-content`
	timestampAppend(t, path, partial)
	store := timestampStore(t)
	cache := &rolloutStatsCache{store: store}
	query := timestampSynthetic("turn", "user", "", "private-first-content", timestampSequence([3]string{"user", "", "private-first-content"}), 1, 1, 0)
	if !cache.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
		t.Fatal("complete record before partial unavailable")
	}
	before := cache.timestamps.bytesRead
	restarted := &rolloutStatsCache{store: store}
	restarted.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})
	if restarted.timestamps.bytesRead != 0 {
		t.Fatal("unchanged unfinished record reread after restart")
	}
	if err := store.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).ForEach(func(key, value []byte) error {
			if bytes.HasPrefix(key, []byte("rollout_time")) && (bytes.Contains(value, []byte("private-")) || bytes.Contains(key, []byte("private-"))) {
				t.Fatal("index persisted conversation or reasoning")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	timestampAppend(t, path, `"}}`+"\n")
	sequence := timestampSequence([3]string{"user", "", "private-first-content"}, [3]string{"assistant", "final_answer", "private-partial-content"})
	query = timestampSynthetic("turn", "assistant", "final_answer", "private-partial-content", sequence, 2, 1, 0)
	if !restarted.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
		t.Fatal("partial append did not resume canonical message")
	}
	if restarted.timestamps.bytesRead > before+3*rolloutTimestampAnchorBytes {
		t.Fatal("append exceeded bounded metadata and partial-record reads")
	}
}

func TestRolloutTimestampDenseRecentTailContinuesWithoutRestarting(t *testing.T) {
	home := t.TempDir()
	var records []string
	records = append(records, `{"type":"response_item","payload":{"type":"function_call_output","output":"`+strings.Repeat("x", rolloutTimestampScanBytes+rolloutTimestampTailBytes)+`"}}`, timestampComplete("turn"))
	for range rolloutTimestampRecords + 1000 {
		records = append(records, `{"type":"response_item","payload":{"type":"function_call_output"}}`)
	}
	records = append(records, timestampTurn("latest"), timestampEvent("user_message", "2026-09-20T09:00:00Z", "recent", ""), timestampComplete("latest"))
	path := historyTimestampFixture(t, home, "thread", records...)
	cache := &rolloutStatsCache{store: timestampStore(t)}
	query := []rolloutTimestampQuery{timestampSynthetic("latest", "user", "", "recent", timestampSequence([3]string{"user", "", "recent"}), 1, 1, 0)}
	if cache.rolloutTimestampCandidates(home, path, "thread", query)[0].Found {
		t.Fatal("fixture did not exceed bounded tail record count")
	}
	var previousOffset int64
	for _, cp := range cache.timestamps.entries {
		previousOffset = cp.TailOffset
		if cp.TailOffset >= cp.Size {
			t.Fatal("partial tail was marked complete")
		}
	}
	restarted := &rolloutStatsCache{store: cache.store}
	if !restarted.rolloutTimestampCandidates(home, path, "thread", query)[0].Found {
		t.Fatal("durable recent-tail continuation lost latest turn")
	}
	for _, cp := range restarted.timestamps.entries {
		if cp.TailOffset <= previousOffset {
			t.Fatal("tail restarted rather than advancing its durable offset")
		}
	}
}

func TestRolloutTimestampCorruptCheckpointIsBoundedAndRebuilt(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread", timestampRecord("message", "user", "2026-09-20T08:00:00Z", "value"))
	store := timestampStore(t)
	cache := &rolloutStatsCache{store: store}
	query := []rolloutTimestampQuery{{TurnID: "turn", ItemID: "message", Role: "user"}}
	if !cache.rolloutTimestampCandidates(home, path, "thread", query)[0].Found {
		t.Fatal("initial message unavailable")
	}
	for scope, cp := range cache.timestamps.entries {
		broken := *cp
		broken.AnchorLength = 1 << 40
		value, _ := json.Marshal(broken)
		if err := store.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).Put([]byte(scope+"/checkpoint"), value) }); err != nil {
			t.Fatal(err)
		}
		broken.Generation = strings.Repeat("x", 36)
		if validRolloutTimestampCheckpoint(broken) {
			t.Fatal("malformed generation accepted")
		}
	}
	restarted := &rolloutStatsCache{store: store}
	if !restarted.rolloutTimestampCandidates(home, path, "thread", query)[0].Found {
		t.Fatal("corrupt checkpoint was not rebuilt")
	}
	if restarted.timestamps.bytesRead > 3*statsLineBytes {
		t.Fatal("corrupt checkpoint caused an unbounded read")
	}
}

func TestRolloutTimestampIndexInvalidationAndIsolation(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread", timestampRecord("message", "user", "2026-09-20T08:00:00Z", "one"))
	cache := &rolloutStatsCache{store: timestampStore(t), namespace: "scope-a"}
	query := []rolloutTimestampQuery{{TurnID: "turn", ItemID: "message", Role: "user"}}
	if !cache.rolloutTimestampCandidates(home, path, "thread", query)[0].Found {
		t.Fatal("initial identity unavailable")
	}
	replacement := filepath.Join(home, "sessions", "replacement.jsonl")
	data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread\"}}\n%s\n%s\n", timestampTurn("turn"), timestampRecord("message", "user", "2026-09-20T09:00:00Z", "two"))
	if err := os.WriteFile(replacement, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if result := cache.rolloutTimestampCandidates(home, path, "thread", query)[0]; !result.Found || result.Timestamp.Hour() != 9 {
		t.Fatalf("inode replacement retained old identity: %+v", result)
	}
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"thread"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if cache.rolloutTimestampCandidates(home, path, "thread", query)[0].Found {
		t.Fatal("truncate retained old timestamp")
	}
	otherHome := t.TempDir()
	otherPath := historyTimestampFixture(t, otherHome, "thread", timestampRecord("message", "user", "2026-09-20T10:00:00Z", "other"))
	for _, target := range []struct{ home, path, thread string }{{home, otherPath, "thread"}, {otherHome, otherPath, "wrong-thread"}} {
		if cache.rolloutTimestampCandidates(target.home, target.path, target.thread, query)[0].Found {
			t.Fatal("cross-home/thread timestamp exposed")
		}
	}
	symlink := filepath.Join(home, "sessions", "escape.jsonl")
	if err := os.Symlink(otherPath, symlink); err != nil {
		t.Fatal(err)
	}
	if cache.rolloutTimestampCandidates(home, symlink, "thread", query)[0].Found {
		t.Fatal("symlink escaped rooted home")
	}
}

func TestRolloutTimestampIndexRollbackAndReusedTurnRemainFallback(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread", timestampEvent("user_message", "2026-09-20T08:00:00Z", "old", ""), timestampComplete("turn"),
		`{"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}`, timestampTurn("new"), timestampEvent("user_message", "2026-09-20T09:00:00Z", "new", ""), timestampComplete("new"))
	cache := &rolloutStatsCache{store: timestampStore(t)}
	queries := []rolloutTimestampQuery{timestampSynthetic("turn", "user", "", "old", timestampSequence([3]string{"user", "", "old"}), 1, 1, 0), timestampSynthetic("new", "user", "", "new", timestampSequence([3]string{"user", "", "new"}), 1, 1, 0)}
	results := cache.rolloutTimestampCandidates(home, path, "thread", queries)
	if results[0].Found || !results[1].Found {
		t.Fatalf("rollback cutoff: %+v", results)
	}
	timestampAppend(t, path, timestampTurn("new")+"\n"+timestampEvent("user_message", "2026-09-20T10:00:00Z", "new", "")+"\n"+timestampComplete("new")+"\n")
	if cache.rolloutTimestampCandidates(home, path, "thread", queries)[1].Found {
		t.Fatal("reused turn identity remained unambiguous")
	}
}

func TestRolloutTimestampDiscoveryBudgetRotationAndTwoHomes(t *testing.T) {
	cache := &rolloutStatsCache{store: timestampStore(t)}
	var homes []string
	var lists [][]rolloutTimestampFile
	for h := 0; h < 2; h++ {
		home := t.TempDir()
		homes = append(homes, home)
		var files []rolloutTimestampFile
		for i := 0; i < 3; i++ {
			id := fmt.Sprintf("thread-%d", i)
			path := filepath.Join(home, "sessions", id+".jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n%s\n%s\n", id, timestampTurn(id), timestampRecord("message", "user", "2026-09-20T08:00:00Z", "value"))
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			files = append(files, rolloutTimestampFile{path, id})
		}
		lists = append(lists, files)
	}
	cache.discoverRolloutTimestampPass(homes[0], lists[0])
	initial := cache.timestamps.bytesRead
	cache.discoverRolloutTimestampPass(homes[1], lists[1])
	if cache.timestamps.bytesRead != initial {
		t.Fatal("second home exceeded shared work budget")
	}
	cache.timestamps.discoveryAfter = time.Time{}
	cache.discoverRolloutTimestampPass(homes[0], lists[0]) // Yields to recently seen other home.
	if cache.timestamps.bytesRead != initial {
		t.Fatal("first home monopolized global budget")
	}
	cache.discoverRolloutTimestampPass(homes[1], lists[1])
	if cache.timestamps.bytesRead == initial {
		t.Fatal("other home did not progress")
	}
	cache.timestamps.discoveryAfter = time.Time{}
	cache.discoverRolloutTimestampPass(homes[0], lists[0])
	if len(cache.timestamps.entries) != 3 {
		t.Fatalf("rotating pass did not advance later session: %d", len(cache.timestamps.entries))
	}
	before := cache.timestamps.bytesRead
	cache.discoverRolloutTimestampPass(homes[0], lists[0])
	if cache.timestamps.bytesRead != before {
		t.Fatal("same pass exceeded throttle")
	}
}
