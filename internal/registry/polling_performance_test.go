package registry

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Keep the live queues tiny while growing the immutable history. This models
// a long-lived gateway, where polling must not get slower after every turn.
func pollingPerformanceStore(t testing.TB, completedTurns int) *Store {
	t.Helper()
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "polling.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, query := range []string{
		`INSERT INTO workers(worker_id,name,os,arch,auth_token_hash) VALUES
          ('00000000-0000-0000-0000-000000000001','polling-worker','linux','arm64',zeroblob(32))`,
		`INSERT INTO runtimes(runtime_id,worker_id,profile_id,name,generation,state) VALUES
          ('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001','main','Runtime',1,'running')`,
		`INSERT INTO sessions(session_id,worker_id,runtime_id,codex_thread_id,name,state,active_turn_id) VALUES
          ('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002','thread','Session','running','active')`,
		`INSERT INTO telegram_bindings(bot_id,user_id,chat_id,message_thread_id,session_id)
          VALUES ('bot',1,20,3,'00000000-0000-0000-0000-000000000003')`,
	} {
		if _, err := tx.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if completedTurns > 0 {
		// Each completed turn has one sent progress message, a sent terminal
		// response, and a completed deletion. All use the same destination and
		// runtime as the live turn, so missing turn indexes cannot hide here.
		if _, err := tx.Exec(ctx, `WITH RECURSIVE history(n) AS (
            VALUES(1) UNION ALL SELECT n+1 FROM history WHERE n<$1
          ) INSERT INTO events(event_id,worker_id,runtime_id,runtime_generation,session_id,event_seq,kind,payload,occurred_at)
          SELECT printf('10000000-0000-0000-0000-%012d',n),
            '00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',1,
            '00000000-0000-0000-0000-000000000003',n,
            CASE WHEN n%2=1 THEN 'agent_progress_message' ELSE 'turn_completed' END,
            json_object('turn_id','history-'||((n+1)/2)),'2025-01-01T00:00:00.000000000Z'
          FROM history`, completedTurns*2); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_deliveries
          (delivery_id,event_id,bot_id,chat_id,message_thread_id,kind,payload,status,telegram_message_id)
          SELECT printf('20000000-0000-0000-0000-%012d',event_seq),event_id,'bot',20,3,kind,payload,'sent',event_seq
          FROM events`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_progress_messages
          (cleanup_id,delivery_id,chunk_index,bot_id,chat_id,message_thread_id,telegram_message_id,
           runtime_id,runtime_generation,session_id,turn_id,status)
          SELECT printf('30000000-0000-0000-0000-%012d',event_seq),
            printf('20000000-0000-0000-0000-%012d',event_seq),0,'bot',20,3,event_seq,
            runtime_id,runtime_generation,session_id,json_extract(payload,'$.turn_id'),'deleted'
          FROM events WHERE kind='agent_progress_message'`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func addPollingLiveProgress(t testing.TB, store *Store) {
	t.Helper()
	ctx := t.Context()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		eventID, deliveryID := uuid.NewString(), uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO events
          (event_id,worker_id,runtime_id,runtime_generation,session_id,event_seq,kind,payload,occurred_at)
          VALUES($1,'00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',1,
            '00000000-0000-0000-0000-000000000003',$2,$3,'{"turn_id":"active"}',`+sqliteNow+`)`, eventID, 1000000+i, kind); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_deliveries
          (delivery_id,event_id,bot_id,chat_id,message_thread_id,kind,payload,status,telegram_message_id)
          VALUES($1,$2,'bot',20,3,$3,'{}','sent',$4)`, deliveryID, eventID, kind, 1000000+i); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_progress_messages
          (cleanup_id,delivery_id,chunk_index,bot_id,chat_id,message_thread_id,telegram_message_id,
           runtime_id,runtime_generation,session_id,turn_id)
          VALUES($1,$2,0,'bot',20,3,$3,'00000000-0000-0000-0000-000000000002',1,
            '00000000-0000-0000-0000-000000000003','active')`, uuid.NewString(), deliveryID, 1000000+i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func pollingQueryPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.pool.Query(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan.String()
}

func TestPollingLargeHistory(t *testing.T) {
	store := pollingPerformanceStore(t, 10000)
	ctx := t.Context()
	t.Run("indexed_read_only_idle", func(t *testing.T) {
		defer func() {
			if _, err := store.pool.Exec(ctx, `PRAGMA query_only=OFF`); err != nil {
				t.Fatal(err)
			}
		}()
		testPollingIdleHistory(t, store)
	})
	t.Run("indexed_live_progress", func(t *testing.T) {
		testPollingLiveHistory(t, store)
	})
}

func testPollingIdleHistory(t *testing.T, store *Store) {
	t.Helper()
	for _, test := range []struct{ query, index string }{
		{deliveryQueueReadySQL, "telegram_deliveries_queue_idx"},
		{telegramDeletionsDueSQL, "telegram_progress_ready_idx"},
	} {
		if plan := pollingQueryPlan(t, store, test.query); !strings.Contains(plan, test.index) {
			t.Fatalf("empty queue probe does not use %s:\n%s", test.index, plan)
		}
	}
	// The idle path must finish without requesting a write transaction. A
	// timing threshold would be flaky on CI; SQLite enforces this invariant.
	if _, err := store.pool.Exec(t.Context(), `PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if rows, err := store.ClaimDeliveries(t.Context(), 100); err != nil || len(rows) != 0 {
			t.Fatalf("idle delivery poll wrote or claimed history: %+v %v", rows, err)
		}
		if rows, err := store.ClaimTelegramDeletions(t.Context(), 100); err != nil || len(rows) != 0 {
			t.Fatalf("idle cleanup poll wrote or claimed history: %+v %v", rows, err)
		}
	}
}

