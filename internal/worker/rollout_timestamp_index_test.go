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

func timestampActivity(id, kind, timestamp, turn string) string {
	raw, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": timestamp, "payload": map[string]any{
		"type": "sub_agent_activity", "event_id": id, "kind": kind, "turn_id": turn,
		"agent_thread_id": "private-agent-thread", "agent_path": "private-agent-path", "model": "private-agent-model",
		"occurred_at_ms": 1, // The recovered date must come from the rollout record.
	}})
	return string(raw)
}

func timestampLifecycle(kind, itemType, id, turn, timestamp string) string {
	raw, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": timestamp, "payload": map[string]any{
		"type": kind, "turn_id": turn, "item": map[string]string{"type": itemType, "id": id},
	}})
	return string(raw)
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

func TestRolloutTimestampIndexSnapshotPrefixAfterRepeatedAppendAndCompaction(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprintf("durable=%t", durable), func(t *testing.T) {
			home := t.TempDir()
			path := historyTimestampFixture(t, home, "thread",
				timestampEvent("user_message", "2026-10-09T08:00:00Z", "request", ""),
				timestampEvent("agent_message", "2026-10-09T08:01:00Z", "same", "commentary"),
				timestampEvent("agent_message", "2026-10-09T08:02:00Z", "same", "commentary"),
				timestampEvent("context_compacted", "2026-10-09T08:03:00Z", "", ""),
				`{"type":"response_item","timestamp":"2026-10-09T08:04:00Z","payload":{"type":"function_call","call_id":"tool"}}`)
			cache := &rolloutStatsCache{}
			if durable {
				cache.store = timestampStore(t)
			}
			tool := rolloutTimestampQuery{TurnID: "turn", ItemID: "tool", Role: "tool"}
			if !cache.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{tool})[0].Found {
				t.Fatal("initial exact tool was not indexed")
			}
			sequence := timestampSequence([3]string{"user", "", "request"}, [3]string{"assistant", "commentary", "same"}, [3]string{"assistant", "commentary", "same"}, [3]string{"compaction", "", ""})
			queries := []rolloutTimestampQuery{
				timestampSynthetic("turn", "assistant", "commentary", "same", sequence, 3, 2, 0),
				timestampSynthetic("turn", "assistant", "commentary", "same", sequence, 3, 2, 1),
				timestampSynthetic("turn", "compaction", "", "", sequence, 3, 1, 0),
			}
			// None of these snapshot items was queried before the source grew.
			// Later identical messages and compactions must stay outside its proof.
			timestampAppend(t, path, strings.Join([]string{
				timestampEvent("agent_message", "2026-10-09T08:05:00Z", "same", "commentary"),
				timestampEvent("context_compacted", "2026-10-09T08:06:00Z", "", ""),
				timestampEvent("agent_message", "2026-10-09T08:07:00Z", "same", "commentary"),
			}, "\n")+"\n")
			for i, result := range cache.rolloutTimestampCandidates(home, path, "thread", queries) {
				if !result.Found || result.Timestamp.Format("15:04") != fmt.Sprintf("08:%02d", i+1) {
					t.Fatalf("unvisited snapshot prefix %d: %+v", i, result)
				}
			}
			latestSequence := timestampSequence([3]string{"user", "", "request"}, [3]string{"assistant", "commentary", "same"}, [3]string{"assistant", "commentary", "same"}, [3]string{"compaction", "", ""}, [3]string{"assistant", "commentary", "same"}, [3]string{"compaction", "", ""}, [3]string{"assistant", "commentary", "same"})
			latest := timestampSynthetic("turn", "assistant", "commentary", "same", latestSequence, 5, 4, 3)
			if result := cache.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{latest})[0]; !result.Found || result.Timestamp.Format("15:04") != "08:07" {
				t.Fatalf("latest whole-turn proof: %+v", result)
			}
		})
	}
}

