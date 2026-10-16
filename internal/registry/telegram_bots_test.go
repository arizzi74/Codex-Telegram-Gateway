package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func telegramBotRecordForTest(id string, enabled bool) TelegramBotRecord {
	now := time.Now().UTC()
	return TelegramBotRecord{ID: id, TelegramID: 222, OwnerID: 10, Username: "TestBot", DisplayName: "Test", Enabled: enabled, EncryptedCredentials: []byte{1, 2, 3}, CreatedAt: now, UpdatedAt: now}
}

func TestTelegramBotDisableRetiresCurrentAndFutureDeliveryWork(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	store := env.store
	if err := store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	bot := telegramBotRecordForTest("bot", true)
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	insert := func() uuid.UUID {
		id := uuid.New()
		if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload) VALUES($1,'bot',20,'turn_completed','{}')`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := insert()
	bot.Enabled = false
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTelegramBotWork(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	during := insert()
	bot.Enabled = true
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	after := insert()
	for _, check := range []struct {
		id   uuid.UUID
		want string
	}{{before, "cancelled"}, {during, "cancelled"}, {after, "pending"}} {
		var state string
		var revoked bool
		if err := store.pool.QueryRow(ctx, `SELECT status,visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1`, check.id).Scan(&state, &revoked); err != nil {
			t.Fatal(err)
		}
		if state != check.want || (state == "cancelled" && !revoked) {
			t.Fatalf("disabled-window content became deliverable: %s %v", state, revoked)
		}
	}
	// Both queues are also gated at INSERT, independent of processor claim timing.
	bot.Enabled = false
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_updates(bot_id,update_id,raw) VALUES('bot',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	voice := uuid.New()
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_voice_jobs(job_id,bot_id,update_id,user_id,chat_id,worker_id,runtime_id,runtime_generation,session_id,thread_id,command_ttl_ns,file_id,expires_at) VALUES($1,'bot',1,10,20,$2,$3,1,$4,'thread-1',1,'private-file',$5)`, voice, env.worker, env.runtime, env.session, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	speech := uuid.New()
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_speech_jobs(job_id,delivery_id,worker_id,runtime_id,runtime_generation,session_id,turn_id,bot_id,chat_id,user_id,expires_at) VALUES($1,$2,$3,$4,1,$5,'turn','bot',20,10,$6)`, speech, after, env.worker, env.runtime, env.session, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var voiceState, speechState string
	var fileGone bool
	if err := store.pool.QueryRow(ctx, `SELECT status,file_id IS NULL FROM telegram_voice_jobs WHERE job_id=$1`, voice).Scan(&voiceState, &fileGone); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT status FROM telegram_speech_jobs WHERE job_id=$1`, speech).Scan(&speechState); err != nil {
		t.Fatal(err)
	}
	if voiceState != "failed" || !fileGone || speechState != "cancelled" {
		t.Fatalf("new jobs escaped bot revocation: voice=%s fileGone=%v speech=%s", voiceState, fileGone, speechState)
	}
}

