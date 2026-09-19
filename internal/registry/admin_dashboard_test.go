package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestAdminDashboardVisibleSessionsAndAllowedDetailsIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	observed := time.Now().UTC().Truncate(time.Millisecond)
	stats := &protocol.SessionStats{PromptCount: adminCount(12), AssistantMessageCount: adminCount(10),
		TotalTokens: adminCount(34567), LastMessage: "The latest visible reply", LastMessageRole: "assistant",
		ObservedAt: observed, CreatedAt: &observed, LastMessageAt: &observed, HistoryComplete: true, Model: "test-model"}
	visible := adminSessionEvent(t, env, 1, env.session, "user-thread", false, stats)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, visible); err != nil {
		t.Fatal(err)
	}
	archivedID := uuid.New()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection,
		adminSessionEvent(t, env, 2, archivedID, "retired-helper", true, nil)); err != nil {
		t.Fatal(err)
	}
	// Keep private metadata and raw payloads in their existing durable records.
	// None belongs to the admin projection, even under an unexpected JSON key.
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET metadata=json_set(metadata,
        '$.private', 'SESSION_PRIVATE_SENTINEL', '$.stats.extra', 'STATS_PRIVATE_SENTINEL') WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE workers SET heartbeat_metadata='{"private":"WORKER_PRIVATE_SENTINEL"}' WHERE worker_id=$1`, env.worker); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE runtimes SET codex_version='1.2.3', metadata='{"private":"RUNTIME_PRIVATE_SENTINEL"}' WHERE runtime_id=$1`, env.runtime); err != nil {
		t.Fatal(err)
	}
	var responseCommand uuid.UUID
	for _, status := range []string{"pending", "dispatched", "acknowledged", "completed", "failed", "expired", "outcome_unknown"} {
		id := uuid.New()
		if status == "acknowledged" {
			responseCommand = id
		}
		if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
            (command_id, source, worker_id, runtime_id, runtime_generation, session_id, operation, payload, status)
            VALUES($1,'telegram',$2,$3,1,$4,'start_turn','{"secret":"COMMAND_PRIVATE_SENTINEL","image":"data:image/png;base64,PRIVATE_IMAGE"}',$5)`,
			id, env.worker, env.runtime, env.session, status); err != nil {
			t.Fatal(err)
		}
	}
	for i, state := range []string{"pending", "pending", "approved"} {
		var response any
		if i == 1 {
			response = responseCommand
		}
		if _, err := env.store.pool.Exec(ctx, `INSERT INTO approvals
            (approval_id,worker_id,runtime_id,runtime_generation,session_id,codex_request_id,codex_thread_id,
             approval_type,request_payload,state,requested_at,response_command_id)
            VALUES($1,$2,$3,1,$4,$5,'user-thread','permissions','{"secret":"APPROVAL_PRIVATE_SENTINEL"}',$6,$7,$8)`,
			uuid.New(), env.worker, env.runtime, env.session, fmt.Sprintf("request-%d", i), state, observed, response); err != nil {
			t.Fatal(err)
		}
	}
	dashboard, err := env.store.AdminDashboardSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Sessions) != 1 || dashboard.Sessions[0].ID != env.session.String() {
		t.Fatalf("dashboard did not exclude retired helper: %#v", dashboard.Sessions)
	}
	session := dashboard.Sessions[0]
	if session.WorkerName != "event-worker" || session.RuntimeName != "Main" || session.WorkerConnectivity != "online" || session.CodexVersion != "1.2.3" || session.RuntimeGeneration != 1 {
		t.Fatalf("missing operational session details: %#v", session)
	}
	if session.LastSeen == nil || session.DiscoveredAt.IsZero() || session.LastEventAt == nil || !session.LastEventAt.Equal(visible.OccurredAt) {
		t.Fatalf("missing activity timestamps: %#v", session)
	}
	if session.PendingApprovals != 1 || session.QueuedCommands != 3 || dashboard.PendingApprovals != 1 || dashboard.QueuedCommands != 3 {
		t.Fatalf("incorrect pending work counts: session=%#v dashboard=%#v", session, dashboard)
	}
	if session.Stats == nil || session.Stats.PromptCount == nil || *session.Stats.PromptCount != 12 || session.Stats.TotalTokens == nil || *session.Stats.TotalTokens != 34567 || session.Stats.LastMessage != stats.LastMessage {
		t.Fatalf("lost saved conversation metrics: %#v", session.Stats)
	}
	encoded, err := json.Marshal(dashboard)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PRIVATE_SENTINEL", "PRIVATE_IMAGE", "HeartbeatMetadata", "payload", "auth_token", "retired-helper"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("dashboard exposed %q", forbidden)
		}
	}
	all, err := env.store.SessionSnapshot(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("dashboard filtering removed reconciliation history: count=%d err=%v", len(all), err)
	}
}

func TestSessionStatisticsSurviveReplayAndLegacySnapshotsIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	observed := time.Now().UTC().Truncate(time.Millisecond)
	first := adminSessionEvent(t, env, 1, env.session, "user-thread", false,
		&protocol.SessionStats{PromptCount: adminCount(7), ObservedAt: observed, ActiveSince: &observed, HistoryComplete: true})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, first); err != nil {
		t.Fatal(err)
	}
	assertCount := func(want int64) {
		t.Helper()
		sessions, err := env.store.SessionSnapshot(ctx)
		if err != nil || len(sessions) != 1 || sessions[0].Stats == nil || sessions[0].Stats.PromptCount == nil || *sessions[0].Stats.PromptCount != want {
			t.Fatalf("stats prompt count want=%d: sessions=%#v err=%v", want, sessions, err)
		}
	}
	assertCount(7)
	// A state transition produced by an older worker has no stats field.
	legacy := adminSessionEvent(t, env, 2, env.session, "user-thread", false, nil)
	legacy.Kind = "session_state_changed"
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, legacy); err != nil {
		t.Fatal(err)
	}
	assertCount(7)
	dashboard, err := env.store.AdminDashboardSnapshot(ctx)
	if err != nil || len(dashboard.Sessions) != 1 || dashboard.Sessions[0].Stats.ActiveSince != nil {
		t.Fatalf("idle dashboard session retained old active duration: %#v, %v", dashboard.Sessions, err)
	}
	stale := adminSessionEvent(t, env, 3, env.session, "user-thread", false,
		&protocol.SessionStats{PromptCount: adminCount(1), ObservedAt: observed.Add(-time.Minute)})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, stale); err != nil {
		t.Fatal(err)
	}
	assertCount(7)
	fresh := adminSessionEvent(t, env, 4, env.session, "user-thread", false,
		&protocol.SessionStats{PromptCount: adminCount(9), ObservedAt: observed.Add(time.Minute), HistoryComplete: true})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, fresh); err != nil {
		t.Fatal(err)
	}
	assertCount(9)
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Stats.ActiveSince != nil {
		t.Fatalf("finished turn retained stale active-since timestamp: %#v, %v", sessions, err)
	}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, first); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	assertCount(9)
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{
		ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	historical := adminSessionEvent(t, env, 5, env.session, "user-thread", false,
		&protocol.SessionStats{PromptCount: adminCount(100), ObservedAt: observed.Add(time.Hour)})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, historical); err != nil {
		t.Fatal(err)
	}
	assertCount(9)
}

func TestAdminSessionActivityLookupUsesCoveringIndexIntegration(t *testing.T) {
	store := integrationStore(t)
	rows, err := store.pool.Query(context.Background(), "EXPLAIN QUERY PLAN "+adminSessionQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH e USING COVERING INDEX events_session_activity_idx") {
			indexed = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("dashboard activity lookup would scan historical event payload rows")
	}
}

func adminCount(value int64) *int64 { return &value }

func adminSessionEvent(t *testing.T, env eventTestEnv, seq uint64, id uuid.UUID, thread string, archived bool, stats *protocol.SessionStats) protocol.Event {
	t.Helper()
	session := protocol.Session{ID: id.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		ThreadID: thread, Name: thread, CWD: "/workspace/project", State: "idle", Loaded: !archived,
		Archived: archived, UpdatedAt: time.Now().UTC(), Stats: stats}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{Seq: seq, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: id.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: data}
}
