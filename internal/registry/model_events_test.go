package registry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func modelEventMenu() *protocol.ModelMenu {
	return &protocol.ModelMenu{Options: []protocol.ModelOption{
		{Args: "--menu model-a", Label: "Model A"},
		{Args: "--cancel", Label: "Cancel"},
	}}
}

func acceptModelEventCommand(t *testing.T, env eventTestEnv) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, 2)
	in.Action, in.Target, in.TopicID = "codex", "model", 77
	accepted, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("accept models menu: %#v, %v", accepted, err)
	}
	return accepted
}

func TestModelMenuPrivateFrozenDeliveryIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET state='waiting_input', active_turn_id='current-turn' WHERE session_id=$1", env.session); err != nil {
		t.Fatal(err)
	}
	otherChat := telegramUpdate(env, 10)
	otherChat.Action, otherChat.Target, otherChat.ChatID, otherChat.UserID = "connect", env.session.String(), 444, 11
	if _, err := env.store.AcceptTelegram(ctx, otherChat); err != nil {
		t.Fatal(err)
	}
	otherChat.UpdateID, otherChat.Action, otherChat.Text = 11, "multisession", "on"
	if _, err := env.store.AcceptTelegram(ctx, otherChat); err != nil {
		t.Fatal(err)
	}
	accepted := acceptModelEventCommand(t, env)
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	acceptModeUpdate(t, env, 3, "connect", other.String(), "")
	// A picker alone is sufficient to deliver the response. It must never
	// borrow a later chat selection or change the currently running turn.
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, State: "completed", ModelMenu: modelEventMenu()})
	for range 2 {
		if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var chat, topic int64
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*), min(chat_id), min(message_thread_id) FROM telegram_deliveries WHERE event_id=$1`, event.ID).Scan(&count, &chat, &topic); err != nil || count != 1 || chat != 20 || topic != 77 {
		t.Fatalf("private menu delivery: count=%d chat=%d topic=%d err=%v", count, chat, topic, err)
	}
	var state, turn, selected, status string
	if err := env.store.pool.QueryRow(ctx, "SELECT state, active_turn_id FROM sessions WHERE session_id=$1", env.session).Scan(&state, &turn); err != nil || state != "waiting_input" || turn != "current-turn" {
		t.Fatalf("menu changed execution: %q %q %v", state, turn, err)
	}
	if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE chat_id=20").Scan(&selected); err != nil || selected != other.String() {
		t.Fatalf("menu changed selection: %q %v", selected, err)
	}
	if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1", accepted.CommandID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("menu command outcome: %q %v", status, err)
	}
}

func TestModelMenuRejectsInjectedResultsIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	accepted := acceptModelEventCommand(t, env)
	statusCommand := acceptModeUpdate(t, env, 3, "codex", "status", "")
	other := uuid.New()
	insertRouteSession(t, env, other, "another-thread", "")
	for _, scenario := range []string{
		"missing command", "unknown command", "wrong command", "wrong session", "wrong generation",
		"turn event", "failure event", "session snapshot", "history page", "workspace page", "error",
		"active turn", "running state", "empty menu", "duplicate option", "malformed menu", "permissions menu",
	} {
		t.Run(scenario, func(t *testing.T) {
			result := protocol.Result{CommandID: accepted.CommandID, State: "completed", ModelMenu: modelEventMenu()}
			kind := "command_completed"
			switch scenario {
			case "missing command":
				result.CommandID = ""
			case "unknown command":
				result.CommandID = uuid.NewString()
			case "wrong command":
				result.CommandID = statusCommand.CommandID
			case "turn event":
				kind = "turn_completed"
			case "failure event":
				kind = "command_failed"
			case "session snapshot":
				result.Session = &protocol.Session{ID: env.session.String(), State: "idle"}
			case "history page":
				result.History = historyTestPage()
			case "workspace page":
				result.Workspace = &protocol.WorkspacePage{}
			case "error":
				result.Error = &protocol.Error{Code: "test", Message: "failure"}
			case "active turn":
				result.TurnID = "current-turn"
			case "running state":
				result.State = "running"
			case "permissions menu":
				result.Permissions = permissionEventMenu()
			case "empty menu":
				result.ModelMenu.Options = nil
			case "duplicate option":
				result.ModelMenu.Options[1].Args = result.ModelMenu.Options[0].Args
			}
			event := historyTestEvent(t, env, kind, result)
			switch scenario {
			case "wrong session":
				event.SessionID = other.String()
			case "wrong generation":
				event.RuntimeGeneration = 0
			case "malformed menu":
				event.Data, _ = json.Marshal(map[string]any{"command_id": accepted.CommandID, "model_menu": "bad"})
			}
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); !errors.Is(err, ErrEventTarget) {
				t.Fatalf("injected menu accepted: %v", err)
			}
			if watermark, err := env.store.EventWatermark(ctx, env.worker); err != nil || watermark != 1 {
				t.Fatalf("rejection advanced watermark: %d %v", watermark, err)
			}
			var deliveries int
			if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id=$1", event.ID).Scan(&deliveries); err != nil || deliveries != 0 {
				t.Fatalf("rejection delivered menu: %d %v", deliveries, err)
			}
		})
	}
}

func TestModelMenuOldGenerationDoesNotDeliverIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	accepted := acceptModelEventCommand(t, env)
	if _, err := env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime); err != nil {
		t.Fatal(err)
	}
	event := historyTestEvent(t, env, "command_completed", protocol.Result{CommandID: accepted.CommandID, ModelMenu: modelEventMenu()})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id=$1", event.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("stale runtime menu was delivered: %d %v", count, err)
	}
	var status string
	if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1", accepted.CommandID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("historical command not resolved: %q %v", status, err)
	}
}
