package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
)

func historyTimestampFixture(t *testing.T, home, threadID string, records ...string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]string{"id": threadID}})
	if err := os.WriteFile(path, []byte(string(meta)+"\n"+timestampTurn("turn")+"\n"+strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func timestampTurn(id string) string {
	value, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]string{"type": "task_started", "turn_id": id}})
	return string(value)
}

func timestampRecord(id, role, timestamp, text string) string {
	idJSON, _ := json.Marshal(id)
	roleJSON, _ := json.Marshal(role)
	timeJSON, _ := json.Marshal(timestamp)
	textJSON, _ := json.Marshal(text)
	return `{"timestamp":` + string(timeJSON) + `,"type":"response_item","payload":{"type":"message","id":` + string(idJSON) + `,"role":` + string(roleJSON) + `,"content":[{"type":"input_text","text":` + string(textJSON) + `}]}}`
}

func TestHistoryTimestampsMatchIdentityInsteadOfRepeatedText(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampRecord("older", "user", "2026-09-20T08:00:00Z", "again"),
		timestampRecord("newer", "user", "2026-09-20T17:30:02.123+02:00", "again"),
		timestampRecord("answer", "assistant", "2026-09-20T15:31:00Z", "done"))
	turnTime := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	messages := []protocol.HistoryMessage{
		{TurnID: "turn", ItemID: "newer", Role: "user", Text: "again", Timestamp: &turnTime},
		{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "done", Timestamp: &turnTime},
		{TurnID: "turn", ItemID: "not-saved", Role: "user", Text: "again", Timestamp: &turnTime},
	}
	enrichRolloutHistoryTimestamps(home, path, "thread", messages)
	for i, expected := range []string{"2026-09-20T15:30:02.123Z", "2026-09-20T15:31:00Z"} {
		if messages[i].TimestampSource != "message" || messages[i].Timestamp.Format(time.RFC3339Nano) != expected {
			t.Fatalf("message %d timestamp = %+v", i, messages[i])
		}
	}
	if messages[2].TimestampSource != "" || !messages[2].Timestamp.Equal(turnTime) {
		t.Fatalf("unmatched message borrowed time: %+v", messages[2])
	}
}

func TestHistoryTimestampsDeclineAmbiguousAndWrongRoleIDs(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampRecord("duplicate", "user", "2026-09-20T08:00:00Z", "first"),
		timestampRecord("duplicate", "user", "2026-09-20T09:00:00Z", "second"),
		timestampRecord("wrong-role", "assistant", "2026-09-20T09:00:00Z", "reply"),
		timestampRecord("reused", "user", "2026-09-20T09:00:00Z", "same"))
	messages := []protocol.HistoryMessage{
		{TurnID: "turn", ItemID: "duplicate", Role: "user"},
		{TurnID: "turn", ItemID: "wrong-role", Role: "user"},
		{TurnID: "turn", ItemID: "reused", Role: "user"},
		{TurnID: "two", ItemID: "reused", Role: "user"},
	}
	enrichRolloutHistoryTimestamps(home, path, "thread", messages)
	for _, message := range messages {
		if message.Timestamp != nil || message.TimestampSource != "" {
			t.Fatalf("ambiguous timestamp: %+v", message)
		}
	}
}

func TestHistoryTimestampsRequireMatchingTurnContext(t *testing.T) {
	input := timestampRecord("unknown-turn", "user", "2026-09-20T08:00:00Z", "same") + "\n" +
		timestampTurn("new-turn") + "\n" + timestampRecord("reused", "user", "2026-09-20T09:00:00Z", "same") + "\n" +
		`{"type":"turn_context","payload":{"turn_id":"correct-turn"}}` + "\n" + timestampRecord("matching", "user", "2026-09-20T10:00:00Z", "same") + "\n"
	times := readHistoryMessageTimes(strings.NewReader(input), false, map[string]historyTimestampTarget{
		"unknown-turn": {"old-turn", "user"}, "reused": {"old-turn", "user"}, "matching": {"correct-turn", "user"},
	})
	if len(times) != 1 || times["matching"].IsZero() {
		t.Fatalf("cross-turn or unknown-turn timestamp accepted: %+v", times)
	}
}

func TestHistoryTimestampsClearContextAfterBrokenBoundary(t *testing.T) {
	for _, boundary := range []string{
		`{"type":"turn_context","payload": broken}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id": broken}}`,
	} {
		input := timestampTurn("old-turn") + "\n" + boundary + "\n" + timestampRecord("reused", "user", "2026-09-20T09:00:00Z", "same") + "\n"
		times := readHistoryMessageTimes(strings.NewReader(input), false, map[string]historyTimestampTarget{"reused": {"old-turn", "user"}})
		if len(times) != 0 {
			t.Fatalf("broken boundary retained old context: %+v", times)
		}
	}
}

