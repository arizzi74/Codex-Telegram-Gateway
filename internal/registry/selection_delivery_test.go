package registry

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestSelectedConfirmationPrecedesReplayedAndLiveProgress(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	progressEvent(t, env, 4, "tool_progress_message", "turn-a", "")
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")

	// Even a batch claim cannot take the replay queued before its confirmation.
	confirmation := claimProgress(t, env.store, 1)[0]
	if confirmation.Kind != "ui_response" {
		t.Fatalf("first delivery = %s, want connection confirmation", confirmation.Kind)
	}
	claimProgress(t, env.store, 0)
	progressEvent(t, env, 5, "agent_progress_message", "turn-a", "")
	claimProgress(t, env.store, 0)
	if err := env.store.RetryDelivery(ctx, confirmation.ID, time.Hour, "Telegram unavailable"); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, env.store, 0)

	// Durable barriers survive a new sender/store while Telegram is retrying.
	reopened, err := Open(ctx, env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimProgress(t, reopened, 0)
	if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=$2 WHERE delivery_id=$1`, confirmation.ID, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	retry := claimProgress(t, reopened, 1)[0]
	if retry.ID != confirmation.ID || retry.Attempt != 2 {
		t.Fatalf("retry = %+v", retry)
	}
	if _, err := reopened.PrepareDeliveryChunks(ctx, retry.ID, []json.RawMessage{json.RawMessage(`{"text":"Connected to"}`), json.RawMessage(`{"text":"details"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkDeliveryChunkSent(ctx, retry.ID, 0, 80, "", "", ""); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, reopened, 0)
	if err := reopened.MarkDeliveryChunkSent(ctx, retry.ID, 1, 81, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, row := range claimProgress(t, reopened, 2) {
		var event protocol.Event
		if err := json.Unmarshal(row.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.SessionID != env.session.String() || (event.Seq != 4 && event.Seq != 5) {
			t.Fatalf("wrong restored/live progress: %+v", event)
		}
		if skip, err := reopened.SuppressTelegramDelivery(ctx, row.ID); skip || err != nil {
			t.Fatalf("confirmed progress still blocked: %v %v", skip, err)
		}
	}
}

func TestSelectionConfirmationFencesAlreadyLeasedProgressAndRetiresOldChoices(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-b", "")
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	leased := claimProgress(t, env.store, 1)[0]
	selectSession := IncomingUpdate{BotID: "bot", UserID: 1, ChatID: 20, TopicID: 3, UpdateID: 1, Action: "select", Target: env.session.String()}
	if _, err := env.store.AcceptTelegram(ctx, selectSession); err != nil {
		t.Fatal(err)
	}
	if skip, err := env.store.SuppressTelegramDelivery(ctx, leased.ID); skip || !errors.Is(err, ErrTelegramSelectionPending) {
		t.Fatalf("leased progress was sent/cancelled instead of deferred: %v %v", skip, err)
	}
	old := claimProgress(t, env.store, 1)[0]
	if err := env.store.RetryDelivery(ctx, old.ID, time.Hour, "Telegram unavailable"); err != nil {
		t.Fatal(err)
	}
	for i, target := range []string{other.String(), env.session.String(), other.String(), env.session.String()} {
		selectSession.UpdateID, selectSession.Target = int64(i+2), target
		if _, err := env.store.AcceptTelegram(ctx, selectSession); err != nil {
			t.Fatal(err)
		}
	}
	current := claimProgress(t, env.store, 1)[0]
	if current.Kind != "ui_response" || current.ID == old.ID {
		t.Fatalf("wrong latest confirmation: %+v", current)
	}
	if err := env.store.MarkDeliverySent(ctx, current.ID, 90, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if skip, err := env.store.SuppressTelegramDelivery(ctx, old.ID); !skip || err != nil {
		t.Fatalf("old confirmation survived rapid selections: %v %v", skip, err)
	}
	// The first leased message was irrevocably revoked by selecting B. Its
	// fresh replay for the new A selection can proceed without the old retry.
	if skip, err := env.store.SuppressTelegramDelivery(ctx, leased.ID); !skip || err != nil {
		t.Fatalf("old progress revived: %v %v", skip, err)
	}
	if err := env.store.SkipDelivery(ctx, leased.ID); err != nil {
		t.Fatal(err)
	}
	fresh := claimProgress(t, env.store, 1)[0]
	if skip, err := env.store.SuppressTelegramDelivery(ctx, fresh.ID); skip || err != nil {
		t.Fatalf("old failed confirmation blocked new selection: %v %v", skip, err)
	}
	selectSession.UpdateID, selectSession.Action = 10, "disconnect"
	if _, err := env.store.AcceptTelegram(ctx, selectSession); err != nil {
		t.Fatal(err)
	}
	if skip, err := env.store.SuppressTelegramDelivery(ctx, fresh.ID); !skip || err != nil {
		t.Fatalf("disconnect left progress eligible: %v %v", skip, err)
	}
}

func TestSelectionConfirmationDoesNotBlockOtherDestinationsOrQuestions(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	selectSession := IncomingUpdate{BotID: "bot", UserID: 1, ChatID: 20, TopicID: 3, UpdateID: 1, Action: "select", Target: env.session.String()}
	if _, err := env.store.AcceptTelegram(ctx, selectSession); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',1,20,4,$1),('other-bot',1,20,3,$1),('bot',1,21,3,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	rows := claimProgress(t, env.store, 4)
	for _, row := range rows {
		if row.Kind != "ui_response" && row.BotID == "bot" && row.ChatID == 20 && row.TopicID == 3 {
			t.Fatalf("selected destination passed its barrier: %+v", row)
		}
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "question", ThreadID: "thread-1", TurnID: "turn-a", Type: "user_input", Questions: []protocol.Question{{ID: "q", Prompt: "Continue?"}}}
	data, _ := json.Marshal(approval)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, protocol.Event{ID: uuid.NewString(), Seq: 4, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "user_input_requested", Data: data, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	questions, err := env.store.ClaimDeliveries(ctx, 100)
	if err != nil || len(questions) == 0 {
		t.Fatalf("question blocked: %+v %v", questions, err)
	}
	for _, row := range questions {
		if row.Kind != "user_input_requested" {
			t.Fatalf("progress escaped barrier: %+v", row)
		}
		if skip, err := env.store.SuppressTelegramDelivery(ctx, row.ID); skip || err != nil {
			t.Fatalf("question blocked by connection confirmation: %v %v", skip, err)
		}
	}
	// The selected session's barrier does not stall another session's feed in
	// multisession mode, even at the very same Telegram destination.
	other := env
	other.session = uuid.New()
	insertRouteSession(t, env, other.session, "thread-b", "")
	selectSession.UpdateID, selectSession.Action, selectSession.Target, selectSession.Text = 2, "multisession", "", "on"
	if _, err := env.store.AcceptTelegram(ctx, selectSession); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, other, 5, "turn_started", "turn-b", "")
	progressEvent(t, other, 6, "tool_progress_message", "turn-b", "")
	foundOther := false
	for _, row := range claimProgress(t, env.store, 2) {
		if row.Kind == "ui_response" {
			continue
		}
		var event protocol.Event
		if err := json.Unmarshal(row.Payload, &event); err != nil || event.SessionID != other.session.String() {
			t.Fatalf("wrong multisession progress: %+v %v", event, err)
		}
		if skip, err := env.store.SuppressTelegramDelivery(ctx, row.ID); skip || err != nil {
			t.Fatalf("unrelated session blocked: %v %v", skip, err)
		}
		foundOther = true
	}
	if !foundOther {
		t.Fatal("unrelated session progress not delivered")
	}
}
