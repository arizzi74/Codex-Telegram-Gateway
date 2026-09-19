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

func forProgressKinds(t *testing.T, test func(t *testing.T, kind, otherKind string)) {
	t.Helper()
	for _, kinds := range [][2]string{{"agent_progress_message", "tool_progress_message"}, {"tool_progress_message", "agent_progress_message"}} {
		t.Run(kinds[0], func(t *testing.T) { test(t, kinds[0], kinds[1]) })
	}
}

func assertProgressTarget(t *testing.T, store *Store, delivery Delivery, expected int64) {
	t.Helper()
	actual, err := store.TelegramProgressTarget(context.Background(), delivery.ID)
	if err != nil || actual != expected {
		t.Fatalf("progress target: got %d want %d: %v", actual, expected, err)
	}
}

func checkpointProgress(t *testing.T, store *Store, delivery Delivery, messageID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.PrepareDeliveryChunks(ctx, delivery.ID, []json.RawMessage{json.RawMessage(`{"text":"progress"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryChunkSent(ctx, delivery.ID, 0, messageID, "", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramProgressReplacementSurvivesRestartAndCleansOnceIntegration(t *testing.T) {
	forProgressKinds(t, func(t *testing.T, kind, otherKind string) {
		env := progressEnv(t)
		ctx := context.Background()
		progressEvent(t, env, 2, otherKind, "turn-a", "")
		commentary := claimProgress(t, env.store, 1)[0]
		if err := env.store.MarkDeliverySent(ctx, commentary.ID, 100, "", "", ""); err != nil {
			t.Fatal(err)
		}
		progressEvent(t, env, 3, kind, "turn-a", "")
		first := claimProgress(t, env.store, 1)[0]
		assertProgressTarget(t, env.store, first, 0)
		checkpointProgress(t, env.store, first, 101)
		reopened, err := Open(ctx, env.store.pool.path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		progressEvent(t, env, 4, kind, "turn-a", "")
		second := claimProgress(t, reopened, 1)[0]
		assertProgressTarget(t, reopened, second, 101)
		checkpointProgress(t, reopened, second, 101)
		progressEvent(t, env, 5, kind, "turn-a", "")
		third := claimProgress(t, reopened, 1)[0]
		assertProgressTarget(t, reopened, third, 101)
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
	})
}

func TestTelegramProgressCoalescesQueuedAndFailedEventsIntegration(t *testing.T) {
	forProgressKinds(t, func(t *testing.T, kind, otherKind string) {
		env := progressEnv(t)
		ctx := context.Background()
		progressEvent(t, env, 2, kind, "turn-a", "")
		first := claimProgress(t, env.store, 1)[0]
		if err := env.store.RetryDelivery(ctx, first.ID, time.Hour, "temporary failure"); err != nil {
			t.Fatal(err)
		}
		progressEvent(t, env, 3, kind, "turn-a", "")
		latest := progressEvent(t, env, 4, kind, "turn-a", "")
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
		assertProgressTarget(t, env.store, second, 0)
		checkpointProgress(t, env.store, second, 110)
		claimProgress(t, env.store, 0)
	})
}

func TestTelegramProgressSerializesInflightReplacementsAcrossStoresIntegration(t *testing.T) {
	forProgressKinds(t, func(t *testing.T, kind, otherKind string) {
		for _, expire := range []bool{false, true} {
			t.Run(map[bool]string{false: "inflight checkpoint", true: "crashed sender"}[expire], func(t *testing.T) {
				env := progressEnv(t)
				ctx := context.Background()
				progressEvent(t, env, 2, kind, "turn-a", "")
				first := claimProgress(t, env.store, 1)[0]
				secondStore, err := Open(ctx, env.store.pool.path)
				if err != nil {
					t.Fatal(err)
				}
				defer secondStore.Close()
				progressEvent(t, env, 3, kind, "turn-a", "")
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
					checkpointProgress(t, env.store, first, 120)
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
					assertProgressTarget(t, secondStore, claimed[0], 0)
					if err := env.store.ExtendDelivery(ctx, first.ID); err == nil {
						t.Fatal("superseded lease was resurrected")
					}
				} else {
					assertProgressTarget(t, secondStore, claimed[0], 120)
				}
			})
		}
	})
}

func TestTelegramProgressReplacementScopeIntegration(t *testing.T) {
	forProgressKinds(t, func(t *testing.T, kind, otherKind string) {
		env := progressEnv(t)
		ctx := context.Background()
		if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id)
        VALUES ('other-bot',1,20,3,$1),('bot',1,21,3,$1),('bot',1,20,4,$1)`, env.session); err != nil {
			t.Fatal(err)
		}
		progressEvent(t, env, 2, kind, "turn-a", "")
		type destination struct {
			bot         string
			chat, topic int64
		}
		messages := make(map[destination]int64)
		for i, delivery := range claimProgress(t, env.store, 4) {
			assertProgressTarget(t, env.store, delivery, 0)
			messageID := int64(130 + i)
			messages[destination{delivery.BotID, delivery.ChatID, delivery.TopicID}] = messageID
			checkpointProgress(t, env.store, delivery, messageID)
		}
		progressEvent(t, env, 3, kind, "turn-a", "")
		for _, delivery := range claimProgress(t, env.store, 4) {
			messageID := messages[destination{delivery.BotID, delivery.ChatID, delivery.TopicID}]
			assertProgressTarget(t, env.store, delivery, messageID)
			checkpointProgress(t, env.store, delivery, messageID)
		}
		progressEvent(t, env, 4, kind, "turn-b", "")
		for i, delivery := range claimProgress(t, env.store, 4) {
			assertProgressTarget(t, env.store, delivery, 0)
			checkpointProgress(t, env.store, delivery, int64(140+i))
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
		progressEvent(t, other, 6, kind, "turn-a", "")
		otherDelivery := claimProgress(t, env.store, 1)[0]
		assertProgressTarget(t, env.store, otherDelivery, 0)
		checkpointProgress(t, env.store, otherDelivery, 150)
		if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{
			ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running",
		}}}); err != nil {
			t.Fatal(err)
		}
		progressEventAtGeneration(t, env, 7, 2, kind, "turn-a", "")
		for _, delivery := range claimProgress(t, env.store, 4) {
			assertProgressTarget(t, env.store, delivery, 0)
			if suppress, err := env.store.SuppressProgressDelivery(ctx, delivery.ID); err != nil || suppress {
				t.Fatalf("old generation suppressed new tool: %v: %v", suppress, err)
			}
		}
	})
}

