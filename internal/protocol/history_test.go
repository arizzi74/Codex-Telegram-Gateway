package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHistoryTimestampRoundTripAndLegacyAbsence(t *testing.T) {
	stamp := time.Date(2026, 9, 20, 12, 34, 56, 0, time.UTC)
	page := HistoryPage{Limit: DefaultHistoryLimit, Prompts: []HistoryPrompt{{TurnID: "turn", ItemID: "prompt", Text: "saved input", Timestamp: &stamp}}, Messages: []HistoryMessage{{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "saved reply", Timestamp: &stamp}}}
	data, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var decoded HistoryPage
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.Prompts[0].Timestamp == nil || !decoded.Prompts[0].Timestamp.Equal(stamp) || decoded.Messages[0].Timestamp == nil || !decoded.Messages[0].Timestamp.Equal(stamp) {
		t.Fatalf("history timestamp did not survive transport: %#v, %v", decoded, err)
	}
	var old HistoryPrompt
	if err := json.Unmarshal([]byte(`{"turn_id":"turn","item_id":"prompt","text":"old"}`), &old); err != nil || old.Timestamp != nil {
		t.Fatalf("legacy history manufactured a timestamp: %#v, %v", old, err)
	}
}

func TestReadHistoryRequiresFrozenTargetAndBoundedPagination(t *testing.T) {
	command := Command{ID: uuid.NewString(), WorkerID: uuid.NewString(), RuntimeID: uuid.NewString(),
		RuntimeGeneration: 1, SessionID: uuid.NewString(), ThreadID: "thread", Operation: ReadHistory,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Arguments: Arguments{History: &HistoryRequest{Limit: DefaultHistoryLimit}}}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, request := range []*HistoryRequest{nil, {}, {Limit: 51}, {Limit: -1},
		{Limit: 2, NewestFirst: true},
		{Limit: 10, Before: &HistoryCursor{TurnID: "turn"}},
		{Limit: 10, Before: &HistoryCursor{TurnID: "\n", ItemID: "item"}},
		{Limit: 10, Before: &HistoryCursor{TurnID: "turn", ItemID: strings.Repeat("x", 513)}},
	} {
		command.Arguments.History = request
		if command.Validate() == nil {
			t.Fatalf("accepted invalid history request: %#v", request)
		}
	}
	command.Arguments.History = &HistoryRequest{Limit: 10}
	command.SessionID = ""
	if command.Validate() == nil {
		t.Fatal("accepted history without a session target")
	}
}
