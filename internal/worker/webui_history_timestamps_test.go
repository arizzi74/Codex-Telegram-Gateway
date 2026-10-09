package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func webUITimestampEvent(role, phase, timestamp, text string) string {
	payload := map[string]any{"type": "agent_message", "message": text}
	if role == "user" {
		payload["type"] = "user_message"
	} else if phase != "" {
		payload["phase"] = phase
	}
	raw, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": timestamp, "payload": payload})
	return string(raw)
}

func webUITimestampItem(id, role, phase, text string) map[string]any {
	item := map[string]any{"id": id, "type": "agentMessage", "text": text}
	if role == "user" {
		item = map[string]any{"id": id, "type": "userMessage", "content": []map[string]string{{"type": "text", "text": text}}}
	} else if phase != "" {
		item["phase"] = phase
	}
	return item
}

func webUITimestampAgent(t *testing.T, home, path string, items []map[string]any) (*Agent, protocol.Runtime, protocol.Session, *codextest.Server) {
	t.Helper()
	a, runtime, _, cleanup := testAgent(t, "private-secret")
	t.Cleanup(cleanup)
	client, server, err := codextest.New(a.ctx, codexadapter.InitializeInfo{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	a.manager.install(runtime, client)
	useWebUIHistoryFixture(a, client)
	session := installSession(a, runtime, "thread", "")
	turn := map[string]any{"id": "turn", "status": "completed", "startedAt": 1700000000, "completedAt": 1700000100, "items": items}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "path": path, "source": "cli", "turns": []map[string]any{turn}}}, nil)
	return a, runtime, session, server
}

func TestWebUIHistoryRecordedTimesSurviveRedactionPaginationAndRepeatedMessages(t *testing.T) {
	home := t.TempDir()
	base := time.Date(2026, 10, 9, 7, 52, 0, 0, time.UTC)
	items := make([]map[string]any, 0, 25)
	records := make([]string, 0, 60)
	want := make(map[string]int64)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("item-%d", 407+i)
		stamp := base.Add(time.Duration(i)*time.Second + 123*time.Millisecond)
		role, phase := "assistant", "commentary"
		text := fmt.Sprintf("private-secret repeated %d", i%3)
		if i == 0 {
			role, phase = "user", ""
		}
		if i == 24 {
			phase = "final_answer"
		}
		if i == 10 {
			// Projection into a display notice must preserve the fingerprint.
			text = strings.Repeat("private-secret ", 13000)
		}
		items = append(items, webUITimestampItem(id, role, phase, text))
		records = append(records, webUITimestampEvent(role, phase, stamp.Format(time.RFC3339Nano), text))
		// response_item mirrors are unrelated to the native reconstruction.
		records = append(records, timestampRecord("mirror-"+id, role, stamp.Add(time.Hour).Format(time.RFC3339Nano), text))
		want[id] = stamp.UnixMilli()
	}
	records = append(records, `{"type":"compacted","payload":{"message":"summary","replacement_history":[{"role":"assistant","content":[{"type":"output_text","text":"private-secret repeated 1"}]}]}}`)
	path := historyTimestampFixture(t, home, "thread", records...)
	a, runtime, session, server := webUITimestampAgent(t, home, path, items)
	request := webUIHistoryRequest{Limit: 20, Direction: "desc"}
	raw, err := a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	first := webUITestPage(t, raw)
	assertPage := func(page webUIHistoryPage, count int) {
		t.Helper()
		if len(page.Data) != count {
			t.Fatalf("page has %d entries, want %d", len(page.Data), count)
		}
		for _, entry := range page.Data {
			id := webUIItemID(entry.Item)
			if entry.RecordedAtMS == nil || *entry.RecordedAtMS != want[id] {
				t.Fatalf("%s recorded time = %v, want %d", id, entry.RecordedAtMS, want[id])
			}
			if entry.StartedAtMS != nil || entry.CompletedAtMS != nil || entry.TurnStartedAt == nil || *entry.TurnStartedAt != 1700000000 {
				t.Fatalf("%s acquired an invented lifecycle time: %+v", id, entry)
			}
			if strings.Contains(string(entry.Item), "private-secret") {
				t.Fatal("private text survived sanitization")
			}
		}
	}
	assertPage(first, 20)
	if first.NextCursor == "" || !strings.Contains(string(first.Data[14].Item), "displayNotice") {
		t.Fatal("missing bounded continuation or display projection")
	}
	request.Cursor = first.NextCursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	assertPage(webUITestPage(t, raw), 5)
	request.Cursor = ""
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	assertPage(webUITestPage(t, raw), 20)
	if fullWebUIHistoryReads(server) != 1 || countCall(server.Calls(), "thread/read") != 3 || countCall(server.Calls(), "thread/turns/list") != 3 {
		t.Fatalf("timestamp lookup reread source history: %+v", server.Calls())
	}
	a.webHistory.mu.Lock()
	defer a.webHistory.mu.Unlock()
	for _, snapshot := range a.webHistory.snapshots {
		for i, query := range snapshot.timestampQueries {
			if query.Digest == "" || len(query.Digest) != 64 || strings.Contains(string(snapshot.turn.Items[i]), "private-secret") {
				t.Fatal("snapshot lost its hash-only matching metadata")
			}
		}
	}
}