func TestTelegramProgressDisconnectedRoutesAndTerminalCleanupIntegration(t *testing.T) {
	forProgressKinds(t, func(t *testing.T, kind, otherKind string) {
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
				progressEvent(t, env, 2, kind, "turn-a", command)
				for _, delivery := range claimProgress(t, env.store, 1) {
					checkpointProgress(t, env.store, delivery, 160)
				}
				if _, err := env.store.pool.Exec(ctx, `DELETE FROM telegram_bindings`); err != nil {
					t.Fatal(err)
				}
				progressEvent(t, env, 3, terminal, "turn-a", command)
				claimProgress(t, env.store, 0)
				deletions, err := env.store.ClaimTelegramDeletions(ctx, 100)
				if err != nil || len(deletions) != 1 {
					t.Fatalf("terminal cleanup lost frozen routes: %+v: %v", deletions, err)
				}
				progressEvent(t, env, 4, kind, "turn-a", command)
				claimProgress(t, env.store, 0)
			})
		}
	})
}

func TestTelegramProgressKeepsIndependentCommentaryAndToolSlotsIntegration(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	progressEvent(t, env, 3, "tool_progress_message", "turn-a", "")
	var commentary, tool Delivery
	for _, delivery := range claimProgress(t, env.store, 2) {
		assertProgressTarget(t, env.store, delivery, 0)
		if delivery.Kind == "agent_progress_message" {
			commentary = delivery
		} else {
			tool = delivery
		}
	}
	progressEvent(t, env, 4, "agent_progress_message", "turn-a", "")
	latest := progressEvent(t, env, 5, "agent_progress_message", "turn-a", "")
	if suppress, err := env.store.SuppressProgressDelivery(ctx, commentary.ID); err != nil || !suppress {
		t.Fatalf("older commentary remained current: %v: %v", suppress, err)
	}
	if suppress, err := env.store.SuppressProgressDelivery(ctx, tool.ID); err != nil || suppress {
		t.Fatalf("newer commentary suppressed the tool slot: %v: %v", suppress, err)
	}
	claimProgress(t, env.store, 0)
	checkpointProgress(t, env.store, commentary, 100)
	checkpointProgress(t, env.store, tool, 101)
	nextCommentary := claimProgress(t, env.store, 1)[0]
	var event protocol.Event
	if err := json.Unmarshal(nextCommentary.Payload, &event); err != nil || event.ID != latest.ID {
		t.Fatalf("commentary backlog did not coalesce: %+v: %v", event, err)
	}
	assertProgressTarget(t, env.store, nextCommentary, 100)
	progressEvent(t, env, 6, "tool_progress_message", "turn-a", "")
	// The commentary replacement is still leased; a tool replacement has
	// its own slot and can be delivered without waiting for that lease.
	nextTool := claimProgress(t, env.store, 1)[0]
	if nextTool.Kind != "tool_progress_message" {
		t.Fatalf("expected the independent tool slot: %+v", nextTool)
	}
	assertProgressTarget(t, env.store, nextTool, 101)
	checkpointProgress(t, env.store, nextTool, 101)
	checkpointProgress(t, env.store, nextCommentary, 100)
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("one of the current slots was deleted: %+v: %v", due, err)
	}
	progressEvent(t, env, 7, "turn_completed", "turn-a", "")
	final := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, final.ID, 200, "", "", ""); err != nil {
		t.Fatal(err)
	}
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 2 {
		t.Fatalf("final did not retire both slots: %+v: %v", due, err)
	}
	for _, deletion := range due {
		if deletion.MessageID != 100 && deletion.MessageID != 101 {
			t.Fatalf("unexpected cleanup message: %+v", deletion)
		}
	}
}

