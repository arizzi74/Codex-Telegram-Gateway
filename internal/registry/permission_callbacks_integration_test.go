package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func permissionCallbackTestMenu(t *testing.T, env eventTestEnv) Callback {
	t.Helper()
	in := telegramUpdate(env, 2)
	in.Action, in.Target, in.TopicID = "codex", "permissions", 77
	result, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil || result.CommandID == "" {
		t.Fatalf("open permissions: %#v, %v", result, err)
	}
	if _, err := env.store.pool.Exec(context.Background(), "UPDATE commands SET status='completed' WHERE command_id=$1", result.CommandID); err != nil {
		t.Fatal(err)
	}
	return Callback{Action: "permissions", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77,
		SessionID: env.session, RuntimeID: env.runtime, Generation: 1, QuestionID: result.CommandID,
		Decision: "workspace", ExpiresAt: time.Now().Add(time.Minute)}
}

func TestPermissionsCallbackFrozenTargetAndScopeIntegration(t *testing.T) {
	for _, scenario := range []string{"valid", "switched selection", "disconnected", "busy", "wrong user", "wrong bot", "wrong chat", "wrong topic", "stale generation", "archived session", "expired", "changed origin", "invalid decision", "missing generation"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			menu := permissionCallbackTestMenu(t, env)
			token, err := env.store.CreateCallback(ctx, menu)
			if err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 4)
			in.CallbackToken, in.TopicID = token, 77
			selected := env.session.String()
			switch scenario {
			case "switched selection":
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "")
				selection := telegramUpdate(env, 3)
				selection.Action, selection.Target, selection.TopicID = "connect", other.String(), 77
				if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
					t.Fatal(err)
				}
				selected = other.String()
			case "disconnected":
				selection := telegramUpdate(env, 3)
				selection.Action = "disconnect"
				if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
					t.Fatal(err)
				}
				selected = ""
			case "busy":
				if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET state='running', active_turn_id='running-turn' WHERE session_id=$1", env.session); err != nil {
					t.Fatal(err)
				}
			case "wrong user":
				in.UserID++
			case "wrong bot":
				in.BotID = "other-bot"
			case "wrong chat":
				in.ChatID++
			case "wrong topic":
				in.TopicID++
			case "stale generation":
				if _, err := env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime); err != nil {
					t.Fatal(err)
				}
			case "archived session":
				if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET archived=TRUE WHERE session_id=$1", env.session); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := env.store.pool.Exec(ctx, "UPDATE telegram_callbacks SET expires_at=$2 WHERE token=$1", token, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
			case "changed origin":
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.question_id',?) WHERE token=?`, uuid.NewString(), token); err != nil {
					t.Fatal(err)
				}
			case "invalid decision":
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.decision',?) WHERE token=?`, "workspace\nother", token); err != nil {
					t.Fatal(err)
				}
			case "missing generation":
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_remove(payload,'$.generation') WHERE token=$1`, token); err != nil {
					t.Fatal(err)
				}
			}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			if err != nil {
				t.Fatal(err)
			}
			valid := scenario == "valid" || scenario == "switched selection" || scenario == "disconnected" || scenario == "busy"
			if !valid {
				if result.ErrorCode != "callback_invalid" || len(commands) != 0 {
					t.Fatalf("invalid permissions callback accepted: %#v, %#v", result, commands)
				}
				return
			}
			if result.CommandID == "" || len(commands) != 1 {
				t.Fatalf("permissions command not queued: %#v, %#v", result, commands)
			}
			command := commands[0]
			if command.Operation != protocol.CodexCommand || command.SessionID != env.session.String() || command.ThreadID != "thread-1" || command.RuntimeID != env.runtime.String() || command.RuntimeGeneration != 1 || command.Arguments.Codex == nil || command.Arguments.Codex.Name != "permissions" || command.Arguments.Codex.Args != menu.Decision || command.Arguments.Text != "" {
				t.Fatalf("permissions command lost frozen route: %#v", command)
			}
			var actualSelection string
			if err := env.store.pool.QueryRow(ctx, `SELECT COALESCE((SELECT session_id FROM telegram_bindings
                    WHERE bot_id='bot' AND user_id=10 AND chat_id=20 ORDER BY message_thread_id DESC LIMIT 1),'')`).Scan(&actualSelection); err != nil || actualSelection != selected {
				t.Fatalf("permission choice changed selection: %q, want %q, %v", actualSelection, selected, err)
			}
			// A running turn remains running until the worker evaluates the setter.
			if scenario == "busy" {
				var state, active string
				if err := env.store.pool.QueryRow(ctx, "SELECT state, active_turn_id FROM sessions WHERE session_id=$1", env.session).Scan(&state, &active); err != nil || state != "running" || active != "running-turn" {
					t.Fatalf("permission callback changed running turn: %q %q %v", state, active, err)
				}
			}
			in.UpdateID++
			replayed, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || replayed.ErrorCode != "callback_invalid" {
				t.Fatalf("permission callback replay accepted: %#v, %v", replayed, err)
			}
		})
	}
}

func TestPermissionsCallbackSiblingChoicesAreAtomicIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	menu := permissionCallbackTestMenu(t, env)
	var tokens []string
	for _, option := range []string{"read-only", "workspace"} {
		menu.Decision = option
		token, err := env.store.CreateCallback(ctx, menu)
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	var wg sync.WaitGroup
	results := make(chan AcceptResult, len(tokens))
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := telegramUpdate(env, int64(3+i))
			in.CallbackToken, in.TopicID = token, 77
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
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || queued != 1 || rejected != 1 || len(commands) != 1 {
		t.Fatalf("permissions choice race: queued=%d rejected=%d commands=%d err=%v", queued, rejected, len(commands), err)
	}
}

func TestPermissionsCallbackCreationAndRequesterIntegration(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	menu := permissionCallbackTestMenu(t, env)
	userID, err := env.store.PermissionsRequester(ctx, uuid.MustParse(menu.QuestionID))
	if err != nil || userID != menu.UserID {
		t.Fatalf("permissions requester: %d, %v", userID, err)
	}
	if _, err := env.store.PermissionsRequester(ctx, uuid.New()); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("missing permissions requester: %v", err)
	}
	for _, scenario := range []string{"missing command", "invalid command", "wrong user", "wrong bot", "wrong chat", "wrong topic", "wrong session", "wrong runtime", "wrong generation", "empty decision", "blank decision", "long decision", "control decision", "invalid utf8"} {
		t.Run(scenario, func(t *testing.T) {
			invalid := menu
			switch scenario {
			case "missing command":
				invalid.QuestionID = uuid.NewString()
			case "invalid command":
				invalid.QuestionID = "not-a-command"
			case "wrong user":
				invalid.UserID++
			case "wrong bot":
				invalid.BotID = "other-bot"
			case "wrong chat":
				invalid.ChatID++
			case "wrong topic":
				invalid.TopicID++
			case "wrong session":
				invalid.SessionID = uuid.New()
			case "wrong runtime":
				invalid.RuntimeID = uuid.New()
			case "wrong generation":
				invalid.Generation++
			case "empty decision":
				invalid.Decision = ""
			case "blank decision":
				invalid.Decision = " "
			case "long decision":
				invalid.Decision = strings.Repeat("a", 257)
			case "control decision":
				invalid.Decision = "workspace\t"
			case "invalid utf8":
				invalid.Decision = "\xff"
			}
			if token, err := env.store.CreateCallback(ctx, invalid); err == nil || token != "" {
				t.Fatalf("invalid permissions callback created: %q, %v", token, err)
			}
		})
	}
	in := telegramUpdate(env, 3)
	in.Action, in.Target, in.TopicID = "codex", "status", 77
	status, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || status.CommandID == "" {
		t.Fatalf("status command: %#v, %v", status, err)
	}
	if _, err := env.store.PermissionsRequester(ctx, uuid.MustParse(status.CommandID)); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("unrelated command requester accepted: %v", err)
	}
	menu.QuestionID = status.CommandID
	if _, err := env.store.CreateCallback(ctx, menu); !errors.Is(err, ErrCallbackInvalid) {
		t.Fatalf("unrelated command origin accepted: %v", err)
	}
}
