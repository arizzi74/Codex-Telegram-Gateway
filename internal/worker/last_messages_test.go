package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestLastMessagesIncludesTelegramAndCLIButNeverChangesTurn(t *testing.T) {
	for _, state := range []string{"idle", "not_loaded", "running"} {
		t.Run(state, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "messages", CWD: runtime.DefaultCWD, State: state, Loaded: state != "not_loaded"})
			if err != nil {
				t.Fatal(err)
			}
			actor := &sessionActor{agent: a, runtime: runtime, session: session, finalText: "preserve"}
			if state == "running" {
				actor.session.ActiveTurnID = "running-turn"
				actor.activeCommand = &protocol.Command{ID: "active-command"}
				actor.queue = []protocol.Command{{ID: "queued-command"}}
			}
			before, active := actor.session, actor.activeCommand
			thread := historyThread(session.ThreadID, "CLI prompt", "Telegram prompt")
			// Both user messages in this fixture belong to the same turn.
			raw, _ := json.Marshal(thread)
			var value map[string]any
			_ = json.Unmarshal(raw, &value)
			turn := value["turns"].([]any)[0].(map[string]any)
			turn["items"] = append(turn["items"].([]any), map[string]any{"type": "agentMessage", "id": "answer", "phase": "final_answer", "text": "Codex answer"})
			server.SetThreads([]map[string]any{value}, nil)
			// /tghistory would omit this correlated gateway submission. The
			// conversation view must retain it alongside the CLI prompt.
			original := agentCommand(runtime, session, protocol.StartTurn)
			original.Arguments.Text = "Telegram prompt"
			if _, err := a.store.Receive(original); err != nil {
				t.Fatal(err)
			}
			if err := a.store.SetCommandState(original.ID, CommandCompleted, &protocol.Result{TurnID: "turn"}); err != nil {
				t.Fatal(err)
			}
			command := agentCommand(runtime, session, protocol.ReadHistory)
			command.Arguments.History = &protocol.HistoryRequest{Limit: 3, Messages: true}
			record := runHistoryCommand(t, actor, command)
			if record.State != CommandCompleted || record.Result == nil || record.Result.History == nil {
				t.Fatalf("read failed: %#v", record)
			}
			page := record.Result.History
			if !page.Conversation || len(page.Prompts) != 0 || len(page.Messages) != 3 || page.Messages[0].Text != "CLI prompt" || page.Messages[1].Text != "Telegram prompt" || page.Messages[2].Role != "assistant" || page.Messages[2].Text != "Codex answer" {
				t.Fatalf("page=%#v", page)
			}
			if actor.session != before || actor.activeCommand != active || actor.finalText != "preserve" || (state == "running" && len(actor.queue) != 1) || record.Result.TurnID != "" || record.Result.Session != nil {
				t.Fatal("message read changed running session")
			}
			for _, call := range historyOperationCalls(server.Calls()) {
				if call.Method != "thread/turns/list" {
					t.Fatalf("history made mutating request: %s", call.Method)
				}
			}
		})
	}
}

func TestLastMessagesPageKeepsNewestBoundedRedactedSuffix(t *testing.T) {
	messages := make([]protocol.HistoryMessage, 10)
	for i := range messages {
		messages[i] = protocol.HistoryMessage{TurnID: "turn", ItemID: fmt.Sprint(i), Role: "assistant", Text: "SECRET_VALUE" + strings.Repeat("界", historyPromptRunes)}
	}
	redactor, err := auth.NewRedactor([]string{"SECRET_VALUE"}, "")
	if err != nil {
		t.Fatal(err)
	}
	page := lastMessagesPage(messages, false, 50, redactor)
	if len(page.Messages) != 4 || page.Messages[0].ItemID != "6" || page.Messages[3].ItemID != "9" || page.Next == nil || page.Next.ItemID != "6" {
		t.Fatalf("bounded page=%#v", page)
	}
	total := 0
	for _, message := range page.Messages {
		total += utf8.RuneCountInString(message.Text)
		if !message.Truncated || strings.Contains(message.Text, "SECRET_VALUE") {
			t.Fatal("unredacted or unbounded message")
		}
	}
	if total > historyPageRunes {
		t.Fatalf("message budget exceeded: %d", total)
	}
}
