package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func newHistoryTestEnv(t *testing.T) eventTestEnv {
	t.Helper()
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 1)
	in.Action, in.Target = "connect", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
		t.Fatal(err)
	}
	return env
}

func historyTestPage() *protocol.HistoryPage {
	return &protocol.HistoryPage{Limit: protocol.DefaultHistoryLimit, NewestFirst: true, Prompts: []protocol.HistoryPrompt{
		{TurnID: "new-turn", ItemID: "new-item", Text: "Later Telegram prompt"},
		{TurnID: "old-turn", ItemID: "old-item", Text: "Earlier CLI prompt"},
	}, Next: &protocol.HistoryCursor{TurnID: "old-turn", ItemID: "old-item"}}
}

func TestHistoryValidatesOldAndNewWorkerPagination(t *testing.T) {
	page := historyTestPage()
	request := &protocol.HistoryRequest{Limit: protocol.DefaultHistoryLimit}
	if !validHistoryPage(page, request) {
		t.Fatal("newest-first page rejected")
	}
	page.NewestFirst = false
	if validHistoryPage(page, request) {
		t.Fatal("oldest cursor was accepted in the wrong position")
	}
	page.Prompts[0], page.Prompts[1] = page.Prompts[1], page.Prompts[0]
	if !validHistoryPage(page, request) {
		t.Fatal("legacy chronological worker page rejected")
	}
}

func historyTestEvent(t *testing.T, env eventTestEnv, kind string, result protocol.Result) protocol.Event {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}
}

func acceptHistoryTestCommand(t *testing.T, env eventTestEnv, count string) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.TopicID = "history", count, 77
	accepted, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("accept history: %#v, %v", accepted, err)
	}
	return accepted
}

