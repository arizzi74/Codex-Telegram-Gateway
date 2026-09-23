package registry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUISessionDeletionDurableIdempotentAndNoTelegramSideEffects(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	in := telegramUpdate(env, 0)
	setTestSelection(t, env.store, in, other)
	requestID := uuid.New()
	queued, err := env.store.QueueWebUISessionDeletion(ctx, env.session, requestID)
	if err != nil || !queued.Pending || queued.Deleted || queued.CommandID != requestID.String() {
		t.Fatalf("queue: %+v %v", queued, err)
	}
	again, err := env.store.QueueWebUISessionDeletion(ctx, env.session, requestID)
	if err != nil || again != queued {
		t.Fatalf("retry was not idempotent: %+v %v", again, err)
	}
	if _, err := env.store.QueueWebUISessionDeletion(ctx, env.session, uuid.New()); err == nil {
		t.Fatal("a second confirmation queued a duplicate deletion")
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("durable queue: %+v %v", commands, err)
	}
	command := commands[0]
	if command.ID != requestID.String() || command.Operation != protocol.DeleteSession || command.SessionID != env.session.String() || command.ThreadID != "thread-1" || command.RuntimeID != env.runtime.String() || command.RuntimeGeneration != 1 || command.Arguments.CWD != "/work" || command.ExpiresAt.Sub(command.CreatedAt) != 2*time.Minute {
		t.Fatalf("deletion target not pinned: %+v", command)
	}
	var source string
	var telegram bool
	if err := env.store.pool.QueryRow(ctx, `SELECT source,telegram_bot_id IS NOT NULL OR telegram_user_id IS NOT NULL OR telegram_chat_id IS NOT NULL FROM commands WHERE command_id=$1`, requestID).Scan(&source, &telegram); err != nil || source != "webui" || telegram {
		t.Fatalf("deletion impersonated Telegram: %q %v %v", source, telegram, err)
	}
	if got := testSelectedSession(t, env.store, in); got != other {
		t.Fatal("queuing deletion switched Telegram selection")
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: requestID.String(), Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	accepted, err := env.store.WebUISessionDeletion(ctx, env.session, requestID)
	if err != nil || !accepted.Pending || accepted.Deleted || accepted.Status != "acknowledged" {
		t.Fatalf("acknowledgement falsely completed deletion: %+v %v", accepted, err)
	}
	deleted := protocol.Session{ID: env.session.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), ThreadID: "thread-1", Name: "Thread", CWD: "/work", State: "not_loaded", Archived: true, Deleted: true, UpdatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(protocol.Result{CommandID: command.ID, State: "completed", Session: &deleted})
	event := protocol.Event{ID: uuid.NewString(), Seq: 2, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: raw}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	completed, err := env.store.WebUISessionDeletion(ctx, env.session, requestID)
	if err != nil || completed.Pending || !completed.Deleted || completed.Status != "completed" {
		t.Fatalf("completion: %+v %v", completed, err)
	}
	again, err = env.store.QueueWebUISessionDeletion(ctx, env.session, requestID)
	if err != nil || again != completed {
		t.Fatalf("completed replay should not require visible session: %+v %v", again, err)
	}
	if got := testSelectedSession(t, env.store, in); got != other {
		t.Fatal("completed deletion switched another Telegram selection")
	}
	var deliveries, wizards int
	if err := env.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM telegram_deliveries),(SELECT count(*) FROM telegram_session_wizards)`).Scan(&deliveries, &wizards); err != nil || deliveries != 0 || wizards != 0 {
		t.Fatalf("web deletion created Telegram messages/wizard: %d/%d %v", deliveries, wizards, err)
	}
	snapshot, err := env.store.LiveSessionActivity(ctx)
	if err != nil || len(snapshot.Sessions) != 1 || snapshot.Sessions[0].SessionID != other.String() {
		t.Fatalf("deleted session still visible: %+v %v", snapshot, err)
	}
}

func TestWebUISessionDeletionRejectsUnsafeTargetsAndScopesStatus(t *testing.T) {
	for _, tc := range []struct{ name, query, code string }{
		{"archived", `UPDATE sessions SET archived=TRUE`, "not_found"},
		{"running", `UPDATE sessions SET state='running',active_turn_id='turn'`, protocol.SessionBusy},
		{"pending input", `UPDATE sessions SET state='waiting_input'`, protocol.SessionBusy},
		{"disabled", `UPDATE workers SET enabled=FALSE`, "worker_unavailable"},
		{"offline", `UPDATE workers SET connectivity='unreachable'`, "worker_unavailable"},
		{"runtime stopped", `UPDATE runtimes SET state='stopped'`, "worker_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := t.Context()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.pool.Exec(ctx, tc.query); err != nil {
				t.Fatal(err)
			}
			_, err := env.store.QueueWebUISessionDeletion(ctx, env.session, uuid.New())
			var operationErr *protocol.Error
			if (tc.code == "not_found" && !errors.Is(err, ErrWebUIDeletionNotFound)) || (tc.code != "not_found" && (!errors.As(err, &operationErr) || operationErr.Code != tc.code)) {
				t.Fatalf("unexpected target rejection: %v", err)
			}
			var count int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM commands`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected deletion changed queue: %d %v", count, err)
			}
		})
	}
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := env.store.QueueWebUISessionDeletion(ctx, env.session, id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.WebUISessionDeletion(ctx, uuid.New(), id); !errors.Is(err, ErrWebUIDeletionNotFound) {
		t.Fatalf("cross-session status exposed: %v", err)
	}
	if _, err := env.store.QueueWebUISessionDeletion(ctx, uuid.New(), id); err == nil {
		t.Fatal("request identifier reused for another target")
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE commands SET status='failed',error_code='PRIVATE_CODE',error_message='PRIVATE ERROR /private/path' WHERE command_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	failed, err := env.store.WebUISessionDeletion(ctx, env.session, id)
	if err != nil || failed.Pending || failed.Deleted || failed.ErrorCode != "command_failed" {
		t.Fatalf("failure result: %+v %v", failed, err)
	}
	raw, _ := json.Marshal(failed)
	if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "/private") {
		t.Fatalf("worker error leaked: %s", raw)
	}
	var archived bool
	if err := env.store.pool.QueryRow(ctx, `SELECT archived FROM sessions WHERE session_id=$1`, env.session).Scan(&archived); err != nil || archived {
		t.Fatal("failed deletion hid session")
	}
	replayed, err := env.store.QueueWebUISessionDeletion(ctx, env.session, id)
	if err != nil || replayed != failed {
		t.Fatalf("failed attempt retried destructively: %+v %v", replayed, err)
	}
	if _, err := env.store.QueueWebUISessionDeletion(ctx, env.session, uuid.New()); err != nil {
		t.Fatalf("explicit retry could not queue: %v", err)
	}
}