func TestWebUIHistoryRecordedTimesBackfillExistingSnapshotAfterPartialAppend(t *testing.T) {
	home := t.TempDir()
	base := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	var items []map[string]any
	var records []string
	for i := 0; i < 21; i++ {
		text := fmt.Sprintf("message %d", i)
		items = append(items, webUITimestampItem(fmt.Sprintf("item-%d", i+1), "assistant", "commentary", text))
		records = append(records, webUITimestampEvent("assistant", "commentary", base.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), text))
	}
	path := historyTimestampFixture(t, home, "thread", records...)
	data, err := os.ReadFile(path)
	if err != nil || os.WriteFile(path, data[:len(data)-1], 0o600) != nil {
		t.Fatal("could not create partial rollout fixture")
	}
	a, runtime, session, server := webUITimestampAgent(t, home, path, items)
	request := webUIHistoryRequest{Limit: 20, Direction: "desc"}
	raw, err := a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	first := webUITestPage(t, raw)
	for _, entry := range first.Data {
		if entry.RecordedAtMS != nil {
			t.Fatal("partial source turn borrowed a timestamp before cardinality validation")
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Revisit uses the same sanitized snapshot and its original fingerprint.
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range webUITestPage(t, raw).Data {
		if entry.RecordedAtMS == nil {
			t.Fatal("cached snapshot did not recover after index backfill")
		}
	}
	request.Cursor = first.NextCursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	if len(page.Data) != 1 || page.Data[0].RecordedAtMS == nil || *page.Data[0].RecordedAtMS != base.UnixMilli() || fullWebUIHistoryReads(server) != 1 {
		t.Fatal("continuation lost backfilled timestamp or reread the completed turn")
	}
}

func TestWebUIHistoryRecordedTimesStayWithActiveCursorSnapshot(t *testing.T) {
	home := t.TempDir()
	base := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	var items []map[string]any
	var records []string
	for i := 1; i <= 3; i++ {
		text := fmt.Sprintf("message %d", i)
		items = append(items, webUITimestampItem(fmt.Sprintf("item-%d", i), "assistant", "commentary", text))
		records = append(records, webUITimestampEvent("assistant", "commentary", base.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), text))
	}
	path := historyTimestampFixture(t, home, "thread", records...)
	a, runtime, session, server := webUITimestampAgent(t, home, path, items)
	setActive := func() {
		server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "path": path, "source": "cli", "turns": []map[string]any{{"id": "turn", "status": "inProgress", "items": items}}}}, nil)
	}
	setActive()
	request := webUIHistoryRequest{Limit: 1, Direction: "desc"}
	raw, err := a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Cursor = webUITestPage(t, raw).NextCursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	entry := webUITestPage(t, raw).Data[0]
	if webUIItemID(entry.Item) != "item-2" || entry.RecordedAtMS == nil {
		t.Fatal("active snapshot was not initially timestamped")
	}
	items = append(items, webUITimestampItem("item-4", "assistant", "commentary", "message 4"))
	setActive()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString(webUITimestampEvent("assistant", "commentary", base.Add(4*time.Second).Format(time.RFC3339Nano), "message 4") + "\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	retained := webUITestPage(t, raw).Data[0]
	if retained.RecordedAtMS == nil || *retained.RecordedAtMS != *entry.RecordedAtMS || webUIItemID(retained.Item) != "item-2" || fullWebUIHistoryReads(server) != 1 {
		t.Fatal("growing source changed a previously verified cursor snapshot time")
	}
	request.Cursor = ""
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	latest := webUITestPage(t, raw).Data[0]
	if webUIItemID(latest.Item) != "item-4" || latest.RecordedAtMS == nil || fullWebUIHistoryReads(server) != 2 {
		t.Fatal("active refresh did not recover the new source record")
	}
}