func TestHistoryFrozenRoutingAndPrivateDeliveryIntegration(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "prompts"
		if empty {
			name = "empty history"
		}
		t.Run(name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET state='running', active_turn_id='current-turn' WHERE session_id=$1", env.session); err != nil {
				t.Fatal(err)
			}
			otherChat := telegramUpdate(env, 10)
			otherChat.Action, otherChat.Target, otherChat.ChatID, otherChat.UserID = "connect", env.session.String(), 444, 11
			if _, err := env.store.AcceptTelegram(ctx, otherChat); err != nil {
				t.Fatal(err)
			}
			accepted := acceptHistoryTestCommand(t, env, "")
			other := uuid.New()
			insertRouteSession(t, env, other, "another-thread", "")
			selection := telegramUpdate(env, 3)
			selection.Action, selection.Target = "connect", other.String()
			if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			if err != nil || len(commands) != 1 {
				t.Fatalf("pending: %#v, %v", commands, err)
			}
			command := commands[0]
			if command.Operation != protocol.ReadHistory || command.SessionID != env.session.String() || command.ThreadID != "thread-1" || command.RuntimeGeneration != 1 || command.Arguments.History.Limit != protocol.DefaultHistoryLimit || command.Arguments.Text != "" || command.ExpectedTurnID != "" || command.Arguments.Codex != nil {
				t.Fatalf("history became a prompt or lost its target: %#v", command)
			}
			requester, err := env.store.HistoryRequester(ctx, uuid.MustParse(command.ID))
			if err != nil || requester != 10 {
				t.Fatalf("history requester: %d, %v", requester, err)
			}
			page := historyTestPage()
			if empty {
				page.Prompts, page.Next = nil, nil
			}
			event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, State: "completed", History: page})
			for range 2 {
				if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			var chat, topic int64
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*), min(chat_id), min(message_thread_id) FROM telegram_deliveries WHERE event_id=$1`, event.ID).Scan(&count, &chat, &topic); err != nil || count != 1 || chat != 20 || topic != 77 {
				t.Fatalf("private history delivery: count=%d chat=%d topic=%d err=%v", count, chat, topic, err)
			}
			var state, turn, selected, status string
			if err := env.store.pool.QueryRow(ctx, "SELECT state, active_turn_id FROM sessions WHERE session_id=$1", env.session).Scan(&state, &turn); err != nil || state != "running" || turn != "current-turn" {
				t.Fatalf("history mutated execution: %q %q %v", state, turn, err)
			}
			if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE chat_id=20").Scan(&selected); err != nil || selected != other.String() {
				t.Fatalf("history changed selection: %q %v", selected, err)
			}
			if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1", command.ID).Scan(&status); err != nil || status != "completed" {
				t.Fatalf("history completion: %q %v", status, err)
			}
		})
	}
}

func TestHistoryCountValidationIntegration(t *testing.T) {
	for _, count := range []string{"0", "51", "-1", "+1", "1 2", "1.5", "all", "/status", "999999999999999999999"} {
		t.Run(count, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			in := telegramUpdate(env, 2)
			in.Action, in.Text = "history", count
			result, err := env.store.AcceptTelegram(context.Background(), in)
			if err != nil || result.View != "error" || result.ErrorCode != "history_usage" || result.CommandID != "" {
				t.Fatalf("invalid history count: %#v, %v", result, err)
			}
			commands, err := env.store.PendingCommands(context.Background(), 10)
			if err != nil || len(commands) != 0 {
				t.Fatalf("invalid count created a command: %#v, %v", commands, err)
			}
		})
	}
	for _, count := range []string{"1", " 50 "} {
		t.Run(count, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			acceptHistoryTestCommand(t, env, count)
			commands, err := env.store.PendingCommands(context.Background(), 10)
			if err != nil || len(commands) != 1 || commands[0].Operation != protocol.ReadHistory || commands[0].Arguments.Text != "" {
				t.Fatalf("valid count became a prompt: %#v, %v", commands, err)
			}
		})
	}
}

func TestHistoryCallbackScopeAndPaginationIntegration(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong user", "wrong bot", "wrong chat", "wrong topic", "switched selection", "overridden topic", "stale generation", "expired", "malformed request"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			request := &protocol.HistoryRequest{Limit: 3, Before: &protocol.HistoryCursor{TurnID: "old-turn", ItemID: "old-item"}}
			token, err := env.store.CreateCallback(ctx, Callback{Action: "history", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77, SessionID: env.session, RuntimeID: env.runtime, Generation: 1, History: request, ExpiresAt: time.Now().Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.CallbackToken, in.TopicID = token, 77
			switch scenario {
			case "wrong user":
				in.UserID++
			case "wrong bot":
				in.BotID = "other-bot"
			case "wrong chat":
				in.ChatID++
			case "wrong topic":
				in.TopicID++
			case "switched selection", "overridden topic":
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "")
				selection := telegramUpdate(env, 3)
				selection.Action, selection.Target = "connect", other.String()
				if scenario == "overridden topic" {
					selection.TopicID = 77
				}
				if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
					t.Fatal(err)
				}
				if err := env.store.RecordBotMessageRoute(ctx, "bot", 20, 99, env.session, "", uuid.Nil); err != nil {
					t.Fatal(err)
				}
				in.ReplyToMessageID = 99
			case "stale generation":
				if _, err := env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := env.store.pool.Exec(ctx, "UPDATE telegram_callbacks SET expires_at=$2 WHERE token=$1", token, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			case "malformed request":
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.history.limit',0) WHERE token=$1`, token); err != nil {
					t.Fatal(err)
				}
			}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommands(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "valid" {
				if result.ErrorCode != "callback_invalid" || len(commands) != 0 {
					t.Fatalf("invalid history callback accepted: %#v, %#v", result, commands)
				}
				return
			}
			if result.CommandID == "" || len(commands) != 1 || commands[0].Operation != protocol.ReadHistory || commands[0].SessionID != env.session.String() || commands[0].Arguments.History.Limit != request.Limit || *commands[0].Arguments.History.Before != *request.Before {
				t.Fatalf("history callback lost frozen page: %#v, %#v", result, commands)
			}
			in.UpdateID++
			result, err = env.store.AcceptTelegram(ctx, in)
			if err != nil || result.ErrorCode != "callback_invalid" {
				t.Fatalf("replayed history callback: %#v, %v", result, err)
			}
		})
	}
}

func TestHistoryCallbackClaimIsAtomicIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	token, err := env.store.CreateCallback(ctx, Callback{Action: "history", BotID: "bot", UserID: 10, ChatID: 20, SessionID: env.session, RuntimeID: env.runtime, Generation: 1, History: &protocol.HistoryRequest{Limit: 10}, ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan AcceptResult, 2)
	for _, updateID := range []int64{2, 3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := telegramUpdate(env, updateID)
			in.CallbackToken = token
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil {
				t.Error(err)
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	queued, rejected := 0, 0
	for result := range results {
		if result.CommandID != "" {
			queued++
		}
		if result.ErrorCode == "callback_invalid" {
			rejected++
		}
	}
	if queued != 1 || rejected != 1 {
		t.Fatalf("history callback race: queued=%d rejected=%d", queued, rejected)
	}
}

func TestHistoryResultRejectsMalformedAndUnsolicitedPagesIntegration(t *testing.T) {
	for _, scenario := range []string{"unsolicited", "wrong operation", "wrong session", "wrong generation", "missing page", "wrong limit", "too many prompts", "long prompt", "large page", "duplicate prompt", "empty prompt", "long id", "wrong next", "empty next", "turn mutation", "session mutation", "history on failure", "history on snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			var accepted AcceptResult
			if scenario == "wrong operation" {
				in := telegramUpdate(env, 2)
				in.Text = "ordinary prompt"
				var err error
				accepted, err = env.store.AcceptTelegram(ctx, in)
				if err != nil || accepted.CommandID == "" {
					t.Fatalf("accept prompt: %#v, %v", accepted, err)
				}
			} else {
				accepted = acceptHistoryTestCommand(t, env, "")
			}
			result := protocol.Result{CommandID: accepted.CommandID, State: "completed", History: historyTestPage()}
			kind := "command_completed"
			switch scenario {
			case "unsolicited":
				result.CommandID = ""
			case "missing page":
				result.History = nil
			case "wrong limit":
				result.History.Limit++
			case "too many prompts":
				result.History.Prompts = make([]protocol.HistoryPrompt, 11)
			case "long prompt":
				result.History.Prompts[0].Text = strings.Repeat("a", 16001)
			case "large page":
				result.History.Prompts = nil
				for range 5 {
					result.History.Prompts = append(result.History.Prompts, protocol.HistoryPrompt{TurnID: "turn", ItemID: uuid.NewString(), Text: strings.Repeat("a", 16000)})
				}
			case "duplicate prompt":
				result.History.Prompts[1] = result.History.Prompts[0]
			case "empty prompt":
				result.History.Prompts[0].Text = " "
			case "long id":
				result.History.Prompts[0].ItemID = strings.Repeat("a", 513)
			case "wrong next":
				result.History.Next.ItemID = "unrelated-item"
			case "empty next":
				result.History.Prompts = nil
			case "turn mutation":
				kind, result.TurnID, result.History = "turn_started", "forged-turn", nil
			case "session mutation":
				result.Session = &protocol.Session{ID: env.session.String(), RuntimeID: env.runtime.String(), WorkerID: env.worker.String(), ThreadID: "thread-1", State: "failed"}
			case "history on failure":
				kind = "command_failed"
			case "history on snapshot":
				kind, result.CommandID = "session_state_changed", ""
			}
			event := historyTestEvent(t, env, kind, result)
			if scenario == "wrong session" {
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "")
				event.SessionID = other.String()
			}
			if scenario == "wrong generation" {
				event.RuntimeGeneration = 2
			}
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); !errors.Is(err, ErrEventTarget) {
				t.Fatalf("malformed history accepted: %v", err)
			}
			var state, status string
			if err := env.store.pool.QueryRow(ctx, "SELECT state FROM sessions WHERE session_id=$1", env.session).Scan(&state); err != nil || state != "idle" {
				t.Fatalf("malformed history changed session: %q, %v", state, err)
			}
			if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1", accepted.CommandID).Scan(&status); err != nil || status != "pending" {
				t.Fatalf("malformed history changed command: %q, %v", status, err)
			}
			if watermark, err := env.store.EventWatermark(ctx, env.worker); err != nil || watermark != 1 {
				t.Fatalf("malformed history persisted: %d, %v", watermark, err)
			}
		})
	}
}

func TestHistoryStaleGenerationIsAuditedWithoutDeliveryIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	accepted := acceptHistoryTestCommand(t, env, "")
	if _, err := env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime); err != nil {
		t.Fatal(err)
	}
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, History: historyTestPage()})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	var count int
	var status string
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id=$1", event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale history delivered: %d, %v", count, err)
	}
	if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1", accepted.CommandID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("stale history not acknowledged: %q, %v", status, err)
	}
}

func TestHistoryFailureIsPrivateIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	in := telegramUpdate(env, 3)
	in.Action, in.Target, in.ChatID = "connect", env.session.String(), 888
	if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
		t.Fatal(err)
	}
	accepted := acceptHistoryTestCommand(t, env, "")
	event := historyTestEvent(t, env, "command_failed", protocol.Result{CommandID: accepted.CommandID, Error: &protocol.Error{Code: "history_unavailable", Message: "Saved history is unavailable."}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	var count int
	var chat int64
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*), min(chat_id) FROM telegram_deliveries WHERE event_id=$1", event.ID).Scan(&count, &chat); err != nil || count != 1 || chat != 20 {
		t.Fatalf("history failure broadcast: %d %d %v", count, chat, err)
	}
}
