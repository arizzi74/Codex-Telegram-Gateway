package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestTelegramTypingTargetsFollowDurableTurnLifecycleIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	selected := telegramUpdate(env, 1)
	selected.TopicID, selected.Action, selected.Target = 17, "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selected); err != nil {
		t.Fatal(err)
	}
	start := telegramUpdate(env, 2)
	start.TopicID, start.Text = 17, "review this"
	accepted, err := env.store.AcceptTelegram(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 17})

	commandID := uuid.MustParse(accepted.CommandID)
	if err := env.store.MarkDispatched(ctx, env.worker, commandID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: accepted.CommandID, Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 17})
	if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET state='failed' WHERE session_id=$1", env.session); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 17})
	if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET state='waiting_approval' WHERE session_id=$1", env.session); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store)
	if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET state='running' WHERE session_id=$1", env.session); err != nil {
		t.Fatal(err)
	}

	startedData, _ := json.Marshal(protocol.Result{CommandID: accepted.CommandID, TurnID: "turn-typing"})
	started := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		SessionID: env.session.String(), Kind: "turn_started", OccurredAt: time.Now().UTC(), Data: startedData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, started); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 17})

	otherSession := uuid.New()
	insertRouteSession(t, env, otherSession, "thread-other", "")
	reselectedElsewhere := telegramUpdate(env, 3)
	reselectedElsewhere.TopicID, reselectedElsewhere.Action, reselectedElsewhere.Target = 17, "select", otherSession.String()
	if _, err := env.store.AcceptTelegram(ctx, reselectedElsewhere); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 17})

	disconnect := telegramUpdate(env, 4)
	disconnect.TopicID, disconnect.Action = 17, "disconnect"
	if _, err := env.store.AcceptTelegram(ctx, disconnect); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store)

	reselect := telegramUpdate(env, 5)
	reselect.TopicID, reselect.Action, reselect.Target = 17, "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, reselect); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 17})

	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "approval-typing", ThreadID: "thread-1", TurnID: "turn-typing", Type: "permissions"}
	approvalData, _ := json.Marshal(approval)
	requested := protocol.Event{Seq: 3, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		SessionID: env.session.String(), Kind: "approval_requested", OccurredAt: time.Now().UTC(), Data: approvalData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, requested); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store)
}

func assertTypingTargets(t *testing.T, store *Store, want ...TelegramTypingTarget) {
	t.Helper()
	got, err := store.ListTelegramTypingTargets(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("typing targets = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("typing targets = %#v, want %#v", got, want)
		}
	}
}