func testPollingLiveHistory(t *testing.T, store *Store) {
	t.Helper()
	addPollingLiveProgress(t, store)
	plan := pollingQueryPlan(t, store, claimTelegramDeletionsSQL, 100)
	for _, index := range []string{"telegram_progress_ready_idx", "events_terminal_turn_idx", "events_runtime_lifecycle_idx"} {
		if !strings.Contains(plan, index) {
			t.Fatalf("live cleanup does not use %s:\n%s", index, plan)
		}
	}
	for _, step := range strings.Split(plan, "\n") {
		// Merely choosing the terminal index is insufficient: dropping its
		// expression key scans every old turn in this session on each poll.
		if strings.Contains(step, "events_terminal_turn_idx") && !strings.Contains(step, "<expr>=?") {
			t.Fatalf("terminal lookup does not constrain the turn key: %s", step)
		}
		if strings.Contains(step, "SCAN delivery") || strings.Contains(step, "SCAN terminal") {
			t.Fatalf("live cleanup scans completed history: %s", step)
		}
	}
	for i := 0; i < 3; i++ {
		if rows, err := store.ClaimTelegramDeletions(t.Context(), 100); err != nil || len(rows) != 0 {
			t.Fatalf("historical completed turns retired live progress: %+v %v", rows, err)
		}
	}
	var count int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_progress_messages WHERE status='pending' AND turn_id='active'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("latest commentary/tool progress changed: count=%d %v", count, err)
	}
	// Explicit retirement must still claim both kinds without scanning or
	// resurrecting the already deleted historical messages.
	if _, err := store.pool.Exec(t.Context(), `UPDATE telegram_progress_messages SET retire_requested=1 WHERE status='pending'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ClaimTelegramDeletions(t.Context(), 100); err != nil || len(rows) != 2 {
		t.Fatalf("explicitly retired live messages were not claimed: %+v %v", rows, err)
	}
}

func TestPollingCancelsSupersededBackoffWithoutDueDelivery(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	old := progressEvent(t, env, 2, "agent_progress_message", "active", "")
	newer := progressEvent(t, env, 3, "agent_progress_message", "active", "")
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=$2 WHERE event_id=$1`, old.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET status='sent' WHERE event_id=$1`, newer.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err := env.store.ClaimDeliveries(ctx, 100); err != nil || len(rows) != 0 {
		t.Fatalf("superseded future delivery was claimed: %+v %v", rows, err)
	}
	var status string
	if err := env.store.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE event_id=$1`, old.ID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("superseded future delivery was not cancelled: %s %v", status, err)
	}
}

func TestPollingCancelsRevokedBackoffWithoutDueDelivery(t *testing.T) {
	store := pollingPerformanceStore(t, 0)
	ctx := t.Context()
	for _, status := range []string{"pending", "failed", "sending"} {
		if _, err := store.pool.Exec(ctx, `INSERT INTO telegram_deliveries
          (delivery_id,bot_id,chat_id,kind,payload,status,visibility_revoked,next_attempt_at)
          VALUES($1,'bot',20,'ui_response','{}',$2,1,$3)`, uuid.NewString(), status, time.Now().UTC().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := store.ClaimDeliveries(ctx, 100); err != nil || len(rows) != 0 {
		t.Fatalf("revoked future delivery was claimed: %+v %v", rows, err)
	}
	var cancelled, sending int
	if err := store.pool.QueryRow(ctx, `SELECT sum(status='cancelled'),sum(status='sending') FROM telegram_deliveries`).Scan(&cancelled, &sending); err != nil || cancelled != 2 || sending != 1 {
		t.Fatalf("backoff cancellation/live lease changed: cancelled=%d sending=%d %v", cancelled, sending, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at='2025-01-01T00:00:00.000000000Z' WHERE status='sending'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ClaimDeliveries(ctx, 100); err != nil || len(rows) != 0 {
		t.Fatalf("expired revoked send was reclaimed: %+v %v", rows, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE status='cancelled'`).Scan(&cancelled); err != nil || cancelled != 3 {
		t.Fatalf("expired revoked lease was not cancelled: %d %v", cancelled, err)
	}
}

func BenchmarkGatewayPollingHistory(b *testing.B) {
	for _, count := range []int{0, 10000} {
		b.Run(fmt.Sprintf("completed_turns_%d", count), func(b *testing.B) {
			store := pollingPerformanceStore(b, count)
			b.Run("idle_delivery", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if rows, err := store.ClaimDeliveries(b.Context(), 100); err != nil || len(rows) != 0 {
						b.Fatalf("idle delivery: %+v %v", rows, err)
					}
				}
			})
			b.Run("idle_cleanup", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if rows, err := store.ClaimTelegramDeletions(b.Context(), 100); err != nil || len(rows) != 0 {
						b.Fatalf("idle cleanup: %+v %v", rows, err)
					}
				}
			})
			addPollingLiveProgress(b, store)
			b.Run("two_live_messages", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if rows, err := store.ClaimTelegramDeletions(b.Context(), 100); err != nil || len(rows) != 0 {
						b.Fatalf("live cleanup: %+v %v", rows, err)
					}
				}
			})
		})
	}
}
