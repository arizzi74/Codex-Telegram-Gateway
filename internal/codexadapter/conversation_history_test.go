package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestConversationMessagesReadsNewestSuffixAndPagesChronologically(t *testing.T) {
	newest := `{"id":"turn-new","itemsView":"full","items":[
		{"id":"user-new","type":"userMessage","content":[{"type":"text","text":"Telegram prompt"}]},
		{"id":"progress","type":"agentMessage","phase":"commentary","text":"temporary progress"},
		{"id":"answer-new","type":"agentMessage","phase":"final_answer","text":"Latest answer"}
	]}`
	oldest := `{"id":"turn-old","items":[
		{"id":"user-old","type":"userMessage","content":[{"type":"text","text":"CLI prompt"}]},
		{"id":"answer-old","type":"agentMessage","text":"Older answer"}
	]}`
	for _, tc := range []struct {
		name   string
		limit  int
		before *protocol.HistoryCursor
		pages  []string
		want   []protocol.HistoryMessage
		older  bool
	}{
		{"last reply stops early", 1, nil, []string{`{"data":[` + newest + `],"nextCursor":"older"}`},
			[]protocol.HistoryMessage{{TurnID: "turn-new", ItemID: "answer-new", Role: "assistant", Text: "Latest answer"}}, true},
		{"both roles", 3, nil, []string{`{"data":[` + newest + `],"nextCursor":"older"}`, `{"data":[` + oldest + `]}`},
			[]protocol.HistoryMessage{{TurnID: "turn-old", ItemID: "answer-old", Role: "assistant", Text: "Older answer"}, {TurnID: "turn-new", ItemID: "user-new", Role: "user", Text: "Telegram prompt"}, {TurnID: "turn-new", ItemID: "answer-new", Role: "assistant", Text: "Latest answer"}}, true},
		{"exclusive older cursor", 2, &protocol.HistoryCursor{TurnID: "turn-new", ItemID: "user-new"}, []string{`{"data":[` + newest + `],"nextCursor":"older"}`, `{"data":[` + oldest + `]}`},
			[]protocol.HistoryMessage{{TurnID: "turn-old", ItemID: "user-old", Role: "user", Text: "CLI prompt"}, {TurnID: "turn-old", ItemID: "answer-old", Role: "assistant", Text: "Older answer"}}, false},
		{"empty", 1, nil, []string{`{"data":[]}`}, []protocol.HistoryMessage{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			type outcome struct {
				messages []protocol.HistoryMessage
				older    bool
				err      error
			}
			done := make(chan outcome, 1)
			go func() {
				messages, older, err := client.ConversationMessages(context.Background(), "target", tc.limit, tc.before)
				done <- outcome{messages, older, err}
			}()
			for i, page := range tc.pages {
				request := fake.next(t)
				fields := params(t, request)
				if method(t, request) != "thread/turns/list" || string(fields["sortDirection"]) != `"desc"` || string(fields["itemsView"]) != `"full"` || string(fields["limit"]) != "1" || string(fields["threadId"]) != `"target"` {
					t.Fatalf("unexpected/mutating history request: %#v", request)
				}
				if (i == 0 && len(fields) != 4) || (i > 0 && string(fields["cursor"]) != `"older"`) {
					t.Fatalf("lost cursor: %v", fields)
				}
				fake.respond(t, request, json.RawMessage(page))
			}
			got := <-done
			if got.err != nil || got.older != tc.older || !reflect.DeepEqual(got.messages, tc.want) {
				t.Fatalf("messages=%#v older=%v err=%v", got.messages, got.older, got.err)
			}
			_ = client.Close()
			if fake.scan.Scan() {
				t.Fatalf("unexpected extra RPC: %s", fake.scan.Text())
			}
		})
	}
}

func TestConversationMessagesProjectsVisibleTextOnly(t *testing.T) {
	messages, err := decodeConversationTurn(json.RawMessage(`{"id":"turn","items":[
		{"type":"developerMessage","content":[{"type":"text","text":"private"}]},
		{"type":"userMessage","id":"user","content":[{"type":"text","text":"Image: "},{"type":"image","url":"data:secret"}]},
		{"type":"reasoning","id":"reason","text":"private reasoning"},
		{"type":"commandExecution","id":"command","aggregatedOutput":"private tool output"},
		{"type":"agentMessage","id":"commentary","phase":"commentary","text":"temporary progress"},
		{"type":"agentMessage","id":"future","phase":"future_phase","text":"unknown phase"},
		{"type":"agentMessage","id":"final","phase":"final_answer","text":"Visible answer"}
	]}`))
	want := []protocol.HistoryMessage{{TurnID: "turn", ItemID: "user", Role: "user", Text: "Image: [Image]"}, {TurnID: "turn", ItemID: "final", Role: "assistant", Text: "Visible answer"}}
	if err != nil || !reflect.DeepEqual(messages, want) {
		t.Fatalf("visible history = %#v, %v", messages, err)
	}
}

func TestConversationMessagesRejectsMissingCursorAndBrokenPagination(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pages  []string
		before *protocol.HistoryCursor
		max    int
		want   error
	}{
		{"missing data", []string{`{}`}, nil, 3, ErrHistoryUnavailable},
		{"summary", []string{`{"data":[{"id":"a","itemsView":"summary","items":[]}]}`}, nil, 3, ErrHistoryUnavailable},
		{"duplicate turn", []string{`{"data":[{"id":"a","items":[]}],"nextCursor":"older"}`, `{"data":[{"id":"a","items":[]}]}`}, nil, 3, ErrHistoryUnavailable},
		{"duplicate cursor", []string{`{"data":[{"id":"a","items":[]}],"nextCursor":"older"}`, `{"data":[{"id":"b","items":[]}],"nextCursor":"older"}`}, nil, 3, ErrHistoryUnavailable},
		{"page cap", []string{`{"data":[{"id":"a","items":[]}],"nextCursor":"older"}`}, nil, 1, ErrHistoryUnavailable},
		{"missing cursor", []string{`{"data":[]}`}, &protocol.HistoryCursor{TurnID: "gone", ItemID: "gone"}, 3, ErrHistoryCursorUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() {
				_, _, err := client.conversationMessages(context.Background(), "target", 2, tc.before, tc.max)
				done <- err
			}()
			for _, page := range tc.pages {
				request := fake.next(t)
				fake.respond(t, request, json.RawMessage(page))
			}
			if err := <-done; !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