func TestHistoryTimestampsReadLargeImagePrefixAndStayWithinTail(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		timestampRecord("old", "user", "2026-09-20T08:00:00Z", "old"),
		timestampRecord("huge", "user", "2026-09-20T09:00:00Z", strings.Repeat("x", historyTimestampTailBytes+1024)),
		timestampTurn("turn"),
		timestampRecord("image", "user", "2026-09-20T10:00:00Z", strings.Repeat("i", statsLineBytes+1024)),
		timestampRecord("answer", "assistant", "2026-09-20T11:00:00Z", "done"))
	messages := []protocol.HistoryMessage{
		{TurnID: "turn", ItemID: "old", Role: "user"}, {TurnID: "turn", ItemID: "huge", Role: "user"},
		{TurnID: "turn", ItemID: "image", Role: "user"}, {TurnID: "turn", ItemID: "answer", Role: "assistant"},
	}
	enrichRolloutHistoryTimestamps(home, path, "thread", messages)
	for i := range messages {
		if (messages[i].TimestampSource == "message") != (i >= 2) {
			t.Fatalf("tail bound/image handling, message %d: %+v", i, messages[i])
		}
	}
}

func TestHistoryTimestampsRejectInvalidUnfinishedOrUnrelatedRecords(t *testing.T) {
	input := strings.Join([]string{timestampTurn("turn"),
		timestampRecord("invalid-date", "user", "2026-09-70T00:00:00Z", "bad"),
		`{"timestamp":"2026-09-20T08:00:00Z","type":"response_item","payload":{"type":"message","id":"broken","role":"user",bad}}`,
		`{"timestamp":"2026-09-20T08:00:00Z","type":"event_msg","payload":{"type":"message","id":"event","role":"user"}}`,
		`{"payload":{"id":"reordered","role":"assistant","type":"message"},"timestamp":"2026-09-20T08:00:00Z","type":"response_item"}`,
	}, "\n") + "\n" + timestampRecord("unfinished", "user", "2026-09-20T08:00:00Z", "complete JSON, no saved newline")
	times := readHistoryMessageTimes(strings.NewReader(input), false, map[string]historyTimestampTarget{"invalid-date": {"turn", "user"}, "broken": {"turn", "user"}, "event": {"turn", "user"}, "reordered": {"turn", "assistant"}, "unfinished": {"turn", "user"}})
	if len(times) != 1 || times["reordered"].IsZero() {
		t.Fatalf("invalid record accepted or valid order rejected: %+v", times)
	}
}

func TestHistoryTimestampsValidateThreadPathAndSymlinks(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	path := historyTimestampFixture(t, home, "actual-thread", timestampRecord("message", "user", "2026-09-20T08:00:00Z", "text"))
	outsidePath := historyTimestampFixture(t, outside, "actual-thread", timestampRecord("message", "user", "2026-09-20T08:00:00Z", "text"))
	symlink := filepath.Join(home, "sessions", "outside.jsonl")
	if err := os.Symlink(outsidePath, symlink); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ name, path, thread string }{
		{"wrong thread", path, "different-thread"}, {"outside path", outsidePath, "actual-thread"}, {"symlink escape", symlink, "actual-thread"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			messages := []protocol.HistoryMessage{{TurnID: "turn", ItemID: "message", Role: "user"}}
			enrichRolloutHistoryTimestamps(home, tt.path, tt.thread, messages)
			if messages[0].Timestamp != nil {
				t.Fatalf("unrelated timestamp exposed: %+v", messages)
			}
		})
	}
}

func TestHistoryRolloutPathUsesValidatedDiscoveryCache(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread", timestampRecord("message", "user", "2026-09-20T08:00:00Z", "text"))
	cache := &rolloutStatsCache{}
	if cache.historyRolloutPath(home, "thread") != "" {
		t.Fatal("empty cache returned a path")
	}
	if cache.read(home, path, "thread", "", nil) == nil {
		t.Fatal("could not read stats")
	}
	if got := cache.historyRolloutPath(home, "thread"); got != path {
		t.Fatalf("path = %q", got)
	}
	if cache.historyRolloutPath(home, "other") != "" || cache.historyRolloutPath(t.TempDir(), "thread") != "" {
		t.Fatal("cross-thread/home cache match")
	}
}
