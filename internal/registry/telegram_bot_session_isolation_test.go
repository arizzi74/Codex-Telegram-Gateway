package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func acceptBotSessionUpdate(t *testing.T, env eventTestEnv, bot string, id int64, action, target, text string) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, id)
	in.BotID, in.Action, in.Target, in.Text = bot, action, target, text
	result, err := env.store.AcceptTelegram(t.Context(), in)
	if err != nil || result.ErrorCode != "" || result.Duplicate {
		t.Fatalf("accept %s for %s: %+v %v", action, bot, result, err)
	}
	return result
}

func TestTelegramBotsSelectAndRouteIndependentSessionsIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-other-bot", "")
	selections := []struct {
		bot     string
		session uuid.UUID
	}{{"bot", env.session}, {"telegram:2", other}}

	// /tgsessions uses the same user/chat and update number on both bots.
	// Opening a picker must neither copy another bot's selection nor select a
	// default conversation before the user chooses one.
	for _, selection := range selections {
		result := acceptBotSessionUpdate(t, env, selection.bot, 1, "sessions", "", "")
		if result.View != "sessions" || result.RuntimeID != env.runtime.String() {
			t.Fatalf("session picker for %s: %+v", selection.bot, result)
		}
		if _, exists := findTestSelectedSession(t, env.store, IncomingUpdate{BotID: selection.bot, UserID: 10, ChatID: 20}); exists {
			t.Fatalf("opening %s's picker selected a conversation", selection.bot)
		}
	}

	for _, selection := range selections {
		token, err := env.store.CreateCallback(ctx, Callback{Action: "select", BotID: selection.bot, UserID: 10, ChatID: 20,
			SessionID: selection.session, RuntimeID: env.runtime, Generation: 1, ExpiresAt: time.Now().UTC().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		in := telegramUpdate(env, 2)
		in.BotID, in.CallbackToken = selection.bot, token
		result, err := env.store.AcceptTelegram(ctx, in)
		if err != nil || result.View != "selected" || result.SessionID != selection.session.String() {
			t.Fatalf("select for %s: %+v %v", selection.bot, result, err)
		}
	}

	for _, selection := range selections {
		in := IncomingUpdate{BotID: selection.bot, UserID: 10, ChatID: 20}
		if got := testSelectedSession(t, env.store, in); got != selection.session {
			t.Fatalf("%s's selection changed through the other bot: %s", selection.bot, got)
		}
		result := acceptBotSessionUpdate(t, env, selection.bot, 3, "text", "", "prompt for "+selection.bot)
		if result.SessionID != selection.session.String() {
			t.Fatalf("ordinary prompt for %s routed to %s", selection.bot, result.SessionID)
		}
		var bot, session string
		if err := env.store.pool.QueryRow(ctx, `SELECT telegram_bot_id,session_id FROM commands WHERE command_id=$1`, result.CommandID).Scan(&bot, &session); err != nil {
			t.Fatal(err)
		}
		if bot != selection.bot || session != selection.session.String() {
			t.Fatalf("frozen route for %s: bot=%s session=%s", selection.bot, bot, session)
		}
	}

	// A token offered to the wrong bot cannot change either selection or burn
	// the token before the intended bot receives it.
	token, err := env.store.CreateCallback(ctx, Callback{Action: "select", BotID: "telegram:2", UserID: 10, ChatID: 20,
		SessionID: env.session, RuntimeID: env.runtime, Generation: 1, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 4)
	in.CallbackToken = token
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.ErrorCode != "callback_invalid" {
		t.Fatalf("cross-bot callback accepted: %+v %v", result, err)
	}
	in.BotID = "telegram:2"
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.View != "selected" {
		t.Fatalf("wrong-bot attempt consumed intended callback: %+v %v", result, err)
	}
	if revision := testSelectionRevision(t, env.store, IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 20}); revision != 1 {
		t.Fatalf("other bot's selection changed primary revision: %d", revision)
	}
}

func TestTelegramBotsIsolateFinalAndProgressDeliveriesIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := env
	other.session = uuid.New()
	insertRouteSession(t, env, other.session, "thread-other-bot", "")
	acceptBotSessionUpdate(t, env, "bot", 1, "select", env.session.String(), "")
	acceptBotSessionUpdate(t, env, "telegram:2", 1, "select", other.session.String(), "")
	if rows := sessionModeDeliveries(t, env.store); len(rows) != 0 {
		t.Fatalf("unexpected selection deliveries: %+v", rows)
	}
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, other, 3, "turn_started", "turn-b", "")
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20}, TelegramTypingTarget{BotID: "telegram:2", ChatID: 20})

	seq := uint64(4)
	for _, kind := range []string{"agent_progress_message", "tool_progress_message", "turn_completed"} {
		for _, selection := range []struct {
			bot, turn string
			env       eventTestEnv
		}{{"bot", "turn-a", env}, {"telegram:2", "turn-b", other}} {
			progressEvent(t, selection.env, seq, kind, selection.turn, "")
			seq++
			rows := sessionModeDeliveries(t, env.store)
			if len(rows) != 1 || rows[0].BotID != selection.bot || rows[0].Kind != kind {
				t.Fatalf("%s from %s crossed bot selections: %+v", kind, selection.bot, rows)
			}
			var event protocol.Event
			if err := json.Unmarshal(rows[0].Payload, &event); err != nil || event.SessionID != selection.env.session.String() {
				t.Fatalf("wrong event route: %+v %v", event, err)
			}
			if suppressed, err := env.store.SuppressTelegramDelivery(ctx, rows[0].ID); err != nil || suppressed {
				t.Fatalf("selected %s output suppressed: %v %v", selection.bot, suppressed, err)
			}
			if err := env.store.MarkDeliverySent(ctx, rows[0].ID, int64(seq), "", "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertTypingTargets(t, env.store)
}

func TestTelegramBotSelectionRetiresOnlyItsOwnProgressIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-other-bot", "")
	for _, bot := range []string{"bot", "telegram:2"} {
		acceptBotSessionUpdate(t, env, bot, 1, "select", env.session.String(), "")
	}
	sessionModeDeliveries(t, env.store)
	progressEvent(t, env, 2, "turn_started", "shared-turn", "")
	progressEvent(t, env, 3, "agent_progress_message", "shared-turn", "")
	rows := sessionModeDeliveries(t, env.store)
	if len(rows) != 2 {
		t.Fatalf("shared session did not reach both explicitly subscribed bots: %+v", rows)
	}
	for _, row := range rows {
		// Telegram message numbers may collide across two private bot chats.
		checkpointProgress(t, env.store, row, 100)
	}
	progressEvent(t, env, 4, "agent_progress_message", "shared-turn", "")
	rows = sessionModeDeliveries(t, env.store)
	if len(rows) != 2 {
		t.Fatalf("progress replacement crossed bot leases: %+v", rows)
	}
	for _, row := range rows {
		assertProgressTarget(t, env.store, row, 100)
		checkpointProgress(t, env.store, row, 100)
	}
	acceptBotSessionUpdate(t, env, "telegram:2", 2, "select", other.String(), "")
	sessionModeDeliveries(t, env.store)
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 1 || due[0].BotID != "telegram:2" || due[0].MessageID != 100 {
		t.Fatalf("one bot's switch retired another bot's progress: %+v %v", due, err)
	}
	if err := env.store.MarkTelegramDeletionDone(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 5, "agent_progress_message", "shared-turn", "")
	rows = sessionModeDeliveries(t, env.store)
	if len(rows) != 1 || rows[0].BotID != "bot" {
		t.Fatalf("progress after independent switch: %+v", rows)
	}
	assertProgressTarget(t, env.store, rows[0], 100)
	checkpointProgress(t, env.store, rows[0], 100)
	progressEvent(t, env, 6, "turn_completed", "shared-turn", "")
	rows = sessionModeDeliveries(t, env.store)
	if len(rows) != 1 || rows[0].BotID != "bot" || rows[0].Kind != "turn_completed" {
		t.Fatalf("final after independent switch: %+v", rows)
	}
}
