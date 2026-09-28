package registry

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func usageCallbackTestMenu(t *testing.T, env eventTestEnv) Callback {
	t.Helper()
	accepted := acceptUsageEventCommand(t, env)
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, State: "completed", UsageMenu: usageEventMenu()})
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return Callback{Action: "usage", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77,
		SessionID: env.session, RuntimeID: env.runtime, Generation: 1, QuestionID: accepted.CommandID,
		Decision: "resets", ExpiresAt: time.Now().Add(time.Minute)}
}

func TestUsageCallbackPreservesFrozenTargetThroughConfirmation(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	menu := usageCallbackTestMenu(t, env)
	other := uuid.New()
	insertRouteSession(t, env, other, "other-usage-thread", "")
	selection := telegramUpdate(env, 3)
	selection.Action, selection.Target, selection.TopicID = "connect", other.String(), 77
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	capability := uuid.NewString()
	steps := []struct {
		args string
		next *protocol.UsageMenu
	}{
		{"resets", &protocol.UsageMenu{Options: []protocol.UsageOption{{Args: "resets 1", Label: "Next resets"}, {Args: "cancel", Label: "Cancel"}}}},
		{"resets 1", &protocol.UsageMenu{Options: []protocol.UsageOption{{Args: "redeem " + capability, Label: "Redeem reset"}, {Args: "cancel", Label: "Cancel"}}}},
		{"redeem " + capability, &protocol.UsageMenu{Options: []protocol.UsageOption{{Args: "confirm " + capability, Label: "Confirm redemption"}, {Args: "cancel", Label: "Cancel"}}}},
		{"confirm " + capability, nil},
	}
	for i, step := range steps {
		menu.Decision = step.args
		token, err := env.store.CreateCallback(ctx, menu)
		if err != nil {
			t.Fatal(err)
		}
		menu.Decision = "cancel"
		cancel, err := env.store.CreateCallback(ctx, menu)
		if err != nil {
			t.Fatal(err)
		}
		in := telegramUpdate(env, int64(10+i*3))
		in.CallbackToken, in.TopicID = token, 77
		chosen, err := env.store.AcceptTelegram(ctx, in)
		if err != nil || chosen.CommandID == "" || chosen.SessionID != env.session.String() {
			t.Fatalf("step %d lost original route: %#v %v", i, chosen, err)
		}
		commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
		if err != nil || len(commands) != 1 || commands[0].Arguments.Codex.Name != "usage" || commands[0].Arguments.Codex.Args != step.args || commands[0].SessionID != env.session.String() || commands[0].ThreadID != "thread-1" || commands[0].RuntimeGeneration != 1 {
			t.Fatalf("step %d command: %#v %v", i, commands, err)
		}
		for j, stale := range []string{token, cancel} {
			in.UpdateID, in.CallbackToken = int64(11+i*3+j), stale
			if replay, err := env.store.AcceptTelegram(ctx, in); err != nil || replay.ErrorCode != "callback_invalid" || replay.CommandID != "" {
				t.Fatalf("used/sibling button accepted: %#v %v", replay, err)
			}
		}
		event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: chosen.CommandID, Text: "Usage response", UsageMenu: step.next})
		event.Seq = uint64(3 + i)
		if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
			t.Fatal(err)
		}
		menu.QuestionID = chosen.CommandID
	}
	var selected string
	if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE chat_id=20 AND message_thread_id=77").Scan(&selected); err != nil || selected != other.String() {
		t.Fatalf("usage picker changed selection: %s %v", selected, err)
	}
}

