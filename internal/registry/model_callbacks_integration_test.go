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

func modelCallbackTestMenu(t *testing.T, env eventTestEnv) Callback {
	t.Helper()
	accepted := acceptModelEventCommand(t, env)
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, State: "completed", ModelMenu: modelEventMenu()})
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return Callback{Action: "model", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77,
		SessionID: env.session, RuntimeID: env.runtime, Generation: 1, QuestionID: accepted.CommandID,
		Decision: "--menu model-a", ExpiresAt: time.Now().Add(time.Minute)}
}

func TestModelCallbackPreservesFrozenTargetAcrossBothSteps(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	menu := modelCallbackTestMenu(t, env)
	first, err := env.store.CreateCallback(ctx, menu)
	if err != nil {
		t.Fatal(err)
	}
	menu.Decision = "--cancel"
	cancel, err := env.store.CreateCallback(ctx, menu)
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-model-thread", "")
	selection := telegramUpdate(env, 3)
	selection.Action, selection.Target, selection.TopicID = "connect", other.String(), 77
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 4)
	in.CallbackToken, in.TopicID = first, 77
	chosen, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || chosen.CommandID == "" || chosen.SessionID != env.session.String() {
		t.Fatalf("choose model: %#v %v", chosen, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Arguments.Codex.Args != "--menu model-a" || commands[0].Arguments.Codex.Name != "model" || commands[0].SessionID != env.session.String() || commands[0].RuntimeGeneration != 1 {
		t.Fatalf("choose lost frozen route: %#v %v", commands, err)
	}
	in.UpdateID, in.CallbackToken = 5, cancel
	if sibling, err := env.store.AcceptTelegram(ctx, in); err != nil || sibling.ErrorCode != "callback_invalid" {
		t.Fatalf("consumed sibling accepted: %#v %v", sibling, err)
	}
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: chosen.CommandID, ModelMenu: &protocol.ModelMenu{Options: []protocol.ModelOption{
		{Args: "model-a high", Label: "High"}, {Args: "--menu", Label: "Back to models"}, {Args: "--cancel", Label: "Cancel"},
	}}})
	event.Seq = 3
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	menu.QuestionID, menu.Decision = chosen.CommandID, "model-a high"
	apply, err := env.store.CreateCallback(ctx, menu)
	if err != nil {
		t.Fatal(err)
	}
	in.UpdateID, in.CallbackToken = 6, apply
	applied, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || applied.CommandID == "" || applied.SessionID != env.session.String() {
		t.Fatalf("apply lost frozen route: %#v %v", applied, err)
	}
	commands, err = env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Arguments.Codex.Args != "model-a high" || commands[0].SessionID != env.session.String() || commands[0].ThreadID != "thread-1" {
		t.Fatalf("apply command: %#v %v", commands, err)
	}
	var selected string
	if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE chat_id=20 AND message_thread_id=77").Scan(&selected); err != nil || selected != other.String() {
		t.Fatalf("picker changed selection: %s %v", selected, err)
	}
}

func TestModelCallbackRejectsUnownedUnadvertisedOrStaleChoices(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	menu := modelCallbackTestMenu(t, env)
	for _, scenario := range []string{"wrong user", "wrong chat", "wrong bot", "wrong topic", "wrong session", "wrong runtime", "wrong generation", "wrong command", "missing generation", "malformed", "unadvertised apply", "unadvertised model"} {
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
				invalid.Decision = "model\n high"
			case "unadvertised apply":
				invalid.Decision = "model-a high"
			case "unadvertised model":
				invalid.Decision = "--menu model-b"
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
				_, err = env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.decision','model-a high') WHERE token=$1`, token)
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
	requester, err := env.store.ModelRequester(ctx, uuid.MustParse(menu.QuestionID))
	if err != nil || requester != menu.UserID {
		t.Fatalf("requester: %d %v", requester, err)
	}
	if _, err := env.store.ModelRequester(ctx, uuid.New()); err == nil {
		t.Fatal("missing requester accepted")
	}
}

func TestModelCallbackSiblingChoicesAreAtomic(t *testing.T) {
	env := newHistoryTestEnv(t)
	menu := modelCallbackTestMenu(t, env)
	var tokens []string
	for _, args := range []string{"--menu model-a", "--cancel"} {
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
			in := telegramUpdate(env, int64(3+i))
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
	if err != nil || queued != 1 || rejected != 1 || len(commands) != 1 || !strings.HasPrefix(commands[0].Arguments.Codex.Args, "--") {
		t.Fatalf("choice race: queued=%d rejected=%d commands=%#v err=%v", queued, rejected, commands, err)
	}
}
