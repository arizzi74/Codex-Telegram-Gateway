package codexadapter

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAsyncQuestionExtractsExactMultilineAnswers(t *testing.T) {
	text := "> Hardware?\n\nMac Studio\nApple Silicon\n\nTwo displays\n\n> Details?\n\n  External audio\nSecond monitor  "
	for title, want := range map[string]string{
		"Hardware?": "Mac Studio\nApple Silicon\n\nTwo displays",
		"Details?":  "  External audio\nSecond monitor  ",
	} {
		answer, ok := AsyncQuestionAnswer(title, text, "Hardware?", "Details?")
		if !ok || answer != want {
			t.Fatalf("answer for %q = %q, %v; want %q", title, answer, ok, want)
		}
	}
	for _, text := range []string{"Mac", "> Other question?\n\nMac", "> Hardware?\n\n  "} {
		if answer, ok := AsyncQuestionAnswer("Hardware?", text); ok || answer != "" {
			t.Fatalf("unconfirmed answer = %q, %v", answer, ok)
		}
	}
	if answer, ok := AsyncQuestionAnswer("Hardware?", "> Hardware?\n\nMac\n\n> quoted diagnostic\n\ncontinued"); !ok || answer != "Mac\n\n> quoted diagnostic\n\ncontinued" {
		t.Fatalf("quoted answer content was truncated: %q, %v", answer, ok)
	}
}

func TestAsyncQuestionHistoryPreservesAnswersAndDoesNotMutatePrior(t *testing.T) {
	prior := []AsyncQuestion{{TurnID: "old", ItemID: "question", Questions: []Question{{ID: "q1", Prompt: "Hardware?"}, {ID: "q2", Prompt: "Details?"}}, AnsweredIDs: []string{"q1"}, Answers: map[string][]string{"q1": {"Mac"}}}}
	raw := json.RawMessage(`{"id":"new","items":[{"type":"userMessage","id":"answer","content":[{"type":"text","text":"> Details?\n\nExternal audio\nSecond display"}]}]}`)
	turn, err := decodeHistoryTurn(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := ResolveHistoryQuestions(prior, []HistoryTurn{turn})
	want := map[string][]string{"q1": {"Mac"}, "q2": {"External audio\nSecond display"}}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Answers, want) {
		t.Fatalf("recovered answers = %#v", got)
	}
	if !reflect.DeepEqual(prior[0].Answers, map[string][]string{"q1": {"Mac"}}) {
		t.Fatalf("resolution mutated prior answers: %#v", prior)
	}
	legacy, err := resolveAsyncQuestionHistory(prior, raw)
	if err != nil || !reflect.DeepEqual(legacy[0].Answers, want) {
		t.Fatalf("legacy history answers = %#v, %v", legacy, err)
	}
	unknown := ResolveHistoryQuestions([]AsyncQuestion{{TurnID: "old", ItemID: "question", Questions: []Question{{ID: "q1", Prompt: "Hardware?"}}}}, []HistoryTurn{{ID: "later", QuestionEvents: []HistoryQuestionEvent{{IsInput: true, Text: "Proceed"}}}})
	if len(unknown[0].Answers) != 0 || len(unknown[0].AnsweredIDs) != 0 || !reflect.DeepEqual(unknown[0].SupersededIDs, []string{"q1"}) {
		t.Fatalf("ordinary input fabricated answers: %#v", unknown)
	}
}