func TestUsageCallbackRejectsUnownedUnadvertisedOrStaleChoices(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	menu := usageCallbackTestMenu(t, env)
	for _, scenario := range []string{"wrong user", "wrong chat", "wrong bot", "wrong topic", "wrong session", "wrong runtime", "wrong generation", "wrong command", "missing generation", "malformed", "unadvertised page", "unadvertised redemption", "unadvertised confirmation"} {
		t.Run(scenario, func(t *testing.T) {
			invalid := menu
			switch scenario {
			case "wrong user":
				invalid.UserID++
			case "wrong chat":
				invalid.ChatID++
			case "wrong bot":
				invalid.BotID = "other"
			case "wrong topic":
				invalid.TopicID++
			case "wrong session":
				invalid.SessionID = uuid.New()
			case "wrong runtime":
				invalid.RuntimeID = uuid.New()
			case "wrong generation":
				invalid.Generation++
			case "wrong command":
				invalid.QuestionID = uuid.NewString()
			case "missing generation":
				invalid.Generation = 0
			case "malformed":
				invalid.Decision = "resets\n"
			case "unadvertised page":
				invalid.Decision = "resets 1"
			case "unadvertised redemption":
				invalid.Decision = "redeem " + uuid.NewString()
			case "unadvertised confirmation":
				invalid.Decision = "confirm " + uuid.NewString()
			}
			if token, err := env.store.CreateCallback(ctx, invalid); err == nil || token != "" {
				t.Fatalf("invalid callback created: %q %v", token, err)
			}
		})
	}
	for i, scenario := range []string{"wrong user", "wrong chat", "wrong bot", "wrong topic", "unadvertised", "expired", "stale generation"} {
		t.Run("consume "+scenario, func(t *testing.T) {
			token, err := env.store.CreateCallback(ctx, menu)
			if err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, int64(10+i))
			in.CallbackToken, in.TopicID = token, 77
			switch scenario {
			case "wrong user":
				in.UserID++
			case "wrong chat":
				in.ChatID++
			case "wrong bot":
				in.BotID = "other"
			case "wrong topic":
				in.TopicID++
			case "unadvertised":
				_, err = env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.decision',$2) WHERE token=$1`, token, "confirm "+uuid.NewString())
			case "expired":
				_, err = env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET expires_at=$2 WHERE token=$1`, token, time.Now().Add(-time.Minute))
			case "stale generation":
				_, err = env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || result.ErrorCode != "callback_invalid" || result.CommandID != "" {
				t.Fatalf("invalid callback accepted: %#v %v", result, err)
			}
		})
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 20)
	if err != nil || len(commands) != 0 {
		t.Fatalf("invalid choices queued commands: %#v %v", commands, err)
	}
	requester, err := env.store.UsageRequester(ctx, uuid.MustParse(menu.QuestionID))
	if err != nil || requester != menu.UserID {
		t.Fatalf("requester: %d %v", requester, err)
	}
	if _, err := env.store.UsageRequester(ctx, uuid.New()); err == nil {
		t.Fatal("missing requester accepted")
	}
}

func TestUsageCallbackConfirmAndCancelAreAtomic(t *testing.T) {
	env := newHistoryTestEnv(t)
	accepted := acceptUsageEventCommand(t, env)
	menu := Callback{Action: "usage", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77,
		SessionID: env.session, RuntimeID: env.runtime, Generation: 1, QuestionID: accepted.CommandID,
		ExpiresAt: time.Now().Add(time.Minute)}
	confirm := "confirm " + uuid.NewString()
	payload := protocol.Result{CommandID: menu.QuestionID, UsageMenu: &protocol.UsageMenu{Options: []protocol.UsageOption{{Args: confirm, Label: "Confirm redemption"}, {Args: "cancel", Label: "Cancel"}}}}
	event := historyTestEvent(t, env, "command_completed", payload)
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, args := range []string{confirm, "cancel"} {
		menu.Decision = args
		token, err := env.store.CreateCallback(t.Context(), menu)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	var wg sync.WaitGroup
	results := make(chan AcceptResult, 2)
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := telegramUpdate(env, int64(4+i))
			in.CallbackToken, in.TopicID = token, 77
			result, err := env.store.AcceptTelegram(t.Context(), in)
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
	commands, err := env.store.PendingCommandsForWorker(t.Context(), env.worker, 20)
	if err != nil || queued != 1 || rejected != 1 || len(commands) != 1 || (commands[0].Arguments.Codex.Args != "cancel" && !strings.HasPrefix(commands[0].Arguments.Codex.Args, "confirm ")) {
		t.Fatalf("choice race: queued=%d rejected=%d commands=%#v err=%v", queued, rejected, commands, err)
	}
}
