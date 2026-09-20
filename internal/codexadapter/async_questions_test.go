package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAsyncQuestionEventSurvivesFinalAnswerPhase(t *testing.T) {
	raw := json.RawMessage(`{"threadId":"thread","turnId":"turn","item":{"id":"call-question","type":"agentMessage","phase":"final_answer","delivery":"async","text":"Which hardware?","questions":[{"title":"Which hardware?","options":["Mac","Windows"]},{"title":"Any details?"}]}}`)
	event := newEvent("item/completed", raw)
	want := []Question{{ID: "q1", Prompt: "Which hardware?", IsOther: true, Choices: []Choice{{Label: "Mac"}, {Label: "Windows"}}}, {ID: "q2", Prompt: "Any details?", IsOther: true}}
	if event.Kind != "input_requested_async" || !event.Async || event.ItemID != "call-question" || event.ThreadID != "thread" || event.TurnID != "turn" || event.Text != "Which hardware?" || !reflect.DeepEqual(event.Questions, want) {
		t.Fatalf("async question was not independently projected: %#v", event)
	}
	if started := newEvent("item/started", raw); started.Kind == "input_requested_async" || len(started.Questions) != 0 {
		t.Fatalf("incomplete question was offered: %#v", started)
	}
	for _, raw := range []string{
		`{"item":{"type":"agentMessage","text":"Ordinary question?","phase":"final_answer","questions":[{"title":"Unmarked question?"}]}}`,
		`{"item":{"type":"reasoning","delivery":"async","questions":[{"title":"Private question?"}]}}`,
	} {
		if event := newEvent("item/completed", json.RawMessage(raw)); event.Async || len(event.Questions) != 0 {
			t.Fatalf("non-async input was classified as question: %#v", event)
		}
	}
}

func TestAsyncQuestionAnswerUsesNativeFramingAndBoundaries(t *testing.T) {
	if answer := FormatAsyncQuestionAnswer("Which\r\nhardware?", "  Mac  "); answer != "> Which  hardware?\n\nMac" {
		t.Fatalf("native framing = %q", answer)
	}
	title := strings.Repeat("a", 511) + "€tail"
	answer := FormatAsyncQuestionAnswer(title, "Windows")
	if answer != "> "+strings.Repeat("a", 511)+"\n\nWindows" || !utf8.ValidString(answer) || !AsyncQuestionAnswerMatches(title, answer) {
		t.Fatalf("UTF-8 title truncation = %q", answer)
	}
	bundle := FormatAsyncQuestionAnswer("Hardware?", "Mac") + "\n\n" + FormatAsyncQuestionAnswer("Details?", "Second display")
	if !AsyncQuestionAnswerMatches("Hardware?", bundle) || !AsyncQuestionAnswerMatches("Details?", bundle) {
		t.Fatal("bundled native answers were not recognized")
	}
	for _, candidate := range []string{"Mac", "> Different question?\n\nMac", "ordinary prefix > Hardware?\n\nMac", "> Hardware?\n\n  ", "> Hardware?\n\n\n\n> Details?\n\nOther answer"} {
		if AsyncQuestionAnswerMatches("Hardware?", candidate) {
			t.Fatalf("unrelated/empty input was treated as an answer: %q", candidate)
		}
	}
}

func TestAsyncQuestionHistoryRecordsOnlySubsequentMatchingAnswers(t *testing.T) {
	questions := []AsyncQuestion{
		{TurnID: "earlier", ItemID: "old", Questions: []Question{{ID: "q1", Prompt: "Hardware?"}, {ID: "q2", Prompt: "Details?"}}},
		{TurnID: "current", ItemID: "later", Questions: []Question{{ID: "q1", Prompt: "Hardware?"}}},
	}
	raw := json.RawMessage(`{"id":"current","items":[
		{"type":"userMessage","id":"unrelated","content":[{"type":"text","text":"Continue working"}]},
		{"type":"userMessage","id":"answer","content":[{"type":"text","text":"> Hardware?\n\nMac"}]},
		{"type":"agentMessage","id":"later","delivery":"async","questions":[{"title":"Hardware?"}]}
	]}`)
	got, err := resolveAsyncQuestionHistory(questions, raw)
	if err != nil || len(got) != 2 || !reflect.DeepEqual(got[0].AnsweredIDs, []string{"q1"}) || len(got[0].Questions) != 2 || len(got[1].AnsweredIDs) != 0 {
		t.Fatalf("history discarded questions or resolved a later question: %#v, %v", got, err)
	}
	raw = json.RawMessage(`{"id":"next","items":[{"type":"userMessage","id":"answer","content":[{"type":"text","text":"> Hardware?\n\nWindows\n\n> Details?\n\nSecond display"}]}]}`)
	got, err = resolveAsyncQuestionHistory(got, raw)
	if err != nil || !reflect.DeepEqual(got[0].AnsweredIDs, []string{"q1", "q2"}) || !reflect.DeepEqual(got[1].AnsweredIDs, []string{"q1"}) {
		t.Fatalf("later bundled answers were lost or duplicated: %#v, %v", got, err)
	}
}

