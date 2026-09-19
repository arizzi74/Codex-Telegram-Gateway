package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestNativeUserMessagesPreserveRepeatedCLIInputWithoutTelegramEcho(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "user-prompts", CWD: runtime.DefaultCWD, State: "running", Loaded: true, ActiveTurnID: "turn"})
	if err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.StartTurn)
	command.Arguments.Text = "same input"
	if _, err := a.store.Receive(command); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetCommandState(command.ID, CommandCompleted, &protocol.Result{TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	for _, input := range []struct{ id, turn, text string }{{"tg", "turn", "same input"}, {"cli", "turn", "same input"}, {"cli", "turn", "same input"}, {"stale", "old", "ignored"}, {"secret", "turn", "sk-abcdefghijklmnopqrstuvwxyz0123456789-secret"}} {
		actor.event(codexadapter.Event{Kind: "user_message_completed", TurnID: input.turn, ItemID: input.id, Text: input.text})
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, event := range events {
		if event.Kind != "user_message" {
			continue
		}
		var result protocol.Result
		if err := json.Unmarshal(event.Data, &result); err != nil {
			t.Fatal(err)
		}
		if result.CommandID != "" {
			t.Fatal("native prompt falsely linked to Telegram command")
		}
		texts = append(texts, result.Text)
	}
	if len(texts) != 2 || texts[0] != "same input" || strings.Contains(texts[1], "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("native prompt output=%#v", texts)
	}
}
