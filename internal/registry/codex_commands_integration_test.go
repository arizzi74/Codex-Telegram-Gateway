package registry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestCodexCommandFrozenRoutingAndOutputIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	selectUpdate := telegramUpdate(env, 1)
	selectUpdate.Action, selectUpdate.Target = "connect", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selectUpdate); err != nil {
		t.Fatal(err)
	}
	otherChat := selectUpdate
	otherChat.UpdateID, otherChat.ChatID = 40, 777
	if _, err := env.store.AcceptTelegram(ctx, otherChat); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Target = "codex", "status"
	accepted, err := env.store.AcceptTelegram(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	selectUpdate.UpdateID, selectUpdate.Target = 3, other.String()
	if _, err := env.store.AcceptTelegram(ctx, selectUpdate); err != nil {
		t.Fatal(err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands: %v %v", commands, err)
	}
	command := commands[0]
	if command.Operation != protocol.CodexCommand || command.SessionID != env.session.String() || command.Arguments.Codex.Name != "status" || command.Arguments.Text != "" {
		t.Fatalf("slash command did not freeze its target: %#v", command)
	}
	result, _ := json.Marshal(protocol.Result{CommandID: accepted.CommandID, Text: "Codex session\nModel: test-model", State: "completed"})
	event := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: result}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id=$1", event.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("slash response delivery: %d %v", count, err)
	}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id=$1", event.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate slash response: %d %v", count, err)
	}
}

func TestOnlyForkMayReturnDifferentSessionIntegration(t *testing.T) {
	for _, name := range []string{"fork", "status"} {
		t.Run(name, func(t *testing.T) {
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
			in.UpdateID, in.Action, in.Target = 2, "codex", name
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			fork := protocol.Session{ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), ThreadID: "fork-thread", CWD: "/work", State: "idle", Loaded: true}
			data, _ := json.Marshal(protocol.Result{CommandID: accepted.CommandID, Session: &fork, State: "completed"})
			event := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: data}
			err = env.store.IngestEvent(ctx, env.worker, env.connection, event)
			if name == "status" {
				if !errors.Is(err, ErrEventTarget) {
					t.Fatalf("unrelated command changed session: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var selected string
			if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE bot_id='bot'").Scan(&selected); err != nil || selected != fork.ID {
				t.Fatalf("fork binding: %s %v", selected, err)
			}
		})
	}
}