func TestAsyncQuestionHistoryIsReadOnlyChronologicalAndStructured(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type outcome struct {
		questions []AsyncQuestion
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		questions, err := client.AsyncQuestions(context.Background(), "thread")
		done <- outcome{questions, err}
	}()
	for index, page := range []string{
		`{"data":[{"id":"turn-a","itemsView":"full","items":[{"id":"private","type":"reasoning","text":"private reasoning"},{"id":"q-a","type":"agentMessage","delivery":"async","phase":"final_answer","text":"Which hardware?","questions":[{"title":"Which hardware?","options":["Mac","Windows"]}]}]}],"nextCursor":"next"}`,
		`{"data":[{"id":"turn-b","items":[{"id":"ordinary","type":"agentMessage","text":"A rhetorical question?"},{"id":"q-b","type":"agentMessage","delivery":"async","text":"Details?","questions":[{"title":"Details?","options":null}]}]}]}`,
	} {
		request := fake.next(t)
		fields := params(t, request)
		if method(t, request) != "thread/turns/list" || string(fields["threadId"]) != `"thread"` || string(fields["sortDirection"]) != `"asc"` || string(fields["itemsView"]) != `"full"` || string(fields["limit"]) != "1" {
			t.Fatalf("unexpected or mutating history read: %#v", request)
		}
		if index == 0 && len(fields) != 4 || index == 1 && string(fields["cursor"]) != `"next"` {
			t.Fatalf("history cursor not preserved: %#v", fields)
		}
		fake.respond(t, request, json.RawMessage(page))
	}
	got := <-done
	want := []AsyncQuestion{
		{TurnID: "turn-a", ItemID: "q-a", Text: "Which hardware?", Questions: []Question{{ID: "q1", Prompt: "Which hardware?", IsOther: true, Choices: []Choice{{Label: "Mac"}, {Label: "Windows"}}}}},
		{TurnID: "turn-b", ItemID: "q-b", Text: "Details?", Questions: []Question{{ID: "q1", Prompt: "Details?", IsOther: true}}},
	}
	if got.err != nil || !reflect.DeepEqual(got.questions, want) {
		t.Fatalf("question history = %#v, %v", got.questions, got.err)
	}
	_ = client.Close()
	if fake.scan.Scan() {
		t.Fatalf("unexpected additional RPC: %s", fake.scan.Text())
	}
}

func TestAsyncQuestionHistoryRejectsIncompletePages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []string
		max   int
	}{
		{"missing data", []string{`{}`}, 3},
		{"summary", []string{`{"data":[{"id":"turn","itemsView":"summary","items":[]}]}`}, 3},
		{"missing items", []string{`{"data":[{"id":"turn"}]}`}, 3},
		{"duplicate turn", []string{`{"data":[{"id":"turn","items":[]}],"nextCursor":"next"}`, `{"data":[{"id":"turn","items":[]}]}`}, 3},
		{"repeated cursor", []string{`{"data":[{"id":"a","items":[]}],"nextCursor":"next"}`, `{"data":[{"id":"b","items":[]}],"nextCursor":"next"}`}, 3},
		{"page limit", []string{`{"data":[{"id":"turn","items":[]}],"nextCursor":"next"}`}, 1},
		{"empty title", []string{`{"data":[{"id":"turn","items":[{"id":"q","type":"agentMessage","delivery":"async","questions":[{"title":" "}]}]}]}`}, 3},
		{"missing identity", []string{`{"data":[{"id":"turn","items":[{"type":"agentMessage","delivery":"async","questions":[{"title":"Question?"}]}]}]}`}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() {
				_, err := client.asyncQuestions(context.Background(), "thread", tc.max)
				done <- err
			}()
			for _, page := range tc.pages {
				fake.respond(t, fake.next(t), json.RawMessage(page))
			}
			if err := <-done; !errors.Is(err, ErrHistoryUnavailable) {
				t.Fatalf("expected incomplete-history rejection, got %v", err)
			}
		})
	}
}