func TestRolloutTimestampIndexSnapshotPrefixCutoffAndStrictProof(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampEvent("agent_message", "2026-10-09T08:00:00Z", "first", "commentary"),
		timestampEvent("agent_message", "2026-10-09T08:01:00Z", "same", "commentary"))
	cache := &rolloutStatsCache{store: timestampStore(t)}
	sequence := timestampSequence([3]string{"assistant", "commentary", "first"}, [3]string{"assistant", "commentary", "same"})
	query := timestampSynthetic("turn", "assistant", "commentary", "same", sequence, 2, 1, 0)
	if !cache.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
		t.Fatal("initial snapshot unavailable")
	}
	// The appended candidate begins exactly at the previous EOF. An inclusive
	// cutoff based on the old record's exclusive end would count it incorrectly.
	timestampAppend(t, path, timestampEvent("agent_message", "2026-10-09T08:02:00Z", "same", "commentary")+"\n")
	wrongCount, wrongOrder, wrongOccurrences, wrongPhase, changedText := query, query, query, query, query
	wrongCount.ExpectedMessages++
	wrongOrder.ExpectedSequenceDigest = timestampSequence([3]string{"assistant", "commentary", "same"}, [3]string{"assistant", "commentary", "first"})
	wrongOccurrences.ExpectedOccurrences++
	wrongPhase.Phase = "final_answer"
	changedText.Digest = rolloutMessageDigest("assistant", "changed")
	queries := []rolloutTimestampQuery{query, wrongCount, wrongOrder, wrongOccurrences, wrongPhase, changedText}
	for i, result := range cache.rolloutTimestampCandidates(home, path, "thread", queries) {
		if result.Found != (i == 0) {
			t.Fatalf("strict snapshot proof %d: %+v", i, result)
		}
		if i == 0 && result.Timestamp.Format("15:04") != "08:01" {
			t.Fatalf("snapshot acquired appended occurrence: %+v", result)
		}
	}
}

func TestRolloutTimestampIndexSnapshotPrefixRetainsCoverageGuards(t *testing.T) {
	for _, scenario := range []struct{ name, suffix string }{
		{"digest gap", `{"type":"event_msg","payload":{"type":"sub_agent_activity","event_id":7}}`},
		{"rollback", `{"type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}` + "\n" + timestampTurn("other")},
		{"reused turn", timestampComplete("turn") + "\n" + timestampTurn("turn")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			home := t.TempDir()
			path := historyTimestampFixture(t, home, "thread", timestampEvent("agent_message", "2026-10-09T08:00:00Z", "old", "commentary"))
			cache := &rolloutStatsCache{store: timestampStore(t)}
			query := timestampSynthetic("turn", "assistant", "commentary", "old", timestampSequence([3]string{"assistant", "commentary", "old"}), 1, 1, 0)
			if !cache.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
				t.Fatal("initial snapshot unavailable")
			}
			timestampAppend(t, path, scenario.suffix+"\n"+timestampEvent("agent_message", "2026-10-09T08:01:00Z", "later", "commentary")+"\n")
			if cache.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
				t.Fatal("saved prefix bypassed invalidated turn coverage")
			}
		})
	}
}

