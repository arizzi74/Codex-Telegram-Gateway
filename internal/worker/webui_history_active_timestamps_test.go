package worker

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestWebUIHistoryRecordedTimesRecoverUnvisitedActiveSnapshotAfterAppend(t *testing.T) {
	for _, tt := range []struct {
		name        string
		appended    string
		partialTail bool
	}{
		{name: "new message", appended: "new progress"},
		{name: "repeated message", appended: "repeated progress"},
		{name: "partial backfill and repeated message", appended: "repeated progress", partialTail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			base := time.Date(2026, 10, 9, 8, 0, 0, 123000000, time.UTC)
			items := []map[string]any{
				webUITimestampItem("item-1", "assistant", "commentary", "repeated progress"),
				webUITimestampItem("item-2", "assistant", "commentary", "repeated progress"),
				{"id": "saved-tool", "type": "commandExecution", "status": "completed"},
			}
			firstRecord := webUITimestampEvent("assistant", "commentary", base.Format(time.RFC3339Nano), "repeated progress")
			secondRecord := webUITimestampEvent("assistant", "commentary", base.Add(2*time.Second).Format(time.RFC3339Nano), "repeated progress")
			toolRecord, err := json.Marshal(map[string]any{
				"type": "response_item", "timestamp": base.Add(4 * time.Second).Format(time.RFC3339Nano),
				"payload": map[string]any{"type": "function_call", "call_id": "saved-tool", "name": "exec_command", "arguments": "{}"},
			})
			if err != nil {
				t.Fatal(err)
			}
			path := historyTimestampFixture(t, home, "thread", firstRecord, secondRecord, string(toolRecord))
			if tt.partialTail {
				// The native snapshot can lead the rollout writer. Its second
				// message is complete JSON but has not been saved with a newline.
				path = historyTimestampFixture(t, home, "thread", firstRecord, secondRecord)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data[:len(data)-1], 0o600); err != nil {
					t.Fatal(err)
				}
			}
			a, runtime, session, server := webUITimestampAgent(t, home, path, items)
			setActive := func() {
				server.SetThreads([]map[string]any{{
					"id": session.ThreadID, "cwd": session.CWD, "path": path, "source": "cli",
					"turns": []map[string]any{{"id": "turn", "status": "inProgress", "startedAt": 1700000000, "items": items}},
				}}, nil)
			}
			setActive()
			readPage := func(cursor string) webUIHistoryPage {
				t.Helper()
				raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 1, Direction: "desc", Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				page := webUITestPage(t, raw)
				if len(page.Data) != 1 {
					t.Fatalf("page has %d entries, want 1", len(page.Data))
				}
				return page
			}
			assertRecorded := func(page webUIHistoryPage, id string, stamp time.Time) {
				t.Helper()
				entry := page.Data[0]
				if got := webUIItemID(entry.Item); got != id {
					t.Fatalf("page item = %q, want %q", got, id)
				}
				if entry.RecordedAtMS == nil || *entry.RecordedAtMS != stamp.UnixMilli() {
					t.Fatalf("%s recorded time = %v, want %d", id, entry.RecordedAtMS, stamp.UnixMilli())
				}
				if entry.StartedAtMS != nil || entry.CompletedAtMS != nil || entry.TurnStartedAt == nil || *entry.TurnStartedAt != 1700000000 {
					t.Fatalf("%s changed lifecycle or turn dates: %+v", id, entry)
				}
			}
			first := readPage("")
			if webUIItemID(first.Data[0].Item) != "saved-tool" || first.NextCursor == "" || fullWebUIHistoryReads(server) != 1 {
				t.Fatal("initial page did not leave both assistant occurrences unvisited in one active snapshot")
			}
			if tt.partialTail {
				if first.Data[0].RecordedAtMS != nil {
					t.Fatal("unsaved tool acquired a recorded time")
				}
			} else {
				assertRecorded(first, "saved-tool", base.Add(4*time.Second))
			}
			// Grow both sources before the old cursor has visited an assistant.
			// A duplicate append must not change either old occurrence's date.
			appendedStamp := base.Add(6 * time.Second)
			appendData := webUITimestampEvent("assistant", "commentary", appendedStamp.Format(time.RFC3339Nano), tt.appended) + "\n"
			if tt.partialTail {
				appendData = "\n" + string(toolRecord) + "\n" + appendData
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString(appendData)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("append rollout: write %v; close %v", writeErr, closeErr)
			}
			items = append(items, webUITimestampItem("item-3", "assistant", "commentary", tt.appended))
			setActive()
			older := readPage(first.NextCursor)
			assertRecorded(older, "item-2", base.Add(2*time.Second))
			if older.NextCursor == "" {
				t.Fatal("older snapshot lost its first assistant occurrence")
			}
			oldest := readPage(older.NextCursor)
			assertRecorded(oldest, "item-1", base)
			if oldest.NextCursor != "" || fullWebUIHistoryReads(server) != 1 {
				t.Fatalf("old snapshot reread native history or included the appended item: full reads=%d", fullWebUIHistoryReads(server))
			}
			// A fresh head refreshes the active native turn and uses its grown
			// sequence while retaining distinct dates for all repeated bodies.
			fresh := readPage("")
			assertRecorded(fresh, "item-3", appendedStamp)
			for i, expected := range []struct {
				id    string
				stamp time.Time
			}{
				{"saved-tool", base.Add(4 * time.Second)},
				{"item-2", base.Add(2 * time.Second)},
				{"item-1", base},
			} {
				if fresh.NextCursor == "" {
					t.Fatalf("fresh sequence ended before entry %d", i)
				}
				fresh = readPage(fresh.NextCursor)
				assertRecorded(fresh, expected.id, expected.stamp)
			}
			if fresh.NextCursor != "" || fullWebUIHistoryReads(server) != 2 {
				t.Fatalf("fresh snapshot read count = %d, want 2 total native full reads", fullWebUIHistoryReads(server))
			}
			if got := countCall(server.Calls(), "thread/read"); got != 7 {
				t.Fatalf("lightweight authorization reads = %d, want one per page (7)", got)
			}
		})
	}
}