func TestTelegramBotRemoveDropsSubscriptionsAndFencesOldCommandsAfterReconnect(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	store := env.store
	if err := store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	bot := telegramBotRecordForTest("bot", true)
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	selection := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 20, TopicID: 7}
	setTestSelection(t, store, selection, env.session)
	revision := testSelectionRevision(t, store, selection)
	oldCommand := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, &revision)
	callback, err := store.CreateCallback(ctx, Callback{Action: "select_session", BotID: "bot", UserID: 10, ChatID: 20, TopicID: 7, SessionID: env.session, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id,multi_session) VALUES('bot',10,20,7,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_bindings(bot_id,user_id,chat_id,session_id) VALUES('other',10,30,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTelegramBotWork(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTelegramBotRecord(ctx, "bot"); err != nil {
		t.Fatal(err)
	}
	var contexts, bindings, callbacks, shared int
	for _, q := range []struct {
		sql string
		out *int
	}{{`SELECT count(*) FROM telegram_chat_modes WHERE bot_id='bot'`, &contexts}, {`SELECT count(*) FROM telegram_bindings WHERE bot_id='bot'`, &bindings}, {`SELECT count(*) FROM telegram_callbacks WHERE token=$1`, &callbacks}, {`SELECT count(*) FROM telegram_bindings WHERE bot_id='other'`, &shared}} {
		if err := store.pool.QueryRow(ctx, q.sql, callback).Scan(q.out); err != nil {
			t.Fatal(err)
		}
	}
	if contexts != 0 || bindings != 0 || callbacks != 0 || shared != 1 {
		t.Fatalf("remove affected wrong contexts: contexts=%d bindings=%d callbacks=%d shared=%d", contexts, bindings, callbacks, shared)
	}
	// Reconnecting the same numeric bot does not revive a prior command route.
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	data, _ := json.Marshal(protocol.Result{CommandID: oldCommand.String(), Text: "private old result"})
	if _, err := store.pool.Exec(ctx, `INSERT INTO events(event_id,worker_id,runtime_id,runtime_generation,session_id,event_seq,kind,payload,occurred_at) VALUES($1,$2,$3,1,$4,2,'command_completed',$5,$6)`, eventID, env.worker, env.runtime, env.session, json.RawMessage(data), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	delivery := uuid.New()
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,event_id,bot_id,chat_id,kind,payload) VALUES($1,$2,'bot',20,'command_completed','{}')`, delivery, eventID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE delivery_id=$1`, delivery).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" {
		t.Fatal("late pre-removal command delivery became public after reconnect")
	}
	for _, payload := range []string{`{"command_id":"` + oldCommand.String() + `","text":"expired"}`, `{"data":{"command_id":"` + oldCommand.String() + `"}}`} {
		id := uuid.New()
		if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload) VALUES($1,'bot',20,'ui_response',$2)`, id, payload); err != nil {
			t.Fatal(err)
		}
		if err := store.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE delivery_id=$1`, id).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "cancelled" {
			t.Fatal("late direct command response bypassed retirement")
		}
	}
	newSession := uuid.New()
	insertRouteSession(t, env, newSession, "thread-new-result", "")
	autoBindTestSession(t, store, oldCommand, newSession)
	if _, ok := findTestSelectedSession(t, store, selection); ok {
		t.Fatal("late command recreated the removed subscription")
	}
	var sessions int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatal("removal deleted shared sessions")
	}
}

func TestTelegramManagedBotCreationClaimRechecksExpiryAtomically(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	intent := TelegramManagedBotIntentRecord{ID: "expired", ManagerID: "manager", OwnerID: 10, RequestedUsername: "TestBot", DisplayName: "Test", LaunchURL: "https://t.me/newbot/ManagerBot/TestBot", Status: "pending", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Second)}
	if err := store.SaveTelegramManagedBotIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	bot := telegramBotRecordForTest("telegram:222", false)
	bot.ManagedBy = "manager"
	if err := store.ClaimTelegramManagedBotCreation(ctx, bot, intent.ID); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("expired intent claimed: %v", err)
	}
	records, err := store.TelegramBotRecords(ctx)
	if err != nil || len(records) != 0 {
		t.Fatal("expired claim saved credentials")
	}
	intent.ID = "valid"
	intent.ExpiresAt = now.Add(time.Hour)
	if err := store.SaveTelegramManagedBotIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimTelegramManagedBotCreation(ctx, bot, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimTelegramManagedBotCreation(ctx, bot, intent.ID); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatal("claimed intent was reusable")
	}
	intents, err := store.TelegramManagedBotIntents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, in := range intents {
		if in.ID == intent.ID {
			found = in.Status == "activating" && in.BotID == bot.ID
		}
	}
	if !found {
		t.Fatal("claim did not atomically bind the intent to its bot")
	}
}

