package registry

import "testing"

func TestTelegramProgressTurnIndexPreservesStringIDs(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	progressEvent(t, env, 2, "agent_progress_message", "00123", "")
	checkpointProgress(t, env.store, claimProgress(t, env.store, 1)[0], 100)
	progressEvent(t, env, 3, "turn_completed", "123", "")
	otherFinal := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, otherFinal.ID, 200, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("numeric-looking turn IDs were conflated: %+v: %v", due, err)
	}
	progressEvent(t, env, 4, "turn_completed", "00123", "")
	final := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, final.ID, 201, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 1 {
		t.Fatalf("matching string turn ID did not clean progress: %+v: %v", due, err)
	}
}

func TestTelegramProgressRuntimeFailureWaitsForDestinationDeliveryAfterStop(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',1,20,4,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	for _, delivery := range claimProgress(t, env.store, 2) {
		checkpointProgress(t, env.store, delivery, 100)
	}
	// Runtime failure is deliberately independent of the progress turn ID.
	// Its queued notification must still block the stopped-runtime fallback.
	progressEvent(t, env, 3, "runtime_failed", "", "")
	finals := claimProgress(t, env.store, 2)
	progressEvent(t, env, 4, "runtime_stopped", "", "")
	if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
		t.Fatalf("runtime stop bypassed queued failure notification: %+v: %v", due, err)
	}
	for _, final := range finals {
		if err := env.store.MarkDeliverySent(ctx, final.ID, 200, "", "", ""); err != nil {
			t.Fatal(err)
		}
		due, err := env.store.ClaimTelegramDeletions(ctx, 100)
		if err != nil || len(due) != 1 || due[0].TopicID != final.TopicID {
			t.Fatalf("failure cleanup crossed destination: %+v for topic %d: %v", due, final.TopicID, err)
		}
		if err := env.store.MarkTelegramDeletionDone(ctx, due[0].ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTelegramDeletionIdleProbeRechecksBackoffAndLease(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	assertNoDeletions := func() {
		t.Helper()
		if due, err := env.store.ClaimTelegramDeletions(ctx, 100); err != nil || len(due) != 0 {
			t.Fatalf("unexpected deletion: %+v: %v", due, err)
		}
	}
	assertNoDeletions()
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	checkpointProgress(t, env.store, claimProgress(t, env.store, 1)[0], 100)
	// Even retired messages respect a pending retry's backoff.
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_progress_messages
        SET retire_requested=1,next_attempt_at='2999-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	assertNoDeletions()
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_progress_messages
        SET next_attempt_at='2000-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 1 || due[0].Attempt != 1 {
		t.Fatalf("newly due deletion was missed after idle polls: %+v: %v", due, err)
	}
	assertNoDeletions()
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_progress_messages
        SET next_attempt_at='2000-01-01T00:00:00.000000000Z'`); err != nil {
		t.Fatal(err)
	}
	retry, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(retry) != 1 || retry[0].ID != due[0].ID || retry[0].Attempt != 2 {
		t.Fatalf("expired deleting lease was missed after idle polls: %+v: %v", retry, err)
	}
	if err := env.store.MarkTelegramDeletionDone(ctx, retry[0].ID); err != nil {
		t.Fatal(err)
	}
	assertNoDeletions()
}
