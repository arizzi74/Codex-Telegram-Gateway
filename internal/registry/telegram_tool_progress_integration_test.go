package registry

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func assertToolTarget(t *testing.T, store *Store, delivery Delivery, expected int64) {
	t.Helper()
	actual, err := store.TelegramToolProgressTarget(context.Background(), delivery.ID)
	if err != nil || actual != expected {
		t.Fatalf("tool target: got %d want %d: %v", actual, expected, err)
	}
}

func checkpointTool(t *testing.T, store *Store, delivery Delivery, messageID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.PrepareDeliveryChunks(ctx, delivery.ID, []json.RawMessage{json.RawMessage(`{"text":"tool"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryChunkSent(ctx, delivery.ID, 0, messageID, "", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramToolProgressReplacementSurvivesRestartAndCleansOnceIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	commentary := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, commentary.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 3, "tool_progress_message", "turn-a", "")
	first := claimProgress(t, env.store, 1)[0]
	assertToolTarget(t, env.store, first, 0)
	checkpointTool(t, env.store, first, 101)
	reopened, err := Open(ctx, env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	progressEvent(t, env, 4, "tool_progress_message", "turn-a", "")
	second := claimProgress(t, reopened, 1)[0]
	assertToolTarget(t, reopened, second, 101)
	checkpointTool(t, reopened, second, 101)
	progressEvent(t, env, 5, "tool_progress_message", "turn-a", "")
	third := claimProgress(t, reopened, 1)[0]
	assertToolTarget(t, reopened, third, 101)
	// The direct, unchunked checkpoint must use the same deduplication rule.
	if err := reopened.MarkDeliverySent(ctx, third.ID, 101, "", "", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_progress_messages`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("tool edits duplicated cleanup records: count=%d: %v", count, err)
	}
	if deletions, err := reopened.ClaimTelegramDeletions(ctx, 100); err != nil || len(deletions) != 0 {
		t.Fatalf("live messages deleted: %+v: %v", deletions, err)
	}
	progressEvent(t, env, 6, "turn_completed", "turn-a", "")
	final := claimProgress(t, reopened, 1)[0]
	if _, err := reopened.PrepareDeliveryChunks(ctx, final.ID, []json.RawMessage{json.RawMessage(`{"text":"one"}`), json.RawMessage(`{"text":"two"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkDeliveryChunkSent(ctx, final.ID, 0, 200, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if deletions, err := reopened.ClaimTelegramDeletions(ctx, 100); err != nil || len(deletions) != 0 {
		t.Fatalf("partial final deleted tool: %+v: %v", deletions, err)
	}
	if err := reopened.MarkDeliveryChunkSent(ctx, final.ID, 1, 201, "", "", ""); err != nil {
		t.Fatal(err)
	}
	deletions, err := reopened.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(deletions) != 2 {
		t.Fatalf("expected commentary and one tool deletion: %+v: %v", deletions, err)
	}
	seen := make(map[int64]bool)
	for _, deletion := range deletions {
		if seen[deletion.MessageID] || (deletion.MessageID != 100 && deletion.MessageID != 101) {
			t.Fatalf("duplicate or unexpected deletion: %+v", deletion)
		}
		seen[deletion.MessageID] = true
		if err := reopened.MarkTelegramDeletionDone(ctx, deletion.ID); err != nil {
			t.Fatal(err)
		}
	}
	if deletions, err := reopened.ClaimTelegramDeletions(ctx, 100); err != nil || len(deletions) != 0 {
		t.Fatalf("tool cleanup was repeated: %+v: %v", deletions, err)
	}
}

func TestTelegramToolProgressCoalescesQueuedAndFailedEventsIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	progressEvent(t, env, 2, "tool_progress_message", "turn-a", "")
	first := claimProgress(t, env.store, 1)[0]
	if err := env.store.RetryDelivery(ctx, first.ID, time.Hour, "temporary failure"); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 3, "tool_progress_message", "turn-a", "")
	latest := progressEvent(t, env, 4, "tool_progress_message", "turn-a", "")
	second := claimProgress(t, env.store, 1)[0]
	var event protocol.Event
	if err := json.Unmarshal(second.Payload, &event); err != nil || event.ID != latest.ID {
		t.Fatalf("coalesced claim was not latest: %+v: %v", event, err)
	}
	if suppress, err := env.store.SuppressProgressDelivery(ctx, first.ID); err != nil || !suppress {
		t.Fatalf("superseded failed event not suppressed: %v: %v", suppress, err)
	}
	if suppress, err := env.store.SuppressProgressDelivery(ctx, second.ID); err != nil || suppress {
		t.Fatalf("current tool was suppressed: %v: %v", suppress, err)
	}
	var cancelled int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE status='cancelled'`).Scan(&cancelled); err != nil || cancelled != 2 {
		t.Fatalf("superseded work remains queued: %d: %v", cancelled, err)
	}
	assertToolTarget(t, env.store, second, 0)
	checkpointTool(t, env.store, second, 110)
	claimProgress(t, env.store, 0)
}

func TestTelegramToolProgressSerializesInflightReplacementsAcrossStoresIntegration(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "inflight checkpoint", true: "crashed sender"}[expire], func(t *testing.T) {
			env := progressEnv(t)
			ctx := context.Background()
			progressEvent(t, env, 2, "tool_progress_message", "turn-a", "")
			first := claimProgress(t, env.store, 1)[0]
			secondStore, err := Open(ctx, env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer secondStore.Close()
			progressEvent(t, env, 3, "tool_progress_message", "turn-a", "")
			claimProgress(t, secondStore, 0)
			if suppress, err := env.store.SuppressProgressDelivery(ctx, first.ID); err != nil || !suppress {
				t.Fatalf("inflight old tool not suppressed: %v: %v", suppress, err)
			}
			if expire {
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at='2000-01-01T00:00:00.000000000Z' WHERE delivery_id=$1`, first.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				// A Telegram send already in flight is allowed to checkpoint.
				// The next sender must reuse its message rather than send anew.
				checkpointTool(t, env.store, first, 120)
			}
			type claimResult struct {
				deliveries []Delivery
				err        error
			}
			results := make(chan claimResult, 2)
			var start sync.WaitGroup
			start.Add(1)
			for _, store := range []*Store{env.store, secondStore} {
				go func() {
					start.Wait()
					deliveries, err := store.ClaimDeliveries(ctx, 100)
					results <- claimResult{deliveries, err}
				}()
			}
			start.Done()
			var claimed []Delivery
			for range 2 {
				result := <-results
				if result.err != nil {
					t.Fatal(result.err)
				}
				claimed = append(claimed, result.deliveries...)
			}
			if len(claimed) != 1 || claimed[0].ID == first.ID {
				t.Fatalf("tool scope had competing owners: %+v", claimed)
			}
			if expire {
				assertToolTarget(t, secondStore, claimed[0], 0)
				if err := env.store.ExtendDelivery(ctx, first.ID); err == nil {
					t.Fatal("superseded lease was resurrected")
				}
			} else {
				assertToolTarget(t, secondStore, claimed[0], 120)
			}
		})
	}
}

func TestTelegramToolProgressReplacementScopeIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id)
        VALUES ('other-bot',1,20,3,$1),('bot',1,21,3,$1),('bot',1,20,4,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "tool_progress_message", "turn-a", "")
	type destination struct {
		bot         string
		chat, topic int64
	}
	messages := make(map[destination]int64)
	for i, delivery := range claimProgress(t, env.store, 4) {
		assertToolTarget(t, env.store, delivery, 0)
		messageID := int64(130 + i)
		messages[destination{delivery.BotID, delivery.ChatID, delivery.TopicID}] = messageID
		checkpointTool(t, env.store, delivery, messageID)
	}
	progressEvent(t, env, 3, "tool_progress_message", "turn-a", "")
	for _, delivery := range claimProgress(t, env.store, 4) {
		messageID := messages[destination{delivery.BotID, delivery.ChatID, delivery.TopicID}]
		assertToolTarget(t, env.store, delivery, messageID)
		checkpointTool(t, env.store, delivery, messageID)
	}
	progressEvent(t, env, 4, "tool_progress_message", "turn-b", "")
	for i, delivery := range claimProgress(t, env.store, 4) {
		assertToolTarget(t, env.store, delivery, 0)
		checkpointTool(t, env.store, delivery, int64(140+i))
	}
	other := env
	other.session = uuid.New()
	discovery := other.discovery(t)
	discovery.Seq = 5
	var session protocol.Session
	if err := json.Unmarshal(discovery.Data, &session); err != nil {
		t.Fatal(err)
	}
	session.ThreadID = "other-thread"
	discovery.Data, _ = json.Marshal(session)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, discovery); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',2,20,3,$1)`, other.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, other, 6, "tool_progress_message", "turn-a", "")
	otherDelivery := claimProgress(t, env.store, 1)[0]
	assertToolTarget(t, env.store, otherDelivery, 0)
	checkpointTool(t, env.store, otherDelivery, 150)
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{
		ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	progressEventAtGeneration(t, env, 7, 2, "tool_progress_message", "turn-a", "")
	for _, delivery := range claimProgress(t, env.store, 4) {
		assertToolTarget(t, env.store, delivery, 0)
		if suppress, err := env.store.SuppressProgressDelivery(ctx, delivery.ID); err != nil || suppress {
			t.Fatalf("old generation suppressed new tool: %v: %v", suppress, err)
		}
	}
}

func TestTelegramToolProgressFrozenRoutesAndTerminalCleanupIntegration(t *testing.T) {
	for _, terminal := range []string{"turn_completed", "turn_failed", "turn_interrupted", "runtime_failed", "runtime_stopped"} {
		t.Run(terminal, func(t *testing.T) {
			env := progressEnv(t)
			ctx := context.Background()
			command := uuid.NewString()
			if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
                (command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status,
                 telegram_bot_id,telegram_chat_id,telegram_message_thread_id)
                VALUES ($1,'telegram',$2,$3,1,$4,'start_turn','{}','pending','bot',99,4)`, command, env.worker, env.runtime, env.session); err != nil {
				t.Fatal(err)
			}
			progressEvent(t, env, 2, "tool_progress_message", "turn-a", command)
			for _, delivery := range claimProgress(t, env.store, 2) {
				checkpointTool(t, env.store, delivery, 160)
			}
			if _, err := env.store.pool.Exec(ctx, `DELETE FROM telegram_bindings`); err != nil {
				t.Fatal(err)
			}
			progressEvent(t, env, 3, terminal, "turn-a", command)
			if terminal != "runtime_stopped" {
				for _, delivery := range claimProgress(t, env.store, 2) {
					if delivery.ChatID != 20 && delivery.ChatID != 99 {
						t.Fatalf("terminal target lost frozen route: %+v", delivery)
					}
					if err := env.store.MarkDeliverySent(ctx, delivery.ID, 170, "", "", ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			deletions, err := env.store.ClaimTelegramDeletions(ctx, 100)
			if err != nil || len(deletions) != 2 {
				t.Fatalf("terminal cleanup lost frozen routes: %+v: %v", deletions, err)
			}
			progressEvent(t, env, 4, "tool_progress_message", "turn-a", command)
			late := claimProgress(t, env.store, 1)[0]
			if suppress, err := env.store.SuppressProgressDelivery(ctx, late.ID); err != nil || !suppress {
				t.Fatalf("late tool escaped terminal suppression: %v: %v", suppress, err)
			}
		})
	}
}
