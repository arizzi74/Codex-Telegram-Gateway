package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestSessionsCallbackPaginationIntegration(t *testing.T) {
	for _, scenario := range []string{"valid", "legacy first page", "wrong user", "wrong bot", "wrong chat", "wrong topic", "stale generation", "missing runtime", "expired", "negative page", "oversized page", "page on another action"} {
		t.Run(scenario, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			selection := telegramUpdate(env, 1)
			selection.Action, selection.Target, selection.TopicID = "select", env.session.String(), 77
			if result, err := env.store.AcceptTelegram(ctx, selection); err != nil || result.SessionID != env.session.String() {
				t.Fatalf("select = %#v, %v", result, err)
			}
			page := 4
			if scenario == "legacy first page" {
				page = 0
			}
			token, err := env.store.CreateCallback(ctx, Callback{
				Action: "sessions", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 77,
				RuntimeID: env.runtime, Generation: 1, SessionPage: page, ExpiresAt: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy first page" {
				var raw []byte
				if err := env.store.pool.QueryRow(ctx, "SELECT payload FROM telegram_callbacks WHERE token=$1", token).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				if _, exists := payload["session_page"]; exists {
					t.Fatal("first-page callback should preserve the legacy payload without session_page")
				}
			}
			in := telegramUpdate(env, 2)
			in.CallbackToken, in.TopicID = token, 77
			switch scenario {
			case "wrong user":
				in.UserID++
			case "wrong bot":
				in.BotID = "another-bot"
			case "wrong chat":
				in.ChatID++
			case "wrong topic":
				in.TopicID++
			case "stale generation":
				_, err = env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime)
			case "missing runtime":
				_, err = env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.runtime_id','missing-runtime') WHERE token=$1`, token)
			case "expired":
				_, err = env.store.pool.Exec(ctx, "UPDATE telegram_callbacks SET expires_at=$2 WHERE token=$1", token, time.Now().Add(-time.Minute))
			case "negative page":
				_, err = env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.session_page',-1) WHERE token=$1`, token)
			case "oversized page":
				_, err = env.store.pool.Exec(ctx, `UPDATE telegram_callbacks SET payload=json_set(payload,'$.session_page',1000001) WHERE token=$1`, token)
			case "page on another action":
				_, err = env.store.pool.Exec(ctx, "UPDATE telegram_callbacks SET action='new' WHERE token=$1", token)
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommands(ctx, 10)
			if err != nil || len(commands) != 0 {
				t.Fatalf("pagination created commands: %#v, %v", commands, err)
			}
			var selected string
			if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=77").Scan(&selected); err != nil || selected != env.session.String() {
				t.Fatalf("pagination changed selection: %q, %v", selected, err)
			}
			if scenario != "valid" && scenario != "legacy first page" {
				if result.View != "error" || result.ErrorCode != "callback_invalid" {
					t.Fatalf("invalid page callback accepted: %#v", result)
				}
				return
			}
			if result.View != "sessions" || result.SessionPage != page || result.RuntimeID != env.runtime.String() || result.SessionID != "" || result.CommandID != "" {
				t.Fatalf("page callback lost view context: %#v", result)
			}
			var persisted []byte
			if err := env.store.pool.QueryRow(ctx, "SELECT payload FROM telegram_deliveries WHERE kind='ui_response' AND payload->>'view'='sessions'").Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			var durable AcceptResult
			if err := json.Unmarshal(persisted, &durable); err != nil || durable.SessionPage != page || durable.RuntimeID != env.runtime.String() {
				t.Fatalf("durable page = %#v, %v", durable, err)
			}
			in.UpdateID++
			if replay, err := env.store.AcceptTelegram(ctx, in); err != nil || replay.ErrorCode != "callback_invalid" {
				t.Fatalf("replayed page callback: %#v, %v", replay, err)
			}
		})
	}
}

func TestCreateCallbackSessionPageValidationIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	for _, tc := range []struct {
		action string
		page   int
		valid  bool
	}{
		{"sessions", 0, true},
		{"sessions", 1, true},
		{"sessions", 1_000_000, true},
		{"sessions", -1, false},
		{"sessions", 1_000_001, false},
		{"new", 0, true},
		{"new", 1, false},
	} {
		_, err := env.store.CreateCallback(context.Background(), Callback{
			Action: tc.action, BotID: "bot", UserID: 10, ChatID: 20,
			RuntimeID: env.runtime, Generation: 1, SessionPage: tc.page, ExpiresAt: time.Now().Add(time.Minute),
		})
		if (err == nil) != tc.valid {
			t.Errorf("action %q page %d: %v, valid=%t", tc.action, tc.page, err, tc.valid)
		}
	}
}

func TestSessionsCommandStartsAtFirstPageIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	in := telegramUpdate(env, 1)
	in.Action, in.Target = "sessions", env.runtime.String()
	result, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil || result.View != "sessions" || result.RuntimeID != env.runtime.String() || result.SessionPage != 0 {
		t.Fatalf("initial sessions view: %#v, %v", result, err)
	}
}
