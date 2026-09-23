package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

type webUIFixtureHistorySource struct{ client *codexadapter.Client }

func (s webUIFixtureHistorySource) ReadPage(ctx context.Context, thread, cursor, direction string) (codexadapter.TranscriptTurnPage, error) {
	return s.client.ReadTranscriptTurnPage(ctx, thread, cursor, direction, true)
}
func (s webUIFixtureHistorySource) Close() error { return nil }
func useWebUIHistoryFixture(a *Agent, client *codexadapter.Client) {
	a.historySource = func(context.Context, string) (webUIHistorySource, error) {
		return webUIFixtureHistorySource{client}, nil
	}
}

func webUITestTurn(id, status string, count int) map[string]any {
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		items = append(items, map[string]any{"id": fmt.Sprintf("%s-%03d", id, i), "type": "agentMessage", "text": fmt.Sprintf("message %d", i)})
	}
	return map[string]any{"id": id, "status": status, "startedAt": 1700000000, "completedAt": 1700000100, "items": items}
}
func webUITestPage(t *testing.T, raw json.RawMessage) webUIHistoryPage {
	t.Helper()
	var page webUIHistoryPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	return page
}
func webUIItemID(raw json.RawMessage) string {
	var item struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &item)
	return item.ID
}
func fullWebUIHistoryReads(server *codextest.Server) int {
	n := 0
	for _, call := range server.Calls() {
		if call.Method == "thread/turns/list" {
			var params struct {
				ItemsView string `json:"itemsView"`
				Limit     int    `json:"limit"`
			}
			_ = json.Unmarshal(call.Params, &params)
			if params.ItemsView == "full" {
				n++
			}
		}
	}
	return n
}

func TestWebUIHistoryFallbackCountsItemsAndCachesCompletedTurns(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	useWebUIHistoryFixture(a, mustClient(t, a, runtime))
	session := installSession(a, runtime, "bounded-thread", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "source": "cli", "turns": []map[string]any{webUITestTurn("old", "completed", 5), webUITestTurn("latest", "completed", 55)}}}, nil)
	request := webUIHistoryRequest{Limit: 20, Direction: "desc"}
	raw, err := a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	if len(page.Data) != 20 || webUIItemID(page.Data[0].Item) != "latest-054" || webUIItemID(page.Data[19].Item) != "latest-035" || page.NextCursor == "" || fullWebUIHistoryReads(server) != 1 {
		t.Fatalf("first bounded page = %#v, full reads=%d", page, fullWebUIHistoryReads(server))
	}
	if page.Data[0].TurnStartedAt == nil || *page.Data[0].TurnStartedAt != 1700000000 || page.Data[0].TurnStatus != "completed" {
		t.Fatal("source dates/status missing")
	}
	// A new connection/page revisit refreshes metadata but not completed items.
	if _, err := a.webUIHistory(t.Context(), runtime, session, request); err != nil {
		t.Fatal(err)
	}
	if fullWebUIHistoryReads(server) != 1 {
		t.Fatal("completed turn was reread")
	}
	request.Cursor = page.NextCursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	page = webUITestPage(t, raw)
	if len(page.Data) != 20 || webUIItemID(page.Data[0].Item) != "latest-034" || fullWebUIHistoryReads(server) != 1 {
		t.Fatal("same-turn continuation reread or reordered history")
	}
	request.Cursor = page.NextCursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	page = webUITestPage(t, raw)
	if len(page.Data) != 20 || webUIItemID(page.Data[0].Item) != "latest-014" || webUIItemID(page.Data[19].Item) != "old-000" || page.NextCursor != "" || fullWebUIHistoryReads(server) != 2 {
		t.Fatalf("cross-turn suffix=%s", raw)
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/turns/list" {
			var params map[string]any
			_ = json.Unmarshal(call.Params, &params)
			if params["limit"] != float64(1) {
				t.Fatal("read more than one source turn")
			}
		}
	}
}

func TestWebUIHistoryCursorsScopeExpireAndRejectWorkspaceChanges(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	useWebUIHistoryFixture(a, mustClient(t, a, runtime))
	session := installSession(a, runtime, "scope-a", "")
	other := installSession(a, runtime, "scope-b", "")
	setThreads := func(cwd string) {
		server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": cwd, "source": "cli", "turns": []map[string]any{webUITestTurn("turn", "completed", 21)}}, {"id": other.ThreadID, "cwd": other.CWD, "source": "cli"}}, nil)
	}
	setThreads(session.CWD)
	raw, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	cursor := webUITestPage(t, raw).NextCursor
	request := webUIHistoryRequest{Limit: 20, Direction: "desc", Cursor: cursor}
	if _, err := a.webUIHistory(t.Context(), runtime, other, request); err == nil {
		t.Fatal("cross-thread cursor accepted")
	}
	stale := runtime
	stale.Generation++
	if _, err := a.webUIHistory(t.Context(), stale, session, request); err == nil {
		t.Fatal("stale-generation cursor accepted")
	}
	if _, err := a.webUIHistory(t.Context(), runtime, session, webUIHistoryRequest{Limit: 20, Direction: "asc", Cursor: cursor}); err == nil {
		t.Fatal("cursor direction changed")
	}
	setThreads(t.TempDir())
	if _, err := a.webUIHistory(t.Context(), runtime, session, request); err == nil {
		t.Fatal("cached data bypassed current workspace boundary")
	}
	setThreads(session.CWD)
	a.webHistory.mu.Lock()
	for _, snapshot := range a.webHistory.snapshots {
		snapshot.created = time.Now().Add(-webUIHistoryTTL - time.Second)
	}
	a.webHistory.mu.Unlock()
	if _, err := a.webUIHistory(t.Context(), runtime, session, request); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired cursor=%v", err)
	}
}

