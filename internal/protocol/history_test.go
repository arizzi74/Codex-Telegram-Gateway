package protocol

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReadHistoryRequiresFrozenTargetAndBoundedPagination(t *testing.T) {
	command := Command{ID: uuid.NewString(), WorkerID: uuid.NewString(), RuntimeID: uuid.NewString(),
		RuntimeGeneration: 1, SessionID: uuid.NewString(), ThreadID: "thread", Operation: ReadHistory,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Arguments: Arguments{History: &HistoryRequest{Limit: DefaultHistoryLimit}}}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, request := range []*HistoryRequest{nil, {}, {Limit: 51}, {Limit: -1},
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