func TestWebUIHistoryRecordedMatchingDeclinesUnrelatedOrAmbiguousEvents(t *testing.T) {
	stamp := "2026-10-09T07:52:00Z"
	for _, tt := range []struct {
		name, thread string
		items        []map[string]any
		records      []string
	}{
		{"arbitrary ID", "thread", []map[string]any{webUITimestampItem("unsaved", "assistant", "commentary", "same")}, []string{webUITimestampEvent("assistant", "commentary", stamp, "same")}},
		{"wrong phase", "thread", []map[string]any{webUITimestampItem("item-1", "assistant", "commentary", "same")}, []string{webUITimestampEvent("assistant", "final_answer", stamp, "same")}},
		{"repeated cardinality", "thread", []map[string]any{webUITimestampItem("item-1", "assistant", "commentary", "same")}, []string{webUITimestampEvent("assistant", "commentary", stamp, "same"), webUITimestampEvent("assistant", "commentary", stamp, "same")}},
		{"wrong turn", "thread", []map[string]any{webUITimestampItem("item-1", "assistant", "commentary", "same")}, []string{timestampTurn("other"), webUITimestampEvent("assistant", "commentary", stamp, "same")}},
		{"wrong thread", "other", []map[string]any{webUITimestampItem("item-1", "assistant", "commentary", "same")}, []string{webUITimestampEvent("assistant", "commentary", stamp, "same")}},
		{"response mirror only", "thread", []map[string]any{webUITimestampItem("item-1", "assistant", "commentary", "same")}, []string{timestampRecord("mirror", "assistant", stamp, "same")}},
		{"changed message order", "thread", []map[string]any{webUITimestampItem("item-1", "assistant", "commentary", "first"), webUITimestampItem("item-2", "assistant", "commentary", "second")}, []string{webUITimestampEvent("assistant", "commentary", stamp, "second"), webUITimestampEvent("assistant", "commentary", stamp, "first")}},
		{"unsupported attachment projection", "thread", []map[string]any{{"id": "item-1", "type": "userMessage", "content": []map[string]string{{"type": "text", "text": "same"}, {"type": "image", "url": "attachment"}}}}, []string{webUITimestampEvent("user", "", stamp, "same"), timestampRecord("item-1", "user", stamp, "same")}},
		{"reversed reconstruction", "thread", []map[string]any{webUITimestampItem("item-2", "assistant", "commentary", "same"), webUITimestampItem("item-1", "assistant", "commentary", "same")}, []string{webUITimestampEvent("assistant", "commentary", stamp, "same"), webUITimestampEvent("assistant", "commentary", stamp, "same"), timestampRecord("item-1", "assistant", stamp, "same"), timestampRecord("item-2", "assistant", stamp, "same")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			path := historyTimestampFixture(t, home, tt.thread, tt.records...)
			a, runtime, session, _ := webUITimestampAgent(t, home, path, tt.items)
			raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "desc"})
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range webUITestPage(t, raw).Data {
				if entry.RecordedAtMS != nil || entry.TurnStartedAt == nil {
					t.Fatalf("unverified event replaced turn fallback: %+v", entry)
				}
			}
		})
	}
}

func TestWebUIHistoryRecordedToolTimesUseExactCallID(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		`{"type":"event_msg","timestamp":"2026-10-09T07:52:00.321Z","payload":{"type":"patch_apply_end","call_id":"exec-patch"}}`,
		`{"type":"response_item","timestamp":"2026-10-09T07:53:00.654Z","payload":{"type":"function_call","call_id":"exec-command","name":"exec_command","arguments":"{}"}}`)
	items := []map[string]any{
		{"id": "exec-patch", "type": "fileChange"},
		{"id": "exec-command", "type": "commandExecution"},
		{"id": "missing-call", "type": "commandExecution"},
	}
	a, runtime, session, _ := webUITimestampAgent(t, home, path, items)
	raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	for i, expected := range []string{"2026-10-09T07:52:00.321Z", "2026-10-09T07:53:00.654Z"} {
		stamp, _ := time.Parse(time.RFC3339Nano, expected)
		if page.Data[i].RecordedAtMS == nil || *page.Data[i].RecordedAtMS != stamp.UnixMilli() || page.Data[i].StartedAtMS != nil || page.Data[i].CompletedAtMS != nil {
			t.Fatalf("tool %d lost exact call timestamp: %+v", i, page.Data[i])
		}
	}
	if page.Data[2].RecordedAtMS != nil {
		t.Fatal("missing call ID acquired another tool timestamp")
	}
}

func TestWebUIHistoryRecordedCompactionMatchesTurnOccurrence(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "thread",
		webUITimestampEvent("assistant", "commentary", "2026-10-09T07:52:00Z", "same"),
		`{"type":"event_msg","timestamp":"2026-10-09T07:52:01.321Z","payload":{"type":"context_compacted"}}`,
		webUITimestampEvent("assistant", "final_answer", "2026-10-09T07:52:02Z", "same"))
	items := []map[string]any{
		webUITimestampItem("item-412", "assistant", "commentary", "same"),
		{"id": "item-413", "type": "contextCompaction"},
		webUITimestampItem("item-414", "assistant", "final_answer", "same"),
	}
	a, runtime, session, _ := webUITimestampAgent(t, home, path, items)
	raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	stamp, _ := time.Parse(time.RFC3339Nano, "2026-10-09T07:52:01.321Z")
	if len(page.Data) != 3 || page.Data[1].RecordedAtMS == nil || *page.Data[1].RecordedAtMS != stamp.UnixMilli() {
		t.Fatal("compaction borrowed another occurrence or lost its recorded time")
	}
}

func TestWebUIHistoryRecordedAtPreservedByNativeProjection(t *testing.T) {
	raw := json.RawMessage(`{"data":[{"turnId":"turn","recordedAtMs":1800000000123,"item":{"id":"item-1","type":"agentMessage","text":"hello"}}]}`)
	projected, err := webUIItemPage(raw)
	if err != nil {
		t.Fatal(err)
	}
	entry := webUITestPage(t, projected).Data[0]
	if entry.RecordedAtMS == nil || *entry.RecordedAtMS != 1800000000123 || entry.StartedAtMS != nil || entry.CompletedAtMS != nil {
		t.Fatalf("recorded provenance was changed: %+v", entry)
	}
}