func TestRolloutTimestampIndexVersionTwoEOFPrefixUpgradeAndRestart(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampEvent("user_message", "2026-10-09T08:00:00Z", "request", ""),
		timestampEvent("agent_message", "2026-10-09T08:01:00Z", "same", "commentary"))
	state, worker := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	query := timestampSynthetic("turn", "assistant", "commentary", "same", timestampSequence([3]string{"user", "", "request"}, [3]string{"assistant", "commentary", "same"}), 2, 1, 0)
	seed := &rolloutStatsCache{store: store}
	if !seed.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
		t.Fatal("initial snapshot unavailable")
	}
	var scope string
	var legacy rolloutTimestampCheckpoint
	for key, cp := range seed.timestamps.entries {
		scope, legacy = key, *cp
	}
	if scope == "" || legacy.Offset != legacy.Size {
		t.Fatal("fixture did not reach durable EOF")
	}
	legacy.Version = 2
	if err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		prefix := []byte(scope + "/" + legacy.Generation + "/sequence/")
		cursor := bucket.Cursor()
		for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		value, err := json.Marshal(legacy)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(scope+"/checkpoint"), value)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := &rolloutStatsCache{store: store}
	if !upgraded.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0].Found {
		t.Fatal("v2 EOF upgrade lost current snapshot")
	}
	current := upgraded.timestamps.entries[scope]
	if current.Version != rolloutTimestampIndexVersion || current.Generation == legacy.Generation || current.Offset != current.Size {
		t.Fatalf("v2 EOF was resumed rather than reindexed: %+v", current)
	}
	if upgraded.timestamps.bytesRead == 0 || upgraded.timestamps.bytesRead > rolloutTimestampScanBytes+rolloutTimestampTailBytes+2*statsLineBytes {
		t.Fatalf("v2 EOF upgrade exceeded bounded work: %d", upgraded.timestamps.bytesRead)
	}
	timestampAppend(t, path, timestampEvent("agent_message", "2026-10-09T08:02:00Z", "same", "commentary")+"\n")
	if result := upgraded.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0]; !result.Found || result.Timestamp.Format("15:04") != "08:01" {
		t.Fatalf("upgrade did not backfill immutable prefix proofs: %+v", result)
	}
	generation := current.Generation
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &rolloutStatsCache{store: store}
	if result := restarted.rolloutTimestampCandidates(home, path, "thread", []rolloutTimestampQuery{query})[0]; !result.Found || result.Timestamp.Format("15:04") != "08:01" {
		t.Fatalf("restart lost immutable snapshot proof: %+v", result)
	}
	if restarted.timestamps.bytesRead != 0 || restarted.timestamps.entries[scope].Generation != generation {
		t.Fatal("restart replayed an unchanged v3 index")
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

func TestRolloutTimestampIndexActivityExactIDsAndTurnIsolation(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampEvent("user_message", "2026-10-08T23:59:50+02:00", "request", ""),
		timestampActivity("subagent-interaction-agent-uuid-9", "interacted", "2026-10-08T23:59:58+02:00", ""),
		timestampActivity("shared-event", "interacted", "2026-10-08T23:59:59+02:00", ""),
		timestampComplete("turn"), timestampTurn("other"),
		timestampActivity("subagent-completed-agent-uuid", "completed", "2026-10-09T00:00:02+02:00", ""),
		timestampActivity("shared-event", "interacted", "2026-10-09T00:00:03+02:00", ""),
		timestampActivity("late-explicit", "completed", "2026-10-09T00:00:04+02:00", "turn"),
		timestampComplete("other"),
		timestampActivity("unknown-current-turn", "completed", "2026-10-09T00:00:05+02:00", ""))
	queries := []rolloutTimestampQuery{
		{TurnID: "turn", ItemID: "subagent-interaction-agent-uuid-9", Role: "activity"},
		{TurnID: "other", ItemID: "subagent-completed-agent-uuid", Role: "activity"},
		{TurnID: "turn", ItemID: "shared-event", Role: "activity"},
		{TurnID: "other", ItemID: "shared-event", Role: "activity"},
		{TurnID: "turn", ItemID: "late-explicit", Role: "activity"},
		{TurnID: "other", ItemID: "late-explicit", Role: "activity"},
		{TurnID: "other", ItemID: "subagent-interaction-agent-uuid-9", Role: "activity"},
		{TurnID: "turn", ItemID: "subagent-interaction-agent-uuid-9", Role: "assistant"},
		{TurnID: "turn", ItemID: "private-agent-thread", Role: "activity"},
		{TurnID: "turn", ItemID: "subagent-interaction-agent-uuid", Role: "activity"},
		{TurnID: "other", ItemID: "unknown-current-turn", Role: "activity"},
		timestampSynthetic("turn", "user", "", "request", timestampSequence([3]string{"user", "", "request"}), 1, 1, 0),
	}
	cache := &rolloutStatsCache{store: timestampStore(t)}
	results := cache.rolloutTimestampCandidates(home, path, "thread", queries)
	expected := []string{"2026-10-08T21:59:58Z", "2026-10-08T22:00:02Z", "2026-10-08T21:59:59Z", "2026-10-08T22:00:03Z", "2026-10-08T22:00:04Z"}
	for i, result := range results {
		if i < len(expected) {
			if !result.Found || result.Timestamp.Format(time.RFC3339) != expected[i] {
				t.Fatalf("activity %d: %+v", i, result)
			}
		} else if i == len(results)-1 {
			if !result.Found {
				t.Fatal("activity changed canonical message sequence or count")
			}
		} else if result.Found {
			t.Fatalf("unverified activity %d acquired a timestamp: %+v", i, result)
		}
	}
	if err := cache.store.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).ForEach(func(key, value []byte) error {
			if bytes.HasPrefix(key, []byte("rollout_time")) && (bytes.Contains(value, []byte("private-agent")) || bytes.Contains(key, []byte("private-agent"))) {
				t.Fatal("activity index persisted agent payload metadata")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRolloutTimestampIndexLifecycleCoreAndNativeSpellings(t *testing.T) {
	variants := []struct{ core, native, role string }{
		{"UserMessage", "userMessage", "user"}, {"AgentMessage", "agentMessage", "assistant"},
		{"CommandExecution", "commandExecution", "tool"}, {"McpToolCall", "mcpToolCall", "tool"},
		{"DynamicToolCall", "dynamicToolCall", "tool"}, {"FileChange", "fileChange", "tool"},
		{"WebSearch", "webSearch", "tool"}, {"ImageGeneration", "imageGeneration", "tool"},
		{"CollabAgentToolCall", "collabAgentToolCall", "tool"}, {"SubAgentActivity", "subAgentActivity", "activity"},
	}
	var records []string
	var queries []rolloutTimestampQuery
	for _, variant := range variants {
		for _, spelling := range []string{variant.core, variant.native} {
			records = append(records,
				timestampLifecycle("item_started", spelling, spelling, "turn", "2026-10-08T23:59:58Z"),
				timestampLifecycle("item_completed", spelling, spelling, "turn", "2026-10-09T00:00:02Z"))
			queries = append(queries, rolloutTimestampQuery{TurnID: "turn", ItemID: spelling, Role: variant.role})
		}
	}
	for _, unsupported := range []string{"subagentactivity", "SUBAGENTACTIVITY", "SubagentActivity", "sub_agent_activity", "Unknown", "Reasoning", "Plan"} {
		records = append(records, timestampLifecycle("item_completed", unsupported, unsupported, "turn", "2026-10-09T00:00:03Z"))
		queries = append(queries, rolloutTimestampQuery{TurnID: "turn", ItemID: unsupported, Role: "activity"})
	}
	records = append(records, timestampComplete("turn"), timestampTurn("other"),
		timestampLifecycle("item_completed", "SubAgentActivity", "late-native", "turn", "2026-10-09T00:00:04Z"),
		timestampLifecycle("item_completed", "SubAgentActivity", "unseen-turn", "missing", "2026-10-09T00:00:05Z"),
		timestampComplete("other"))
	queries = append(queries,
		rolloutTimestampQuery{TurnID: "turn", ItemID: "late-native", Role: "activity"},
		rolloutTimestampQuery{TurnID: "other", ItemID: "late-native", Role: "activity"},
		rolloutTimestampQuery{TurnID: "missing", ItemID: "unseen-turn", Role: "activity"},
		rolloutTimestampQuery{TurnID: "other", ItemID: "unseen-turn", Role: "activity"})
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread", records...)
	cache := &rolloutStatsCache{store: timestampStore(t)}
	for i, result := range cache.rolloutTimestampCandidates(home, path, "thread", queries) {
		switch {
		case i < 2*len(variants):
			if !result.Found || result.Timestamp.Format(time.RFC3339) != "2026-10-08T23:59:58Z" {
				t.Fatalf("lifecycle spelling %q lost start: %+v", queries[i].ItemID, result)
			}
		case i == len(queries)-4:
			if !result.Found || result.Timestamp.Format(time.RFC3339) != "2026-10-09T00:00:04Z" {
				t.Fatalf("late native completion lost explicit parent turn: %+v", result)
			}
		default:
			if result.Found {
				t.Fatalf("unsupported or wrong-turn lifecycle %d acquired a timestamp: %+v", i, result)
			}
		}
	}
}

func TestRolloutTimestampIndexMalformedActivitiesRemainUnmatched(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		`{"timestamp":"2026-10-09T08:00:00Z","type":"event_msg","payload":{"type":"sub_agent_activity","id":"wrong-id-field","call_id":"wrong-call"}}`,
		`{"timestamp":"2026-10-09T08:00:00Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":7}}`,
		`{"timestamp":"2026-10-09T08:00:00Z","type":"event_msg","payload":{"type":"sub_agent_activity","event_id":null}}`,
		`{"timestamp":"2026-10-09T08:00:00Z","type":"event_msg","payload":{"type":"Sub_Agent_Activity","event_id":"wrong-event-case"}}`,
		timestampActivity("invalid-date", "interacted", "invalid", ""),
		timestampActivity("before-epoch", "interacted", "1969-12-31T23:59:59Z", ""),
		timestampActivity("long-turn", "interacted", "2026-10-09T08:00:00Z", strings.Repeat("t", 257)),
		timestampActivity(strings.Repeat("i", 257), "interacted", "2026-10-09T08:00:00Z", ""),
		timestampActivity("duplicate-activity", "interacted", "2026-10-09T08:00:00Z", ""),
		timestampActivity("duplicate-activity", "completed", "2026-10-09T08:01:00Z", ""),
		timestampActivity("valid-after-malformed", "interacted", "2026-10-09T08:02:00Z", ""), timestampComplete("turn"))
	ids := []string{"wrong-id-field", "wrong-call", "wrong-event-case", "invalid-date", "before-epoch", "long-turn", strings.Repeat("i", 257), "duplicate-activity", "valid-after-malformed"}
	queries := make([]rolloutTimestampQuery, len(ids))
	for i, id := range ids {
		queries[i] = rolloutTimestampQuery{TurnID: "turn", ItemID: id, Role: "activity"}
	}
	cache := &rolloutStatsCache{store: timestampStore(t)}
	for i, result := range cache.rolloutTimestampCandidates(home, path, "thread", queries) {
		if result.Found != (i == len(ids)-1) {
			t.Fatalf("malformed activity %q: %+v", ids[i], result)
		}
	}
}

func TestRolloutTimestampIndexActivityLifecyclePrecedence(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampActivity("canonical", "completed", "2026-10-09T08:00:00Z", ""),
		timestampLifecycle("item_completed", "SubAgentActivity", "canonical", "turn", "2026-10-09T08:01:00Z"),
		timestampLifecycle("item_started", "SubAgentActivity", "started", "turn", "2026-10-09T08:02:00Z"),
		timestampLifecycle("item_completed", "SubAgentActivity", "started", "turn", "2026-10-09T08:03:00Z"),
		timestampLifecycle("item_completed", "SubAgentActivity", "ambiguous", "turn", "2026-10-09T08:04:00Z"),
		timestampLifecycle("item_completed", "SubAgentActivity", "ambiguous", "turn", "2026-10-09T08:05:00Z"), timestampComplete("turn"))
	queries := []rolloutTimestampQuery{
		{TurnID: "turn", ItemID: "canonical", Role: "activity"},
		{TurnID: "turn", ItemID: "started", Role: "activity"},
		{TurnID: "turn", ItemID: "ambiguous", Role: "activity"},
	}
	cache := &rolloutStatsCache{store: timestampStore(t)}
	results := cache.rolloutTimestampCandidates(home, path, "thread", queries)
	if !results[0].Found || results[0].Timestamp.Format("15:04") != "08:01" {
		t.Fatalf("canonical lifecycle lost precedence over legacy mirror: %+v", results[0])
	}
	if !results[1].Found || results[1].Timestamp.Format("15:04") != "08:02" {
		t.Fatalf("native completion replaced start: %+v", results[1])
	}
	if results[2].Found {
		t.Fatalf("duplicate canonical activity identity acquired a timestamp: %+v", results[2])
	}
}

func TestRolloutTimestampIndexActivityAndToolCallIDsRemainDistinct(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		`{"timestamp":"2026-10-08T23:50:26.277Z","type":"response_item","payload":{"type":"function_call","call_id":"call_shared"}}`,
		timestampActivity("call_shared", "interacted", "2026-10-08T23:50:26.280Z", ""),
		`{"timestamp":"2026-10-08T23:51:00Z","type":"response_item","payload":{"type":"function_call","call_id":"call_only"}}`,
		timestampActivity("activity_only", "interacted", "2026-10-08T23:51:01Z", ""),
		`{"timestamp":"2026-10-08T23:51:02Z","type":"response_item","payload":{"type":"function_call","call_id":"duplicate_tool"}}`,
		`{"timestamp":"2026-10-08T23:51:03Z","type":"response_item","payload":{"type":"function_call","call_id":"duplicate_tool"}}`,
		timestampActivity("duplicate_tool", "interacted", "2026-10-08T23:51:04Z", ""), timestampComplete("turn"))
	queries := []rolloutTimestampQuery{
		{TurnID: "turn", ItemID: "call_shared", Role: "tool"},
		{TurnID: "turn", ItemID: "call_shared", Role: "activity"},
		{TurnID: "turn", ItemID: "duplicate_tool", Role: "activity"},
		{TurnID: "turn", ItemID: "call_only", Role: "activity"},
		{TurnID: "turn", ItemID: "activity_only", Role: "tool"},
		{TurnID: "turn", ItemID: "duplicate_tool", Role: "tool"},
	}
	cache := &rolloutStatsCache{store: timestampStore(t)}
	results := cache.rolloutTimestampCandidates(home, path, "thread", queries)
	expected := []string{"2026-10-08T23:50:26.277Z", "2026-10-08T23:50:26.280Z", "2026-10-08T23:51:04Z"}
	for i, result := range results {
		if i < len(expected) {
			stamp, err := time.Parse(time.RFC3339Nano, expected[i])
			if err != nil {
				t.Fatal(err)
			}
			if !result.Found || !result.Timestamp.Equal(stamp) {
				t.Fatalf("shared identifier domain %d: %+v", i, result)
			}
		} else if result.Found {
			t.Fatalf("identity leaked across activity and tool domains %d: %+v", i, result)
		}
	}
}