func TestTelegramManagedBotUsernameReservationIncludesAllLocalState(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	intent := func(id, username, status string, expires time.Time) TelegramManagedBotIntentRecord {
		return TelegramManagedBotIntentRecord{ID: id, ManagerID: "another-manager", OwnerID: 10, RequestedUsername: username, DisplayName: "Test", LaunchURL: "https://t.me/", Status: status, CreatedAt: now, ExpiresAt: expires}
	}
	for _, record := range []TelegramManagedBotIntentRecord{
		intent("first", "First_bot", "pending", now.Add(time.Hour)),
		intent("activating", "Activating_bot", "activating", now.Add(-time.Hour)),
		intent("expired", "Expired_bot", "pending", now.Add(-time.Hour)),
		intent("failed", "Failed_bot", "failed", now.Add(time.Hour)),
	} {
		if err := store.SaveTelegramManagedBotIntent(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	// The first request falls outside the admin's 100-row history window.
	for i := 0; i < 101; i++ {
		record := intent(fmt.Sprintf("later-%d", i), fmt.Sprintf("later%d_bot", i), "pending", now.Add(time.Hour))
		record.CreatedAt = now.Add(time.Minute)
		if err := store.SaveTelegramManagedBotIntent(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	bot := telegramBotRecordForTest("disabled", false)
	bot.Username = "Connected_bot"
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		username string
		reserved bool
	}{{"FIRST_BOT", true}, {"activating_BOT", true}, {"connected_BOT", true}, {"Expired_bot", false}, {"Failed_bot", false}, {"Missing_bot", false}} {
		reserved, err := store.TelegramManagedBotUsernameReserved(ctx, test.username)
		if err != nil || reserved != test.reserved {
			t.Errorf("reserved(%q) = %v, %v; want %v", test.username, reserved, err, test.reserved)
		}
	}
	for _, username := range []string{"first_BOT", "activating_bot", "CONNECTED_BOT"} {
		record := intent("duplicate-"+username, username, "pending", now.Add(time.Hour))
		record.ManagerID = "different-manager"
		if err := store.SaveTelegramManagedBotIntent(ctx, record); !errors.Is(err, ErrTelegramBotUsernameReserved) {
			t.Errorf("duplicate reservation %q: %v", username, err)
		}
	}
}

func TestTelegramManagedBotUsernameReservationSerializesSeparateStores(t *testing.T) {
	store := integrationStore(t)
	other, err := Open(t.Context(), store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for i, candidate := range []*Store{store, other} {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			now := time.Now().UTC()
			results <- candidate.SaveTelegramManagedBotIntent(t.Context(), TelegramManagedBotIntentRecord{ID: fmt.Sprintf("request-%d", i), ManagerID: fmt.Sprintf("manager-%d", i), OwnerID: 10, RequestedUsername: "Same_bot", DisplayName: "Same", LaunchURL: "https://t.me/", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		}()
	}
	close(start)
	group.Wait()
	close(results)
	saved, duplicate := 0, 0
	for err := range results {
		if err == nil {
			saved++
		} else if errors.Is(err, ErrTelegramBotUsernameReserved) {
			duplicate++
		} else {
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if saved != 1 || duplicate != 1 {
		t.Fatalf("reservation race: saved=%d duplicate=%d", saved, duplicate)
	}
}

func TestTelegramManagedBotActivationPendingScopesVerifiedCreation(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	intent := TelegramManagedBotIntentRecord{ID: "creation", ManagerID: "manager", OwnerID: 10, RequestedUsername: "TestBot", DisplayName: "Test", LaunchURL: "https://t.me/", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.SaveTelegramManagedBotIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	bot := telegramBotRecordForTest("telegram:222", false)
	bot.ManagedBy = "manager"
	if err := store.ClaimTelegramManagedBotCreation(ctx, bot, intent.ID); err != nil {
		t.Fatal(err)
	}
	assertPending := func(manager string, owner, identity int64, want bool) {
		t.Helper()
		pending, err := store.TelegramManagedBotActivationPending(ctx, manager, owner, identity)
		if err != nil || pending != want {
			t.Fatalf("activation(%q,%d,%d) = %v, %v; want %v", manager, owner, identity, pending, err, want)
		}
	}
	assertPending("manager", 10, 222, false)
	bot.Enabled = true
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	assertPending("manager", 10, 222, true)
	assertPending("another-manager", 10, 222, false)
	assertPending("manager", 99, 222, false)
	assertPending("manager", 10, 333, false)
	bot.OwnerID = 99
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	assertPending("manager", 10, 222, false)
	bot.OwnerID = 10
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteTelegramManagedBotUpdate(ctx, "manager", 150, bot.ID); err != nil {
		t.Fatal(err)
	}
	assertPending("manager", 10, 222, false)
}
