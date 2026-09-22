package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func chatApprovalEvent(t *testing.T, env eventTestEnv, seq uint64, kind string) protocol.Event {
	t.Helper()
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: uuid.NewString(), ThreadID: "thread-1", Type: "command", Summary: "Private approval contents", State: "pending"}
	if kind == "user_input_requested" {
		approval.Type = "user_input"
		approval.Questions = []protocol.Question{{ID: "q", Prompt: "Private question contents"}}
	}
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: kind, Data: raw, OccurredAt: time.Now().UTC()}
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestChatRevocationDisablesRememberedApprovalsAndSurvivesRestart(t *testing.T) {
	for _, kind := range []string{"approval_requested", "user_input_requested"} {
		t.Run(kind, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := t.Context()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{20, 30}); err != nil {
				t.Fatal(err)
			}
			// A prior /start alone remembers both chats. Neither selects a session.
			for i, chat := range []int64{20, 30} {
				if _, err := env.store.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: int64(i), UserID: 10, ChatID: chat, Action: "help"}); err != nil {
					t.Fatal(err)
				}
			}
			if deliveries := sessionModeDeliveries(t, env.store); len(deliveries) != 0 {
				t.Fatal("unexpected event delivery before approval")
			}
			old := chatApprovalEvent(t, env, 2, kind)
			rows := claimProgress(t, env.store, 2)
			var revoked Delivery
			for _, row := range rows {
				if row.ChatID == 20 {
					revoked = row
					if _, err := env.store.PrepareDeliveryChunks(ctx, row.ID, []json.RawMessage{json.RawMessage(`{"chat_id":20,"text":"already accepted"}`), json.RawMessage(`{"chat_id":20,"text":"unsent tail"}`)}); err != nil {
						t.Fatal(err)
					}
					if err := env.store.MarkDeliveryChunkSent(ctx, row.ID, 0, 101, "", "", ""); err != nil {
						t.Fatal(err)
					}
				} else if err := env.store.MarkDeliverySent(ctx, row.ID, 102, "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate loading the tightened config during a gateway restart.
			reopened, err := Open(ctx, env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.ApplyTelegramChatAllowlist(ctx, "bot", []int64{30}); err != nil {
				t.Fatal(err)
			}
			if allowed, err := env.store.TelegramChatAllowed(ctx, "bot", 20); err != nil || allowed {
				t.Fatalf("old sender missed persisted revocation: allowed=%v err=%v", allowed, err)
			}
			if suppressed, err := reopened.SuppressTelegramDelivery(ctx, revoked.ID); err != nil || !suppressed {
				t.Fatalf("leased frozen tail escaped: suppressed=%v err=%v", suppressed, err)
			}
			fresh := chatApprovalEvent(t, env, 3, kind)
			var oldCount, newCount, approvals, sessions int
			if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE event_id=$1 AND chat_id=20`, old.ID).Scan(&oldCount); err != nil {
				t.Fatal(err)
			}
			if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE event_id=$1 AND chat_id=20`, fresh.ID).Scan(&newCount); err != nil {
				t.Fatal(err)
			}
			if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE state='pending'`).Scan(&approvals); err != nil {
				t.Fatal(err)
			}
			if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE archived=FALSE`).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if oldCount != 1 || newCount != 0 || approvals != 2 || sessions != 1 {
				t.Fatalf("revocation altered state or queued private content: old=%d new=%d approvals=%d sessions=%d", oldCount, newCount, approvals, sessions)
			}
			chunks, err := reopened.DeliveryChunks(ctx, revoked.ID)
			if err != nil || len(chunks) != 2 || !chunks[0].Sent || chunks[1].Sent {
				t.Fatalf("revocation damaged accepted checkpoints: %+v %v", chunks, err)
			}
			// Reallowing with the legacy empty-list meaning must not replay the
			// old queued approval, even when the prior sender's lease expires.
			if err := reopened.ApplyTelegramChatAllowlist(ctx, "bot", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=$1 WHERE status='sending'`, time.Now().UTC().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			ready := claimProgress(t, reopened, 1)
			if ready[0].ChatID != 30 {
				t.Fatalf("reauthorization replayed revoked delivery: %+v", ready)
			}
			var status string
			if err := reopened.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE delivery_id=$1`, revoked.ID).Scan(&status); err != nil || status != "cancelled" {
				t.Fatalf("old delivery not terminally suppressed: %q %v", status, err)
			}
			if allowed, err := reopened.TelegramChatAllowed(ctx, "bot", 999); err != nil || !allowed {
				t.Fatalf("empty allowlist no longer unrestricted: %v %v", allowed, err)
			}
		})
	}
}

func TestChatRevocationCancelsLateQueuedRepliesButAllowsDeletion(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	if err := store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{30}); err != nil {
		t.Fatal(err)
	}
	// Importing a historical checkpoint must retain its completed state.
	historical := uuid.NewString()
	if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload,status) VALUES($1,'bot',20,'ui_response','{}','sent')`, historical); err != nil {
		t.Fatal(err)
	}
	var historicalStatus string
	if err := store.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE delivery_id=$1`, historical).Scan(&historicalStatus); err != nil || historicalStatus != "sent" {
		t.Fatalf("historical checkpoint changed: %s %v", historicalStatus, err)
	}
	for _, kind := range []string{"ui_response", "question_answered", "turn_completed", "picker_cleanup"} {
		id := uuid.NewString()
		if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,message_thread_id,kind,payload) VALUES($1,'bot',20,0,$2,'{}')`, id, kind); err != nil {
			t.Fatal(err)
		}
		var status string
		var revoked bool
		if err := store.pool.QueryRow(ctx, `SELECT status,visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1`, id).Scan(&status, &revoked); err != nil {
			t.Fatal(err)
		}
		if kind == "picker_cleanup" {
			if status != "pending" || revoked {
				t.Fatal("cleanup was cancelled")
			}
		} else if status != "cancelled" || !revoked {
			t.Fatalf("late %s was not permanently suppressed: %s %v", kind, status, revoked)
		}
	}
	if err := store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{20, 30}); err != nil {
		t.Fatal(err)
	}
	rows := claimProgress(t, store, 1)
	if rows[0].Kind != "picker_cleanup" {
		t.Fatal("late private content replayed after reauthorization")
	}
}

func TestChatRevocationStopsTypingAndRetiresProgressWithoutChangingSession(t *testing.T) {
	env := progressEnv(t)
	ctx := context.Background()
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	rows := claimProgress(t, env.store, 1)
	checkpointProgress(t, env.store, rows[0], 101)
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20, TopicID: 3})
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{30}); err != nil {
		t.Fatal(err)
	}
	assertTypingTargets(t, env.store)
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 1 || due[0].ChatID != 20 || due[0].MessageID != 101 {
		t.Fatalf("revoked chat progress cleanup lost: %+v %v", due, err)
	}
	var state, binding string
	if err := env.store.pool.QueryRow(ctx, `SELECT state FROM sessions WHERE session_id=$1`, env.session).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE chat_id=20`).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	if state != "running" || binding != env.session.String() {
		t.Fatal("chat revocation mutated the running session or its selection")
	}
}
