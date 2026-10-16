package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func webUIBotTestRecord(t *testing.T, store *Store, id string, identity int64) TelegramBotRecord {
	t.Helper()
	bot := telegramBotRecordForTest(id, true)
	bot.TelegramID = identity
	bot.DisplayName = id + " · A legitimate original name"
	bot.EncryptedCredentials = []byte("PRIVATE CREDENTIALS")
	bot.LastError = "PRIVATE ERROR"
	if err := store.SaveTelegramBotRecord(t.Context(), bot); err != nil {
		t.Fatal(err)
	}
	return bot
}

func webUIBotTestSnapshot(t *testing.T, store *Store) WebUITelegramBotSnapshot {
	t.Helper()
	snapshot, err := store.WebUITelegramBots(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Revision) != 64 || snapshot.Bots == nil {
		t.Fatalf("invalid snapshot: %+v", snapshot)
	}
	return snapshot
}

func webUIPrivateBindingTestCommand(t *testing.T, env eventTestEnv, operation protocol.Operation) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	tx, err := env.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	target, err := sessionRoute(ctx, tx, env.session)
	if err != nil {
		t.Fatal(err)
	}
	args := protocol.Arguments{}
	sessionID := ""
	if operation == protocol.CodexCommand {
		args.Codex = &protocol.CodexCommandPayload{Name: "fork"}
		sessionID = env.session.String()
	} else {
		target.threadID = ""
	}
	command, err := createTelegramCommand(ctx, tx, IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 10}, target, operation, sessionID, "", args)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return uuid.MustParse(command.ID)
}

