package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryPageProjectsBoundedTurnsWithOriginalTimesAndIncrementalCursors(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	for _, direction := range []string{"desc", "asc"} {
		type outcome struct {
			page HistoryTurnPage
			err  error
		}
		done := make(chan outcome, 1)
		go func() {
			page, err := client.ReadHistoryPage(context.Background(), "thread", "anchor", direction)
			done <- outcome{page, err}
		}()
		request := fake.next(t)
		fields := params(t, request)
		if method(t, request) != "thread/turns/list" || string(fields["limit"]) != "1" || string(fields["itemsView"]) != `"full"` || string(fields["cursor"]) != `"anchor"` || string(fields["sortDirection"]) != `"`+direction+`"` {
			t.Fatalf("unsafe or unbounded request: %v", fields)
		}
		fake.respond(t, request, json.RawMessage(`{"data":[{"id":"turn","status":"completed","startedAt":1774000000,"completedAt":1774000060,"itemsView":"full","items":[
			{"type":"userMessage","id":"prompt","content":[{"type":"text","text":"Review this "},{"type":"image","url":"data:image/png;base64,PRIVATEIMAGE"}]},
			{"type":"reasoning","id":"reasoning","summary":["PRIVATEREASONING"]},
			{"type":"commandExecution","id":"tool","aggregatedOutput":"PRIVATETOOL"},
			{"type":"agentMessage","id":"commentary","phase":"commentary","text":"PRIVATECOMMENTARY"},
			{"type":"agentMessage","id":"answer","phase":"final_answer","text":"Visible answer"}
		]}],"nextCursor":"next","backwardsCursor":"reverse"}`))
		got := <-done
		if got.err != nil || len(got.page.Turns) != 1 || got.page.NextCursor != "next" || got.page.BackwardsCursor != "reverse" {
			t.Fatalf("page = %#v, %v", got.page, got.err)
		}
		turn := got.page.Turns[0]
		if turn.Status != "completed" || len(turn.UserPrompts) != 1 || len(turn.Messages) != 2 || len(turn.QuestionEvents) != 1 || turn.UserPrompts[0].Text != "Review this [Image]" {
			t.Fatalf("projection = %#v", turn)
		}
		if !turn.StartedAt.Equal(time.Unix(1774000000, 0)) || !turn.CompletedAt.Equal(time.Unix(1774000060, 0)) || !turn.UserPrompts[0].Timestamp.Equal(*turn.StartedAt) || !turn.Messages[0].Timestamp.Equal(*turn.StartedAt) || !turn.Messages[1].Timestamp.Equal(*turn.CompletedAt) {
			t.Fatalf("original turn timestamps lost: %#v", turn)
		}
		encoded, _ := json.Marshal(got.page)
		if strings.Contains(string(encoded), "PRIVATE") {
			t.Fatalf("cache projection retained non-display payload: %s", encoded)
		}
	}
}

func TestHistoryPageRejectsIncompleteAndUnboundedResults(t *testing.T) {
	for _, response := range []string{
		`{}`,
		`{"data":[] ,"nextCursor":"next"}`,
		`{"data":[] ,"backwardsCursor":"reverse"}`,
		`{"data":[{"id":"turn","items":null}]}`,
		`{"data":[{"id":"turn","items":[],"itemsView":"summary"}]}`,
		`{"data":[{"id":"turn","items":[]},{"id":"other","items":[]}]}`,
		`{"data":[{"id":"turn","items":[]}],"nextCursor":"same"}`,
	} {
		client, fake := newFake(t)
		initialize(t, client, fake)
		done := make(chan error, 1)
		go func() { _, err := client.ReadHistoryPage(context.Background(), "thread", "same", "desc"); done <- err }()
		request := fake.next(t)
		fake.respond(t, request, json.RawMessage(response))
		if err := <-done; err == nil {
			t.Fatalf("accepted invalid page %s", response)
		}
	}
	client, _ := newFake(t)
	if _, err := client.ReadHistoryPage(context.Background(), "thread", "", "invalid"); err == nil || errors.Is(err, ErrNotInitialized) {
		t.Fatalf("invalid direction reached RPC: %v", err)
	}
}

func TestHistoryQuestionProjectionPreservesOrderingAndExistingState(t *testing.T) {
	turn, err := decodeHistoryTurn(json.RawMessage(`{"id":"turn","items":[
		{"type":"userMessage","id":"earlier","content":[{"type":"text","text":"> First?\n\nToo early"}]},
		{"type":"agentMessage","id":"q1","text":"Choose","delivery":"async","questions":[{"title":"First?"}]},
		{"type":"userMessage","id":"answer","content":[{"type":"text","text":"> First?\n\nYes"}]},
		{"type":"agentMessage","id":"q2","text":"Choose","delivery":"async","questions":[{"title":"Second?"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := ResolveHistoryQuestions(nil, []HistoryTurn{turn})
	if len(got) != 2 || !reflect.DeepEqual(got[0].AnsweredIDs, []string{"q1"}) || len(got[1].AnsweredIDs)+len(got[1].SupersededIDs) != 0 {
		t.Fatalf("question/input order lost: %#v", got)
	}
	before, _ := json.Marshal(got)
	again := ResolveHistoryQuestions(got, []HistoryTurn{turn})
	after, _ := json.Marshal(got)
	if string(before) != string(after) || !reflect.DeepEqual(again, got) {
		t.Fatalf("overlap changed prior state: before=%s after=%s result=%#v", before, after, again)
	}
	completed := ResolveHistoryQuestions(got, []HistoryTurn{{ID: "later", QuestionEvents: []HistoryQuestionEvent{{ItemID: "input", Text: "Continue normally", IsInput: true}}}})
	if !reflect.DeepEqual(completed[1].SupersededIDs, []string{"q1"}) || len(got[1].SupersededIDs) != 0 {
		t.Fatalf("later input did not supersede remaining fields safely: %#v", completed)
	}
}

func TestHistoryTimestampsNeverInventMissingDates(t *testing.T) {
	for _, stamp := range []string{"", `,"startedAt":null,"completedAt":null`, `,"startedAt":0,"completedAt":-1`, `,"startedAt":9223372036854775807`} {
		turn, err := decodeHistoryTurn(json.RawMessage(`{"id":"turn"` + stamp + `,"items":[{"id":"user","type":"userMessage","content":[{"type":"text","text":"Prompt"}]},{"id":"reply","type":"agentMessage","text":"Reply"}]}`))
		if err != nil || turn.StartedAt != nil || turn.CompletedAt != nil || turn.UserPrompts[0].Timestamp != nil || turn.Messages[0].Timestamp != nil || turn.Messages[1].Timestamp != nil {
			t.Fatalf("invented a date: %#v, %v", turn, err)
		}
	}
}

func TestActiveTurnTracksNotificationsAndThreadStatus(t *testing.T) {
	client, _ := newFake(t)
	client.track(newEvent("turn/started", json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}`)))
	if client.ActiveTurn("thread") != "turn" || client.ActiveTurn("other") != "" {
		t.Fatal("turn notification identity was not tracked")
	}
	event := newEvent("thread/status/changed", json.RawMessage(`{"threadId":"thread","status":{"type":"idle"}}`))
	if event.Kind != "thread_status_changed" || event.State != "idle" {
		t.Fatalf("status was not projected: %#v", event)
	}
	client.track(event)
	if client.ActiveTurn("thread") != "" {
		t.Fatal("idle status retained stale active turn")
	}
}