func TestTelegramProgressRetiresLegacyMessagesAfterNewerCheckpointIntegration(t *testing.T) {
	forProgressKinds(t, func(t *testing.T, kind, otherKind string) {
		env := progressEnv(t)
		ctx := context.Background()
		progressEvent(t, env, 2, kind, "turn-a", "")
		legacy := claimProgress(t, env.store, 1)[0]
		sendProgressChunks(t, env.store, legacy, 2) // Distinct message IDs 100 and 101.
		progressEvent(t, env, 3, otherKind, "turn-a", "")
		other := claimProgress(t, env.store, 1)[0]
		checkpointProgress(t, env.store, other, 110)
		progressEvent(t, env, 4, kind, "turn-a", "")
		latest := claimProgress(t, env.store, 1)[0]
		assertProgressTarget(t, env.store, latest, 101)
		checkpointProgress(t, env.store, latest, 101)
		reopened, err := Open(ctx, env.store.pool.path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		due, err := reopened.ClaimTelegramDeletions(ctx, 100)
		if err != nil || len(due) != 1 || due[0].MessageID != 100 {
			t.Fatalf("legacy cleanup lost current or other-kind slot: %+v: %v", due, err)
		}
		if err := reopened.MarkTelegramDeletionDone(ctx, due[0].ID); err != nil {
			t.Fatal(err)
		}
		if due, err := reopened.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
			t.Fatalf("current slots were cleaned or legacy cleanup repeated: %+v: %v", due, err)
		}
		// A replacement may have to send a fresh message if Telegram rejects
		// editing the old one. That old physical message must also be retired.
		progressEvent(t, env, 5, kind, "turn-a", "")
		replacement := claimProgress(t, reopened, 1)[0]
		assertProgressTarget(t, reopened, replacement, 101)
		checkpointProgress(t, reopened, replacement, 102)
		due, err = reopened.ClaimTelegramDeletions(ctx, 100)
		if err != nil || len(due) != 1 || due[0].MessageID != 101 {
			t.Fatalf("fallback cleanup lost current or other-kind slot: %+v: %v", due, err)
		}
	})
}