func TestRolloutTimestampIndexVersionOneEOFUpgradeAndRestart(t *testing.T) {
	home := t.TempDir()
	var records []string
	for i := range 300 {
		records = append(records, timestampRecord(fmt.Sprintf("message-%d", i), "user", "2026-10-09T08:00:00Z", "value"))
	}
	records = append(records,
		timestampActivity("legacy-activity", "interacted", "2026-10-09T08:01:00Z", ""),
		timestampLifecycle("item_completed", "SubAgentActivity", "native-activity", "turn", "2026-10-09T08:02:00Z"), timestampComplete("turn"))
	path := historyTimestampFixture(t, home, "thread", records...)
	state, worker := filepath.Join(t.TempDir(), "state.db"), uuid.NewString()
	store, err := OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	queries := []rolloutTimestampQuery{
		{TurnID: "turn", ItemID: "message-0", Role: "user"},
		{TurnID: "turn", ItemID: "legacy-activity", Role: "activity"},
		{TurnID: "turn", ItemID: "native-activity", Role: "activity"},
	}
	seed := &rolloutStatsCache{store: store}
	seed.rolloutTimestampCandidates(home, path, "thread", queries)
	var scope string
	var legacy rolloutTimestampCheckpoint
	for key, cp := range seed.timestamps.entries {
		scope, legacy = key, *cp
	}
	if scope == "" || legacy.Offset != legacy.Size {
		t.Fatal("fixture did not produce a fully indexed durable EOF checkpoint")
	}
	legacy.Version = 1
	oldPrefix := scope + "/" + legacy.Generation + "/"
	// Model an installed v1 parser that reached EOF but never indexed either
	// legacy sub_agent_activity or the PascalCase native lifecycle record.
	if err := store.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketMeta)
		for _, query := range queries[1:] {
			for _, kind := range []string{"exact", "exact-end"} {
				prefix := []byte(oldPrefix + kind + "/" + rolloutTimestampHash(query.TurnID, query.Role, query.ItemID) + "/")
				cursor := bucket.Cursor()
				for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
					if err := cursor.Delete(); err != nil {
						return err
					}
				}
			}
		}
		value, err := json.Marshal(legacy)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(scope+"/checkpoint"), value)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	upgraded := &rolloutStatsCache{store: store}
	for i, result := range upgraded.rolloutTimestampCandidates(home, path, "thread", queries) {
		if !result.Found || result.Timestamp.Format("15:04") != fmt.Sprintf("08:%02d", i) {
			t.Fatalf("v1 EOF upgrade query %d: %+v", i, result)
		}
	}
	current := upgraded.timestamps.entries[scope]
	if current.Version != rolloutTimestampIndexVersion || current.Generation == legacy.Generation || current.Offset != current.Size {
		t.Fatalf("v1 EOF scan was resumed instead of replaced: %+v", current)
	}
	if upgraded.timestamps.bytesRead == 0 || upgraded.timestamps.bytesRead > rolloutTimestampScanBytes+rolloutTimestampTailBytes+2*statsLineBytes {
		t.Fatalf("upgrade read outside bounded work: %d", upgraded.timestamps.bytesRead)
	}
	countOld := func() int {
		t.Helper()
		count := 0
		if err := store.db.View(func(tx *bolt.Tx) error {
			cursor := tx.Bucket(bucketMeta).Cursor()
			for key, _ := cursor.Seek([]byte(oldPrefix)); key != nil && bytes.HasPrefix(key, []byte(oldPrefix)); key, _ = cursor.Next() {
				count++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if remaining := countOld(); remaining != 301-256 {
		t.Fatalf("legacy generation cleanup exceeded its bounded batch: %d remain", remaining)
	}
	generation := current.Generation
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(state, worker)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &rolloutStatsCache{store: store}
	for range 3 {
		for i, result := range restarted.rolloutTimestampCandidates(home, path, "thread", queries) {
			if !result.Found {
				t.Fatalf("upgrade restart lost query %d", i)
			}
		}
		restarted.timestamps.entries = nil // Exercise durable reload after eviction.
	}
	if restarted.timestamps.bytesRead != 0 {
		t.Fatalf("v2 restart repeated upgrade replay: %d bytes", restarted.timestamps.bytesRead)
	}
	if remaining := countOld(); remaining != 0 {
		t.Fatalf("unchanged lookup did not finish bounded cleanup: %d remain", remaining)
	}
	if err := store.db.View(func(tx *bolt.Tx) error {
		var saved rolloutTimestampCheckpoint
		if err := json.Unmarshal(tx.Bucket(bucketMeta).Get([]byte(scope+"/checkpoint")), &saved); err != nil {
			return err
		}
		if saved.Version != rolloutTimestampIndexVersion || saved.Generation != generation || len(saved.StaleGenerations) != 0 {
			t.Fatalf("upgrade did not retain durable v2 generation: %+v", saved)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
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
