package registry

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestProgressOrderSurvivesRetryRestartAndSupersedingCommentary(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	progressEvent(t, env, 2, "tool_progress_message", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	commentary := claimProgress(t, env.store, 1)[0]
	if commentary.Kind != "agent_progress_message" {
		t.Fatalf("tool overtook commentary: %+v", commentary)
	}
	if err := env.store.RetryDelivery(ctx, commentary.ID, time.Hour, "rate limited"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, env.store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimProgress(t, reopened, 0)
	// The newest commentary supersedes the failed row and its long backoff.
	latest := progressEvent(t, env, 4, "agent_progress_message", "turn-a", "")
	commentary = claimProgress(t, reopened, 1)[0]
	var event protocol.Event
	if json.Unmarshal(commentary.Payload, &event) != nil || event.ID != latest.ID {
		t.Fatalf("failed commentary was not replaced: %+v", commentary)
	}
	latestTool := progressEvent(t, env, 5, "tool_progress_message", "turn-a", "")
	claimProgress(t, reopened, 0)
	// Legacy multipart commentary must finish checkpointing before the tool.
	if _, err := reopened.PrepareDeliveryChunks(ctx, commentary.ID, []json.RawMessage{json.RawMessage(`{"text":"one"}`), json.RawMessage(`{"text":"two"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkDeliveryChunkSent(ctx, commentary.ID, 0, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, reopened, 0)
	if err := reopened.MarkDeliveryChunkSent(ctx, commentary.ID, 1, 101, "", "", ""); err != nil {
		t.Fatal(err)
	}
	tool := claimProgress(t, reopened, 1)[0]
	if json.Unmarshal(tool.Payload, &event) != nil || event.ID != latestTool.ID {
		t.Fatalf("latest tool was not delivered after commentary: %+v", tool)
	}
	if skip, err := reopened.SuppressTelegramDelivery(ctx, tool.ID); skip || err != nil {
		t.Fatalf("sent commentary still blocks tool: %v %v", skip, err)
	}
	assertProgressTarget(t, reopened, tool, 0)
	checkpointProgress(t, reopened, tool, 102)
	claimProgress(t, reopened, 0)
}

func TestProgressOrderRechecksAlreadyLeasedTool(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	progressEvent(t, env, 2, "tool_progress_message", "turn-a", "")
	tool := claimProgress(t, env.store, 1)[0]
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	if skip, err := env.store.SuppressTelegramDelivery(ctx, tool.ID); skip || !errors.Is(err, ErrTelegramProgressOrderPending) {
		t.Fatalf("already-leased tool overtook new commentary: %v %v", skip, err)
	}
	commentary := claimProgress(t, env.store, 1)[0]
	checkpointProgress(t, env.store, commentary, 100)
	if skip, err := env.store.SuppressTelegramDelivery(ctx, tool.ID); skip || err != nil {
		t.Fatalf("already-leased tool did not resume: %v %v", skip, err)
	}
	checkpointProgress(t, env.store, tool, 101)
}

func TestProgressOrderIgnoresUnrelatedPendingCommentary(t *testing.T) {
	for _, scope := range []string{"bot", "chat", "topic", "session", "turn", "generation", "revoked", "cancelled", "superseded"} {
		t.Run(scope, func(t *testing.T) {
			env := progressEnv(t)
			ctx := t.Context()
			progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
			commentary := claimProgress(t, env.store, 1)[0]
			if err := env.store.RetryDelivery(ctx, commentary.ID, time.Hour, "backoff"); err != nil {
				t.Fatal(err)
			}
			var query string
			toolEnv, toolTurn := env, "turn-a"
			toolSeq, toolGeneration := uint64(3), uint64(1)
			switch scope {
			case "bot":
				query = `UPDATE telegram_deliveries SET bot_id='other' WHERE delivery_id=$1`
			case "chat":
				query = `UPDATE telegram_deliveries SET chat_id=21 WHERE delivery_id=$1`
			case "topic":
				query = `UPDATE telegram_deliveries SET message_thread_id=4 WHERE delivery_id=$1`
			case "revoked":
				query = `UPDATE telegram_deliveries SET visibility_revoked=1 WHERE delivery_id=$1`
			case "cancelled":
				query = `UPDATE telegram_deliveries SET status='cancelled' WHERE delivery_id=$1`
			case "session":
				other := uuid.New()
				insertRouteSession(t, env, other, "thread-b", "")
				if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',2,20,3,$1)`, other); err != nil {
					t.Fatal(err)
				}
				toolEnv.session = other
			case "turn":
				toolTurn = "turn-b"
			case "generation":
				if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running"}}}); err != nil {
					t.Fatal(err)
				}
				toolGeneration = 2
			case "superseded":
				newer := progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
				toolSeq = 4
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET status='cancelled' WHERE event_id=$1`, newer.ID); err != nil {
					t.Fatal(err)
				}
			}
			if query != "" {
				if _, err := env.store.pool.Exec(ctx, query, commentary.ID); err != nil {
					t.Fatal(err)
				}
			}
			progressEventAtGeneration(t, toolEnv, toolSeq, toolGeneration, "tool_progress_message", toolTurn, "")
			// Exercise the pre-send barrier before ClaimDeliveries has had a
			// chance to cancel superseded or revoked queue entries.
			var toolID string
			if err := env.store.pool.QueryRow(ctx, `SELECT delivery_id FROM telegram_deliveries WHERE kind='tool_progress_message'`).Scan(&toolID); err != nil {
				t.Fatal(err)
			}
			if skip, err := env.store.SuppressTelegramDelivery(ctx, toolID); skip || err != nil {
				t.Fatalf("%s commentary blocked another tool scope: %v %v", scope, skip, err)
			}
			tool := claimProgress(t, env.store, 1)[0]
			if tool.ID != toolID {
				t.Fatalf("unrelated commentary blocked tool claim: %+v", tool)
			}
		})
	}
}
