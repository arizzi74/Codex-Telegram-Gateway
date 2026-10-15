package registry

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestRetiredBotCommandCannotRecreateSessionBinding(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	returned := uuid.New()
	insertRouteSession(t, env, returned, "late-new-session", "")
	selection := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 20, TopicID: 7}
	setTestSelection(t, env.store, selection, env.session)
	revision := testSelectionRevision(t, env.store, selection)
	oldCommand := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, &revision)
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bot_retired_commands(bot_id,command_id) VALUES('bot',$1)`, oldCommand); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `DELETE FROM telegram_bindings WHERE bot_id='bot'`); err != nil {
		t.Fatal(err)
	}
	// Even a matching captured selection revision cannot revive removed routing.
	autoBindTestSession(t, env.store, oldCommand, returned)
	if _, exists := findTestSelectedSession(t, env.store, selection); exists {
		t.Fatal("retired command recreated the removed bot's selection")
	}
	freshCommand := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, &revision)
	autoBindTestSession(t, env.store, freshCommand, returned)
	if got := testSelectedSession(t, env.store, selection); got != returned {
		t.Fatal("retirement also blocked a new command")
	}
}

func TestDisabledBotWebhookIsDiscardedWithoutReplayAfterEnable(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	bot := telegramBotRecordForTest("bot", false)
	if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	in := IncomingUpdate{BotID: "bot", UpdateID: 21, UserID: 10, ChatID: 20, Action: "sessions", CommandTTL: time.Hour}
	if _, err := env.store.AcceptTelegram(ctx, in); !errors.Is(err, ErrTelegramBotDisabled) {
		t.Fatalf("disabled bot accepted work: %v", err)
	}
	bot.Enabled = true
	if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || !result.Duplicate {
		t.Fatalf("disabled-window input replayed: %+v %v", result, err)
	}
	var deliveries, commands, contexts int
	if err := env.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM telegram_deliveries),(SELECT count(*) FROM commands),(SELECT count(*) FROM telegram_chat_modes)`).Scan(&deliveries, &commands, &contexts); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 || commands != 0 || contexts != 0 {
		t.Fatalf("discarded update changed state: deliveries=%d commands=%d contexts=%d", deliveries, commands, contexts)
	}
}