func TestWebUIHistoryActiveRefreshKeepsOldCursorSnapshot(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	useWebUIHistoryFixture(a, mustClient(t, a, runtime))
	session := installSession(a, runtime, "active-thread", "")
	setTurn := func(count int) {
		server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "source": "cli", "turns": []map[string]any{webUITestTurn("active", "inProgress", count)}}}, nil)
	}
	request := webUIHistoryRequest{Limit: 20, Direction: "desc"}
	setTurn(25)
	raw, err := a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	cursor := webUITestPage(t, raw).NextCursor
	setTurn(30)
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	if webUIItemID(webUITestPage(t, raw).Data[0].Item) != "active-029" || fullWebUIHistoryReads(server) != 2 {
		t.Fatal("active head was cached stale")
	}
	request.Cursor = cursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	if len(page.Data) != 5 || webUIItemID(page.Data[0].Item) != "active-004" || fullWebUIHistoryReads(server) != 2 {
		t.Fatal("old snapshot shifted or reread")
	}
}

func TestWebUIHistoryEmptyTraversalAndDisplayAreBounded(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t, `private-secret`)
	defer cleanup()
	useWebUIHistoryFixture(a, mustClient(t, a, runtime))
	session := installSession(a, runtime, "empty-thread", "")
	turns := []map[string]any{webUITestTurn("old", "completed", 1)}
	for i := 0; i < 40; i++ {
		turns = append(turns, webUITestTurn(fmt.Sprint(i), "completed", 0))
	}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "source": "cli", "turns": turns}}, nil)
	request := webUIHistoryRequest{Limit: 20, Direction: "desc"}
	raw, err := a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	page := webUITestPage(t, raw)
	if len(page.Data) != 0 || page.NextCursor == "" || fullWebUIHistoryReads(server) != webUIHistoryTraversal {
		t.Fatalf("unbounded empty traversal: %s, reads=%d", raw, fullWebUIHistoryReads(server))
	}
	request.Cursor = page.NextCursor
	raw, err = a.webUIHistory(t.Context(), runtime, session, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(webUITestPage(t, raw).Data) != 1 {
		t.Fatal("empty continuation lost old message")
	}
	big, _ := json.Marshal(map[string]any{"id": "big", "type": "commandExecution", "aggregatedOutput": strings.Repeat("private-secret ", 30000)})
	reasoning := json.RawMessage(`{"id":"reason","type":"reasoning","summary":["public private-secret"],"content":["RAW_REASONING"]}`)
	items, err := webUISanitizeHistoryItems(a.redactor, []json.RawMessage{big, reasoning})
	if err != nil {
		t.Fatal(err)
	}
	if len(items[0]) > webUIHistoryItemBytes || strings.Contains(string(items[0]), "private-secret") || strings.Contains(string(items[1]), "RAW_REASONING") || strings.Contains(string(items[1]), "private-secret") || !strings.Contains(string(items[1]), "public [REDACTED]") {
		t.Fatalf("invalid cache projection: %s %s", items[0], items[1])
	}
}

func TestWebUIHistoryFallbackOnlyForUnsupportedMethod(t *testing.T) {
	for _, code := range []int{-32601, -32602, -32000} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := testWebUIRelay()
			r.resumed = true
			r.ctx = t.Context()
			r.pool = &webUIRelays{c: &Connection{}}
			called := 0
			r.pool.c.historyWebUI = func(_ context.Context, _ protocol.Runtime, _ protocol.Session, _ webUIHistoryRequest) (json.RawMessage, error) {
				called++
				return json.RawMessage(`{"data":[]}`), nil
			}
			if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/items/list","params":{}}`)); err != nil {
				t.Fatal(err)
			}
			response := fmt.Sprintf(`{"id":1,"error":{"code":%d,"message":"native error"}}`, code)
			if _, err := r.serverMessage([]byte(response)); err != nil {
				t.Fatal(err)
			}
			if (called == 1) != (code == -32601) || r.historyFallback != (code == -32601) {
				t.Fatal("incorrect fallback capability detection")
			}
		})
	}
}

func TestWebUIHistoryFallbackQueueIsBoundedAndPreservesReader(t *testing.T) {
	r := testWebUIRelay()
	r.resumed = true
	r.ctx = t.Context()
	r.historyRequests = make(chan webUIRPC, 1)
	r.pool = &webUIRelays{c: &Connection{}}
	r.pool.c.historyWebUI = func(context.Context, protocol.Runtime, protocol.Session, webUIHistoryRequest) (json.RawMessage, error) {
		t.Fatal("native reader performed a synchronous history read")
		return nil, nil
	}
	for id := 1; id <= 2; id++ {
		if _, err := r.clientMessage([]byte(fmt.Sprintf(`{"id":%d,"method":"thread/items/list","params":{}}`, id))); err != nil {
			t.Fatal(err)
		}
		data, err := r.serverMessage([]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-32601,"message":"unavailable"}}`, id)))
		if id == 1 && (err != nil || len(data) != 0 || len(r.historyRequests) != 1 || r.pending["n:1"] == "") {
			t.Fatalf("fallback was not reserved and queued: %s %v", data, err)
		}
		if id == 2 && err == nil {
			t.Fatal("unbounded fallback queue accepted another request")
		}
	}
	data, err := r.serverMessage([]byte(`{"method":"thread/status/changed","params":{"threadId":"thread","status":{"type":"idle"}}}`))
	if err != nil || len(data) == 0 {
		t.Fatalf("pending history prevented native event delivery: %s %v", data, err)
	}
}