func TestWebUITelegramBotsOriginalNamesAndCanonicalVisibility(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"original", "legitimate", "empty", "managed", "disabled", "revoked", "retired"} {
		webUIBotTestRecord(t, env.store, id, int64(i+1))
	}
	for id, name := range map[string]string{"original": "Clean Original", "empty": "", "managed": "EnkiCodex · Historical session suffix"} {
		if err := env.store.SetTelegramBotSessionName(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		if err := env.store.CaptureTelegramBotOriginalName(ctx, id, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_bots SET managed_by='manager' WHERE bot_id='managed'`); err != nil {
		t.Fatal(err)
	}
	intent := TelegramManagedBotIntentRecord{ID: "linked", ManagerID: "manager", OwnerID: 10, RequestedUsername: "Enki_bot", DisplayName: "EnkiCodex", Status: "connected", BotID: "managed", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := env.store.SaveTelegramManagedBotIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_bots SET enabled=0 WHERE bot_id='disabled'`); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "revoked", []int64{20}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bot_retirements(bot_id) VALUES('retired')`); err != nil {
		t.Fatal(err)
	}
	selection := IncomingUpdate{BotID: "original", UserID: 10, ChatID: 10}
	setTestSelection(t, env.store, selection, env.session)
	// These are different routes, regardless of session sharing.
	for _, in := range []IncomingUpdate{
		{BotID: "legitimate", UserID: 10, ChatID: 20},
		{BotID: "legitimate", UserID: 10, ChatID: 10, TopicID: 1},
		{BotID: "legitimate", UserID: 11, ChatID: 10},
	} {
		setTestSelection(t, env.store, in, env.session)
	}
	snapshot := webUIBotTestSnapshot(t, env.store)
	if len(snapshot.Bots) != 4 {
		t.Fatalf("unavailable bots exposed: %+v", snapshot)
	}
	names := map[string]string{"original": "Clean Original", "legitimate": "legitimate · A legitimate original name", "empty": "", "managed": "EnkiCodex"}
	for _, bot := range snapshot.Bots {
		if bot.DisplayName != names[bot.ID] || (bot.ID == "original" && bot.SessionID != env.session.String()) || (bot.ID != "original" && bot.SessionID != "") {
			t.Fatalf("wrong public metadata: %+v", bot)
		}
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "session suffix") || strings.Contains(string(encoded), "owner_id") {
		t.Fatalf("private or inherited data exposed: %s", encoded)
	}
	if again := webUIBotTestSnapshot(t, env.store); again.Revision != snapshot.Revision {
		t.Fatal("unchanged metadata changed its revision")
	}
	for _, query := range []string{`UPDATE sessions SET archived=1`, `UPDATE sessions SET archived=0; UPDATE workers SET enabled=0`} {
		if _, err := env.store.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
		hidden := webUIBotTestSnapshot(t, env.store)
		for _, bot := range hidden.Bots {
			if bot.SessionID != "" {
				t.Fatalf("hidden session exposed: %+v", bot)
			}
		}
		if hidden.Revision == snapshot.Revision {
			t.Fatal("visibility change retained metadata revision")
		}
	}
}

func TestWebUITelegramBotMovesReplacesClearsAndPreservesOtherRoutes(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	webUIBotTestRecord(t, env.store, "bot", 1)
	webUIBotTestRecord(t, env.store, "second", 2)
	owner := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 10}
	second := IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10}
	setTestSelection(t, env.store, owner, env.session)
	setTestSelection(t, env.store, second, other)
	var isolated []IncomingUpdate
	for _, bot := range []string{"bot", "second"} {
		for _, in := range []IncomingUpdate{{BotID: bot, UserID: 10, ChatID: 20}, {BotID: bot, UserID: 10, ChatID: 10, TopicID: 1}, {BotID: bot, UserID: 11, ChatID: 10}} {
			setTestSelection(t, env.store, in, env.session)
			isolated = append(isolated, in)
		}
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,multi_session) VALUES('second',10,10,1)`); err != nil {
		t.Fatal(err)
	}
	revision := testSelectionRevision(t, env.store, owner)
	stale := webUIPrivateBindingTestCommand(t, env, protocol.CodexCommand)
	// Both the displaced bot's wizard and stale connection announcement retire.
	var wizardID = uuid.NewString()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_session_wizards(wizard_id,bot_id,user_id,chat_id,revision,kind,phase,expires_at) VALUES($1,'bot',10,10,1,'new','name',$2)`, wizardID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	confirmation := uuid.New()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload) VALUES($1,'bot',10,'ui_response',$2)`, confirmation, `{"view":"selected","session_id":"`+env.session.String()+`"}`); err != nil {
		t.Fatal(err)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	snapshot, err := env.store.SetWebUITelegramBot(ctx, env.session, "second")
	if err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if _, exists := findTestSelectedSession(t, env.store, owner); exists {
		t.Fatal("previous bot retained owner-private session")
	}
	if got := testSelectedSession(t, env.store, second); got != env.session {
		t.Fatalf("bot did not move immediately: %s", got)
	}
	for _, bot := range snapshot.Bots {
		if (bot.ID == "bot" && bot.SessionID != "") || (bot.ID == "second" && bot.SessionID != env.session.String()) {
			t.Fatalf("mutation returned stale metadata: %+v", snapshot)
		}
	}
	var revoked, multi bool
	var wizards, updates int
	var frozen string
	if err := env.store.pool.QueryRow(ctx, `SELECT
        (SELECT visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1),
        (SELECT count(*) FROM telegram_session_wizards WHERE wizard_id=$2),
        (SELECT session_id FROM commands WHERE command_id=$3),
        (SELECT count(*) FROM telegram_updates),
        (SELECT multi_session FROM telegram_chat_modes WHERE bot_id='second' AND user_id=10 AND chat_id=10 AND message_thread_id=0)`, confirmation, wizardID, stale).Scan(&revoked, &wizards, &frozen, &updates, &multi); err != nil {
		t.Fatal(err)
	}
	if !revoked || wizards != 0 || frozen != env.session.String() || updates != 0 || !multi {
		t.Fatalf("superseded/frozen context wrong: revoked=%v wizards=%d frozen=%s updates=%d multi=%v", revoked, wizards, frozen, updates, multi)
	}
	queued, err := env.store.AcceptTelegram(ctx, IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10, UpdateID: 1, Text: "Use the newly selected session"})
	if err != nil || queued.CommandID == "" || queued.SessionID != env.session.String() {
		t.Fatalf("next Telegram input used a stale route: %+v %v", queued, err)
	}
	if testSelectionRevision(t, env.store, owner) <= revision {
		t.Fatal("displaced selection was not fenced")
	}
	autoBindTestSession(t, env.store, stale, other)
	if _, exists := findTestSelectedSession(t, env.store, owner); exists {
		t.Fatal("stale fork recreated displaced bot selection")
	}
	if _, err := env.store.SetWebUITelegramBot(ctx, other, ""); err != nil {
		t.Fatal(err)
	}
	if testSelectedSession(t, env.store, second) != env.session {
		t.Fatal("clear on previous session stole moved bot")
	}
	if _, err := env.store.SetWebUITelegramBot(ctx, env.session, ""); err != nil {
		t.Fatal(err)
	}
	if _, exists := findTestSelectedSession(t, env.store, second); exists {
		t.Fatal("clear retained current owner-private selection")
	}
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM commands WHERE command_id=$1`, queued.CommandID).Scan(&frozen); err != nil || frozen != env.session.String() {
		t.Fatalf("clear changed accepted command route: %s %v", frozen, err)
	}
	for _, in := range isolated {
		if got := testSelectedSession(t, env.store, in); got != env.session {
			t.Fatalf("independent route changed: %+v -> %s", in, got)
		}
	}
}

func TestWebUITelegramBotUnavailableTargetsRollback(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	bot := webUIBotTestRecord(t, env.store, "bot", 1)
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for _, sessionID := range []uuid.UUID{uuid.Nil, uuid.New()} {
		if _, err := env.store.SetWebUITelegramBot(ctx, sessionID, "bot"); !errors.Is(err, ErrTelegramTarget) {
			t.Fatalf("unavailable session accepted: %s %v", sessionID, err)
		}
	}
	if _, err := env.store.SetWebUITelegramBot(ctx, env.session, "missing"); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("missing bot accepted: %v", err)
	}
	bot.Enabled = false
	if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if _, err := env.store.SetWebUITelegramBot(ctx, env.session, "bot"); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("disabled bot accepted: %v", err)
	}
	bot.Enabled = true
	if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{20}); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if _, err := env.store.SetWebUITelegramBot(ctx, env.session, "bot"); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("revoked owner-private chat accepted: %v", err)
	}
	activityUnchanged(t, changes)
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", nil); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	for _, query := range []string{`UPDATE sessions SET archived=1`, `UPDATE sessions SET archived=0; UPDATE workers SET enabled=0`} {
		if _, err := env.store.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.SetWebUITelegramBot(ctx, env.session, "bot"); !errors.Is(err, ErrTelegramTarget) {
			t.Fatalf("hidden session accepted: %v", err)
		}
	}
	// Offline selection has the same semantics as /tgsessions.
	if _, err := env.store.pool.Exec(ctx, `UPDATE workers SET enabled=1,connectivity='offline'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SetWebUITelegramBot(ctx, env.session, "bot"); err != nil {
		t.Fatalf("offline enabled worker rejected: %v", err)
	}
	var remembered bool
	if err := env.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_chat_modes WHERE bot_id='bot' AND user_id=10 AND chat_id=10 AND message_thread_id=0)`).Scan(&remembered); err != nil || !remembered {
		t.Fatalf("fresh owner-private destination was not remembered: %v %v", remembered, err)
	}
}

func TestTelegramOwnerPrivateSelectionReplacesWebUIBotAndNotifies(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	webUIBotTestRecord(t, env.store, "bot", 1)
	webUIBotTestRecord(t, env.store, "second", 2)
	if _, err := env.store.SetWebUITelegramBot(ctx, env.session, "bot"); err != nil {
		t.Fatal(err)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	selected, err := env.store.AcceptTelegram(ctx, IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10, UpdateID: 1, Action: "select", Target: env.session.String()})
	if err != nil || selected.View != "selected" {
		t.Fatalf("Telegram selection failed: %+v %v", selected, err)
	}
	activityChanged(t, changes)
	if _, exists := findTestSelectedSession(t, env.store, IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 10}); exists {
		t.Fatal("Telegram selection retained previous bot")
	}
	if got := testSelectedSession(t, env.store, IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10}); got != env.session {
		t.Fatal("Telegram selection did not bind replacement bot")
	}
	if _, err := env.store.AcceptTelegram(ctx, IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10, UpdateID: 2, Action: "disconnect"}); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	for _, bot := range webUIBotTestSnapshot(t, env.store).Bots {
		if bot.SessionID != "" {
			t.Fatalf("Telegram disconnect not reflected: %+v", bot)
		}
	}
}

func TestAutomaticOwnerPrivateBindingUsesExclusivityAndActivityNotification(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	webUIBotTestRecord(t, env.store, "bot", 1)
	webUIBotTestRecord(t, env.store, "second", 2)
	owner := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 10}
	setTestSelection(t, env.store, owner, env.session)
	returned := uuid.New()
	insertRouteSession(t, env, returned, "auto-returned", "")
	setTestSelection(t, env.store, IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10}, returned)
	command := webUIPrivateBindingTestCommand(t, env, protocol.NewSession)
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	session := protocol.Session{ID: returned.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), ThreadID: "auto-returned", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()}
	data, _ := json.Marshal(protocol.Result{CommandID: command.String(), Session: &session})
	event := protocol.Event{ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, Seq: 2, Kind: "command_completed", Data: data, OccurredAt: time.Now().UTC()}
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if got := testSelectedSession(t, env.store, owner); got != returned {
		t.Fatalf("automatic result did not change selection: %s", got)
	}
	if _, exists := findTestSelectedSession(t, env.store, IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10}); exists {
		t.Fatal("automatic selection retained competing owner-private bot")
	}
}

func TestTelegramBotMetadataLifecycleNotifiesSessionActivity(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	changes, cancel, err := store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	bot := webUIBotTestRecord(t, store, "bot", 1)
	activityChanged(t, changes)
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	activityUnchanged(t, changes)
	bot.DisplayName = "New original · Name"
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if err := store.SetTelegramBotSessionName(ctx, "bot", true); err != nil {
		t.Fatal(err)
	}
	if err := store.CaptureTelegramBotOriginalName(ctx, "bot", "Captured original"); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if err := store.DeleteTelegramBotRecord(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	if bots := webUIBotTestSnapshot(t, store).Bots; len(bots) != 0 {
		t.Fatalf("deleted bot retained: %+v", bots)
	}
}

func TestUnauthorizedAutomaticBindingCannotDisplaceCurrentBot(t *testing.T) {
	for _, revocation := range []string{"disabled", "revoked"} {
		t.Run(revocation, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := t.Context()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			bot := webUIBotTestRecord(t, env.store, "bot", 1)
			webUIBotTestRecord(t, env.store, "second", 2)
			owner := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 10}
			setTestSelection(t, env.store, owner, env.session)
			command := webUIPrivateBindingTestCommand(t, env, protocol.NewSession)
			returned := uuid.New()
			insertRouteSession(t, env, returned, "late-unauthorized", "")
			second := IncomingUpdate{BotID: "second", UserID: 10, ChatID: 10}
			setTestSelection(t, env.store, second, returned)
			if revocation == "disabled" {
				bot.Enabled = false
				if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
					t.Fatal(err)
				}
			} else if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{20}); err != nil {
				t.Fatal(err)
			}
			autoBindTestSession(t, env.store, command, returned)
			if got := testSelectedSession(t, env.store, second); got != returned {
				t.Fatalf("late unauthorized result displaced current bot: %s", got)
			}
			if got := testSelectedSession(t, env.store, owner); got != env.session {
				t.Fatalf("late unauthorized result changed selection: %s", got)
			}
		})
	}
}
