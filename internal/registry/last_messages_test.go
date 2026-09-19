package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestLastMessagesFrozenReadAndPrivateResultIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.TopicID = "last_messages", "2", 77
	accepted, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("accept: %#v %v", accepted, err)
	}
	commands, err := env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands=%#v %v", commands, err)
	}
	command := commands[0]
	if command.Operation != protocol.ReadHistory || command.Arguments.History == nil || !command.Arguments.History.Messages || command.Arguments.History.Limit != 2 || command.Arguments.Text != "" || command.ThreadID != "thread-1" || command.SessionID != env.session.String() {
		t.Fatalf("message command lost frozen read target: %#v", command)
	}
	// A second chat connected to this thread must not receive private history.
	other := telegramUpdate(env, 3)
	other.Action, other.Target, other.ChatID = "connect", env.session.String(), 888
	if _, err := env.store.AcceptTelegram(ctx, other); err != nil {
		t.Fatal(err)
	}
	// Unlike unsolicited turn completions, this explicitly requested private
	// read remains deliverable if the user selects another session meanwhile.
	otherSession := uuid.New()
	insertRouteSession(t, env, otherSession, "other-thread", "")
	selection := telegramUpdate(env, 4)
	selection.Action, selection.Target = "connect", otherSession.String()
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	page := &protocol.HistoryPage{Limit: 2, Conversation: true, Messages: []protocol.HistoryMessage{
		{TurnID: "turn", ItemID: "user", Role: "user", Text: "Telegram prompt"},
		{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "Codex reply"},
	}, Next: &protocol.HistoryCursor{TurnID: "turn", ItemID: "user"}}
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, State: "completed", History: page})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	var count int
	var chat, topic int64
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*),min(chat_id),min(message_thread_id) FROM telegram_deliveries WHERE event_id=$1`, event.ID).Scan(&count, &chat, &topic); err != nil || count != 1 || chat != 20 || topic != 77 {
		t.Fatalf("private result count=%d chat=%d topic=%d err=%v", count, chat, topic, err)
	}
	var deliveryID string
	if err := env.store.pool.QueryRow(ctx, `SELECT delivery_id FROM telegram_deliveries WHERE event_id=$1`, event.ID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	if suppressed, err := env.store.SuppressTelegramDelivery(ctx, deliveryID); err != nil || suppressed {
		t.Fatalf("explicit history was suppressed after switching: %v %v", suppressed, err)
	}
	selection.UpdateID, selection.Target = 5, env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	token, err := env.store.CreateCallback(ctx, Callback{Action: "history", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77, SessionID: env.session, RuntimeID: env.runtime, Generation: 1, History: &protocol.HistoryRequest{Limit: 2, Messages: true, Before: page.Next}, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	older := telegramUpdate(env, 6)
	older.CallbackToken, older.TopicID = token, 77
	result, err := env.store.AcceptTelegram(ctx, older)
	if err != nil || result.CommandID == "" {
		t.Fatalf("older callback=%#v %v", result, err)
	}
	var payload []byte
	if err := env.store.pool.QueryRow(ctx, `SELECT payload FROM commands WHERE command_id=$1`, result.CommandID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var next protocol.Command
	if json.Unmarshal(payload, &next) != nil || !next.Arguments.History.Messages || next.Arguments.History.Before == nil || *next.Arguments.History.Before != *page.Next {
		t.Fatalf("older page lost conversation or cursor: %s", payload)
	}
}

func TestLastMessagesDefaultAndCountValidationIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	for i, count := range []string{"", "1", "50", "0", "51", "-1", "+1", "1 2", "all", "9999999999999999999999999"} {
		in := telegramUpdate(env, int64(i+2))
		in.Action, in.Text = "last_messages", count
		result, err := env.store.AcceptTelegram(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if i >= 3 {
			if result.ErrorCode != "last_messages_usage" || result.CommandID != "" {
				t.Fatalf("bad count %q accepted: %#v", count, result)
			}
			continue
		}
		var payload []byte
		if err := env.store.pool.QueryRow(t.Context(), `SELECT payload FROM commands WHERE command_id=$1`, result.CommandID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var command protocol.Command
		want := 1
		if count == "50" {
			want = 50
		}
		if json.Unmarshal(payload, &command) != nil || command.Operation != protocol.ReadHistory || command.Arguments.History.Limit != want || !command.Arguments.History.Messages {
			t.Fatalf("invalid read for %q: %s", count, payload)
		}
	}
}

func TestLastMessagesValidationSeparatesConversationFromPromptHistory(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong mode", "legacy prompts", "role", "duplicate", "cursor", "before", "empty", "oversize", "overcount", "missing mode", "legacy injection"} {
		t.Run(scenario, func(t *testing.T) {
			request := &protocol.HistoryRequest{Limit: 2, Messages: true}
			page := &protocol.HistoryPage{Limit: 2, Conversation: true, Messages: []protocol.HistoryMessage{{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "answer"}}}
			switch scenario {
			case "wrong mode":
				request.Messages = false
			case "legacy prompts":
				page.Prompts = []protocol.HistoryPrompt{{TurnID: "turn", ItemID: "legacy", Text: "legacy"}}
			case "role":
				page.Messages[0].Role = "reasoning"
			case "duplicate":
				page.Messages = append(page.Messages, page.Messages[0])
			case "cursor":
				page.Next = &protocol.HistoryCursor{TurnID: "turn", ItemID: "wrong"}
			case "before":
				request.Before = &protocol.HistoryCursor{TurnID: "turn", ItemID: "answer"}
			case "empty":
				page.Messages[0].Text = " "
			case "oversize":
				page.Messages[0].Text = strings.Repeat("x", 16001)
			case "overcount":
				page.Limit = 1
				page.Messages = append(page.Messages, protocol.HistoryMessage{TurnID: "turn", ItemID: "another", Role: "user", Text: "user"})
			case "missing mode":
				page.Conversation = false
			case "legacy injection":
				page.Conversation = false
				request.Messages = false
			}
			if got := validHistoryPage(page, request); got != (scenario == "valid") {
				t.Fatalf("validHistoryPage=%v", got)
			}
		})
	}
}

func TestLastMessagesUnsupportedWorkerAcknowledgementRepliesOncePrivately(t *testing.T) {
	env := newHistoryTestEnv(t)
	in := telegramUpdate(env, 2)
	in.Action, in.TopicID = "last_messages", 77
	accepted, err := env.store.AcceptTelegram(t.Context(), in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("accept: %#v %v", accepted, err)
	}
	ack := protocol.CommandAck{CommandID: accepted.CommandID, Status: "rejected", Error: &protocol.Error{Code: protocol.UnsupportedOperation, Message: "Update worker"}}
	for range 2 {
		if err := env.store.AcknowledgeCommand(t.Context(), env.worker, env.connection, ack); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT status FROM commands WHERE command_id=$1`, accepted.CommandID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("commandstatus=%s err=%v", status, err)
	}
	var count int
	var chat, topic int64
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*),min(chat_id),min(message_thread_id) FROM telegram_deliveries WHERE kind='ui_response' AND json_extract(payload,'$.command_id')=$1 AND json_extract(payload,'$.error_code')=$2`, accepted.CommandID, protocol.UnsupportedOperation).Scan(&count, &chat, &topic); err != nil || count != 1 || chat != 20 || topic != 77 {
		t.Fatalf("rejection notification count=%d chat=%d topic=%d err=%v", count, chat, topic, err)
	}
	commands, err := env.store.PendingCommands(t.Context(), 10)
	if err != nil || len(commands) != 0 {
		t.Fatalf("rejected read remained queued: %#v %v", commands, err)
	}
}
