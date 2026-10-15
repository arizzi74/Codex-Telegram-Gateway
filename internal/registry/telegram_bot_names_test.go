package registry

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestTelegramBotNamePreferencesPreserveOriginalAndCascadeOnRemoval(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	bot := telegramBotRecordForTest("bot", true)
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTelegramBotSessionName(ctx, "missing", true); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("unknown bot = %v", err)
	}
	if err := store.SetTelegramBotSessionName(ctx, bot.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.CaptureTelegramBotOriginalName(ctx, bot.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CaptureTelegramBotOriginalName(ctx, bot.ID, "Suffixed name"); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("original was replaceable: %v", err)
	}
	if err := store.MarkTelegramBotNamePending(ctx, bot.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTelegramBotSessionName(ctx, bot.ID, false); err != nil {
		t.Fatal(err)
	}
	bot.DisplayName = "Unrelated profile refresh"
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	prefs, err := store.TelegramBotNamePreferences(ctx)
	if err != nil || len(prefs) != 1 || prefs[0].InheritSessionName || prefs[0].OriginalDisplayName == nil || *prefs[0].OriginalDisplayName != "" || prefs[0].AppliedDisplayName != nil {
		t.Fatalf("preference was overwritten: %+v %v", prefs, err)
	}
	if err := store.SetTelegramBotAppliedName(ctx, bot.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTelegramBotRecord(ctx, bot.ID); err != nil {
		t.Fatal(err)
	}
	prefs, err = store.TelegramBotNamePreferences(ctx)
	if err != nil || len(prefs) != 0 {
		t.Fatalf("removed bot retained name state: %+v %v", prefs, err)
	}
}

func TestTelegramBotOwnerSessionNameUsesOnlyAuthorizedPrivateSelection(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	bot := telegramBotRecordForTest("bot", true)
	if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	otherSession := uuid.New()
	insertRouteSession(t, env, otherSession, "other-name-thread", "")
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET name=CASE WHEN session_id=$1 THEN 'Private name' ELSE 'Other name' END,preview='Private preview'`, env.session); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []IncomingUpdate{
		{BotID: "bot", UserID: 10, ChatID: 20},
		{BotID: "bot", UserID: 10, ChatID: 10, TopicID: 1},
		{BotID: "bot", UserID: 11, ChatID: 10},
		{BotID: "other", UserID: 10, ChatID: 10},
	} {
		setTestSelection(t, env.store, binding, otherSession)
	}
	check := func(want string) {
		t.Helper()
		if got, err := env.store.TelegramBotOwnerSessionName(ctx, "bot", 10); err != nil || got != want {
			t.Fatalf("owner selection = %q %v, want %q", got, err, want)
		}
	}
	check("")
	setTestSelection(t, env.store, IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 10}, env.session)
	check("Private name")
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{20}); err != nil {
		t.Fatal(err)
	}
	check("")
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{10, 20}); err != nil {
		t.Fatal(err)
	}
	check("Private name")
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=1 WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	check("")
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=0,name=NULL WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	check("") // The private preview must never become the public bot name.
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET name='Private name' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	check("Private name")
	if _, err := env.store.pool.Exec(ctx, `UPDATE workers SET enabled=0 WHERE worker_id=$1`, env.worker); err != nil {
		t.Fatal(err)
	}
	check("")
}
