package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func progressEvent(t *testing.T, env eventTestEnv, seq uint64, kind, turn string, commandID string) protocol.Event {
	t.Helper()
	return progressEventAtGeneration(t, env, seq, 1, kind, turn, commandID)
}

func progressEventAtGeneration(t *testing.T, env eventTestEnv, seq, generation uint64, kind, turn string, commandID string) protocol.Event {
	t.Helper()
	data, err := json.Marshal(protocol.Result{CommandID: commandID, TurnID: turn, Text: kind + " text"})
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: generation,
		SessionID: env.session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func progressEnv(t *testing.T) eventTestEnv {
	t.Helper()
	env := newEventTestEnv(t)
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(context.Background(), `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',1,20,3,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	return env
}

func claimProgress(t *testing.T, store *Store, count int) []Delivery {
	t.Helper()
	deliveries, err := store.ClaimDeliveries(context.Background(), 100)
	if err != nil || len(deliveries) != count {
		t.Fatalf("deliveries: got %d want %d: %v", len(deliveries), count, err)
	}
	return deliveries
}

func sendProgressChunks(t *testing.T, store *Store, delivery Delivery, count int) {
	t.Helper()
	chunks := make([]json.RawMessage, count)
	for i := range chunks {
		chunks[i] = json.RawMessage(`{"text":"temporary"}`)
	}
	if _, err := store.PrepareDeliveryChunks(context.Background(), delivery.ID, chunks); err != nil {
		t.Fatal(err)
	}
	for i := range chunks {
		if err := store.MarkDeliveryChunkSent(context.Background(), delivery.ID, i, 100+int64(i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTelegramProgressDeletesOnlyAfterAllFinalChunksAndRecoversCleanupIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	delivery := claimProgress(t, env.store, 1)[0]
	if suppress, err := env.store.SuppressProgressDelivery(ctx, delivery.ID); err != nil || suppress {
		t.Fatalf("live progress suppressed: %v %v", suppress, err)
	}
	sendProgressChunks(t, env.store, delivery, 2)
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("live progress deleted: %v %v", due, err)
	}
	progressEvent(t, env, 3, "turn_completed", "turn-a", "")
	final := claimProgress(t, env.store, 1)[0]
	if _, err := env.store.PrepareDeliveryChunks(ctx, final.ID, []json.RawMessage{json.RawMessage(`{"text":"final1"}`), json.RawMessage(`{"text":"final2"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, final.ID, 0, 200, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("partial final deleted progress: %v %v", due, err)
	}
	// A worker restart while the final is only partly delivered must not
	// bypass its durable checkpoints and remove progress prematurely.
	progressEvent(t, env, 4, "runtime_stopped", "", "")
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("runtime stop bypassed pending final: %v %v", due, err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, final.ID, 1, 201, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// A new Store has no memory of sends. Persisted checkpoints and terminal
	// delivery state must be sufficient to recover every pending deletion.
	reopened, err := Open(ctx, env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	due, err := reopened.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 2 {
		t.Fatalf("cleanup after final: %v %v", due, err)
	}
	for _, deletion := range due {
		if deletion.BotID != "bot" || deletion.ChatID != 20 || deletion.TopicID != 3 || deletion.Attempt != 1 {
			t.Fatalf("cleanup route changed: %+v", deletion)
		}
	}
	if err := reopened.RetryTelegramDeletion(ctx, due[0].ID, time.Minute, "temporary Telegram failure"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkTelegramDeletionDone(ctx, due[1].ID); err != nil {
		t.Fatal(err)
	}
	if more, err := reopened.ClaimTelegramDeletions(ctx, 100); err != nil || len(more) != 0 {
		t.Fatalf("cleanup ignored retry delay: %v %v", more, err)
	}
	if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_progress_messages SET next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-1 second') || '000000Z') WHERE cleanup_id=$1`, due[0].ID); err != nil {
		t.Fatal(err)
	}
	retry, err := reopened.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(retry) != 1 || retry[0].ID != due[0].ID || retry[0].Attempt != 2 {
		t.Fatalf("cleanup retry: %v %v", retry, err)
	}
	// A crashed deletion worker's lease is recoverable independently of final sends.
	if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_progress_messages SET next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-1 second') || '000000Z') WHERE cleanup_id=$1`, retry[0].ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := reopened.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(recovered) != 1 || recovered[0].Attempt != 3 {
		t.Fatalf("cleanup lease recovery: %v %v", recovered, err)
	}
	if err := reopened.MarkTelegramDeletionDone(ctx, recovered[0].ID); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, reopened, 0)
}

func TestTelegramProgressSuppressesLateRetriesAndCleansInFlightSendIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	progress := claimProgress(t, env.store, 1)[0]
	if _, err := env.store.PrepareDeliveryChunks(ctx, progress.ID, []json.RawMessage{json.RawMessage(`{"text":"first"}`), json.RawMessage(`{"text":"late"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, progress.ID, 0, 301, "", "", ""); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 3, "turn_completed", "turn-a", "")
	if suppress, err := env.store.SuppressProgressDelivery(ctx, progress.ID); err != nil || !suppress {
		t.Fatalf("terminal progress not suppressed: %v %v", suppress, err)
	}
	final := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, final.ID, 400, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// The Telegram call for chunk 2 may already have been in flight when the
	// final landed. Its late checkpoint must join the same cleanup queue.
	if err := env.store.MarkDeliveryChunkSent(ctx, progress.ID, 1, 302, "", "", ""); err != nil {
		t.Fatal(err)
	}
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 2 {
		t.Fatalf("late send cleanup: %v %v", due, err)
	}
	progressEvent(t, env, 4, "agent_progress_message", "turn-a", "")
	late := claimProgress(t, env.store, 1)[0]
	if suppress, err := env.store.SuppressProgressDelivery(ctx, late.ID); err != nil || !suppress {
		t.Fatalf("late event not suppressed: %v %v", suppress, err)
	}
	if err := env.store.SkipDelivery(ctx, late.ID); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, env.store, 0)
}

func TestTelegramProgressFinalRoutingIncludesFrozenOriginAndOldBindingsIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	command := uuid.NewString()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
        (command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status,
         telegram_bot_id,telegram_chat_id,telegram_message_thread_id)
        VALUES ($1,'telegram',$2,$3,1,$4,'start_turn','{}','pending','bot',99,4)`, command, env.worker, env.runtime, env.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", command)
	progress := claimProgress(t, env.store, 2)
	for _, delivery := range progress {
		sendProgressChunks(t, env.store, delivery, 1)
	}
	if _, err := env.store.pool.Exec(ctx, `DELETE FROM telegram_bindings`); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 3, "turn_completed", "turn-a", command)
	finals := claimProgress(t, env.store, 2)
	for _, delivery := range finals {
		if delivery.ChatID != 99 && delivery.ChatID != 20 {
			t.Fatalf("wrong final route: %+v", delivery)
		}
		if err := env.store.MarkDeliverySent(ctx, delivery.ID, 501, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 2 {
		t.Fatalf("old binding cleanup: %v %v", due, err)
	}
}

func TestTelegramProgressScopesCleanupByDestinationAndTurnIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',1,20,4,$1),('other-bot',1,20,3,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	for _, delivery := range claimProgress(t, env.store, 3) {
		sendProgressChunks(t, env.store, delivery, 1)
	}
	progressEvent(t, env, 3, "agent_progress_message", "turn-b", "")
	for _, delivery := range claimProgress(t, env.store, 3) {
		sendProgressChunks(t, env.store, delivery, 1)
	}
	progressEvent(t, env, 4, "turn_completed", "turn-a", "")
	finals := claimProgress(t, env.store, 3)
	for _, delivery := range finals {
		if delivery.BotID == "bot" && delivery.TopicID == 3 {
			if err := env.store.MarkDeliverySent(ctx, delivery.ID, 600, "", "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 1 || due[0].BotID != "bot" || due[0].TopicID != 3 {
		t.Fatalf("cleanup crossed bot/topic/turn: %v %v", due, err)
	}
	if more, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(more) != 0 {
		t.Fatalf("cleanup crossed bot/topic/turn: %v %v", more, err)
	}
}

func TestTelegramProgressOldCompletionDoesNotDeleteCurrentGenerationIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{
		ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	progressEventAtGeneration(t, env, 2, 2, "agent_progress_message", "reused-turn", "")
	current := claimProgress(t, env.store, 1)[0]
	sendProgressChunks(t, env.store, current, 1)
	progressEvent(t, env, 3, "turn_completed", "reused-turn", "")
	oldFinal := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, oldFinal.ID, 800, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if suppress, err := env.store.SuppressProgressDelivery(ctx, current.ID); err != nil || !suppress {
		// Fully sent parent deliveries are intentionally suppressed regardless
		// of turn state; use another current event to test the generation fence.
		t.Fatalf("sent parent status not respected: %v %v", suppress, err)
	}
	progressEventAtGeneration(t, env, 4, 2, "agent_progress_message", "reused-turn", "")
	next := claimProgress(t, env.store, 1)[0]
	if suppress, err := env.store.SuppressProgressDelivery(ctx, next.ID); err != nil || suppress {
		t.Fatalf("old terminal suppressed current progress: %v %v", suppress, err)
	}
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("old terminal deleted current generation: %v %v", due, err)
	}
}

func TestTelegramProgressCleanupDoesNotCrossSessionIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	other := env
	other.session = uuid.New()
	discovery := other.discovery(t)
	discovery.Seq = 2
	var session protocol.Session
	if err := json.Unmarshal(discovery.Data, &session); err != nil {
		t.Fatal(err)
	}
	session.ThreadID = "thread-other"
	discovery.Data, _ = json.Marshal(session)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, discovery); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',2,20,3,$1)`, other.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 3, "agent_progress_message", "same-turn", "")
	sendProgressChunks(t, env.store, claimProgress(t, env.store, 1)[0], 1)
	progressEvent(t, other, 4, "agent_progress_message", "same-turn", "")
	sendProgressChunks(t, env.store, claimProgress(t, env.store, 1)[0], 1)
	progressEvent(t, env, 5, "turn_completed", "same-turn", "")
	final := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, final.ID, 900, "", "", ""); err != nil {
		t.Fatal(err)
	}
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 1 {
		t.Fatalf("cleanup crossed session: %v %v", due, err)
	}
	var actual uuid.UUID
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_progress_messages WHERE cleanup_id=$1`, due[0].ID).Scan(&actual); err != nil || actual != env.session {
		t.Fatalf("cleanup targeted wrong session: %v %v", actual, err)
	}
}

func TestTelegramProgressCleansInterruptedFailedAndStoppedTurnsIntegration(t *testing.T) {
	for _, kind := range []string{"turn_interrupted", "turn_failed", "runtime_failed", "runtime_stopped"} {
		t.Run(kind, func(t *testing.T) {
			env := progressEnv(t)
			ctx := context.Background()
			progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
			progress := claimProgress(t, env.store, 1)[0]
			sendProgressChunks(t, env.store, progress, 1)
			progressEvent(t, env, 3, kind, "turn-a", "")
			if suppress, err := env.store.SuppressProgressDelivery(ctx, progress.ID); err != nil || !suppress {
				t.Fatalf("ended progress not suppressed: %v %v", suppress, err)
			}
			if kind != "runtime_stopped" {
				if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
					t.Fatalf("cleanup before terminal notification: %v %v", due, err)
				}
				final := claimProgress(t, env.store, 1)[0]
				if err := env.store.MarkDeliverySent(ctx, final.ID, 700, "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 1 {
				t.Fatalf("terminal cleanup: %v %v", due, err)
			}
		})
	}
}

func TestTelegramPromptHasNoQueuedUIButControlsAndErrorsRemainIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	prompt := IncomingUpdate{BotID: "bot", UserID: 1, ChatID: 20, TopicID: 3, UpdateID: 1, Text: "Do the work", Action: "text"}
	result, err := env.store.AcceptTelegram(ctx, prompt)
	if err != nil || result.CommandID == "" {
		t.Fatalf("prompt acceptance: %+v %v", result, err)
	}
	claimProgress(t, env.store, 0)
	prompt.UpdateID++
	prompt.Action, prompt.Target, prompt.Text = "codex", "status", ""
	if _, err := env.store.AcceptTelegram(ctx, prompt); err != nil {
		t.Fatal(err)
	}
	if queued := claimProgress(t, env.store, 1)[0]; queued.Kind != "ui_response" {
		t.Fatalf("control acknowledgement lost: %+v", queued)
	}
	prompt.UpdateID++
	prompt.Action, prompt.Text, prompt.TopicID, prompt.ChatID = "text", "Unbound", 0, 22
	if result, err := env.store.AcceptTelegram(ctx, prompt); err != nil || result.View != "error" {
		t.Fatalf("prompt error lost: %+v %v", result, err)
	}
	claimProgress(t, env.store, 1)
}
