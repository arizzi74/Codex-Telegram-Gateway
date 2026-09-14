package registry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/jackc/pgx/v5"
)

func TestAutomaticSelectionBindingUsesCapturedRevisionIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()
	insertRouteSession(t, env, first, "thread-first-result", "")
	insertRouteSession(t, env, second, "thread-second-result", "")
	selection := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 20, TopicID: 7}

	setTestSelection(t, env.store, selection, env.session)
	if revision := testSelectionRevision(t, env.store, selection); revision != 1 {
		t.Fatalf("initial manual selection revision = %d, want 1", revision)
	}

	// Two asynchronous creations accepted from the same selection revision may
	// not race to replace each other. The first result wins and advances it.
	one := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, uint64Pointer(1))
	two := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, uint64Pointer(1))
	autoBindTestSession(t, env.store, one, first)
	if got := testSelectedSession(t, env.store, selection); got != first {
		t.Fatalf("first automatic selection = %s, want %s", got, first)
	}
	if revision := testSelectionRevision(t, env.store, selection); revision != 2 {
		t.Fatalf("automatic selection revision = %d, want 2", revision)
	}
	autoBindTestSession(t, env.store, two, second)
	if got := testSelectedSession(t, env.store, selection); got != first {
		t.Fatalf("second stale creation replaced selection: %s", got)
	}

	// A later explicit selection invalidates a creation that was already queued.
	stale := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, uint64Pointer(2))
	setTestSelection(t, env.store, selection, env.session)
	autoBindTestSession(t, env.store, stale, second)
	if got := testSelectedSession(t, env.store, selection); got != env.session {
		t.Fatalf("stale creation overwrote manual selection: %s", got)
	}

	// Forks have the additional invariant that the current binding must still
	// be their frozen source even if a malformed/stale revision happens to match.
	revision := testSelectionRevision(t, env.store, selection)
	wrongSourceFork := insertAutomaticBindingCommand(t, env, protocol.CodexCommand, first, &revision)
	autoBindTestSession(t, env.store, wrongSourceFork, second)
	if got := testSelectedSession(t, env.store, selection); got != env.session {
		t.Fatalf("fork from an unselected source replaced selection: %s", got)
	}

	validFork := insertAutomaticBindingCommand(t, env, protocol.CodexCommand, env.session, &revision)
	autoBindTestSession(t, env.store, validFork, second)
	if got := testSelectedSession(t, env.store, selection); got != second {
		t.Fatalf("valid fork selection = %s, want %s", got, second)
	}

	// Topic commands may inherit the chat's default binding. Preserve the same
	// exact-topic-then-default priority when validating a fork's source.
	if _, err := env.store.pool.Exec(ctx, `DELETE FROM telegram_bindings
        WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=7`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',10,20,0,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	revision = testSelectionRevision(t, env.store, selection)
	fallbackFork := insertAutomaticBindingCommand(t, env, protocol.CodexCommand, env.session, &revision)
	autoBindTestSession(t, env.store, fallbackFork, first)
	if got := testSelectedSession(t, env.store, selection); got != first {
		t.Fatalf("fork inherited from default selection = %s, want %s", got, first)
	}

	// A disconnect invalidates pending work, and a legacy command without a
	// captured revision stays conservative after an upgrade.
	disconnectTestSelection(t, env.store, selection)
	pending := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, uint64Pointer(revision+1))
	legacy := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, nil)
	autoBindTestSession(t, env.store, pending, first)
	autoBindTestSession(t, env.store, legacy, first)
	if _, ok := findTestSelectedSession(t, env.store, selection); ok {
		t.Fatal("disconnect was undone by pending or legacy automatic binding")
	}
}

func TestLockAutomaticBindingContextIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	revision := uint64(0)
	commandID := insertAutomaticBindingCommand(t, env, protocol.NewSession, uuid.Nil, &revision)
	resultData, err := json.Marshal(protocol.Result{CommandID: commandID.String(), Session: &protocol.Session{ID: uuid.NewString()}})
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{Kind: "command_completed", RuntimeID: env.runtime.String(), RuntimeGeneration: 1, Data: resultData}

	first, err := env.store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(context.Background())
	if err := lockAutomaticBindingContext(context.Background(), first, env.worker, event); err != nil {
		t.Fatal(err)
	}

	second, err := env.store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(context.Background())
	selection := IncomingUpdate{BotID: "bot", UserID: 10, ChatID: 20, TopicID: 7}
	var acquired bool
	if err := second.QueryRow(context.Background(), "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", telegramContextKey(selection)).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("automatic binding helper did not hold the Telegram context lock")
	}
	if err := first.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRow(context.Background(), "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", telegramContextKey(selection)).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("Telegram context lock was not released with the event transaction")
	}
}

func setTestSelection(t *testing.T, store *Store, in IncomingUpdate, sessionID uuid.UUID) {
	t.Helper()
	tx, err := store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := setBinding(context.Background(), tx, in, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func disconnectTestSelection(t *testing.T, store *Store, in IncomingUpdate) {
	t.Helper()
	tx, err := store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := bumpSelectionRevision(context.Background(), tx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `DELETE FROM telegram_bindings
        WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, in.TopicID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func testSelectionRevision(t *testing.T, store *Store, in IncomingUpdate) uint64 {
	t.Helper()
	tx, err := store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	revision, err := selectionRevision(context.Background(), tx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return revision
}

func insertAutomaticBindingCommand(t *testing.T, env eventTestEnv, operation protocol.Operation, source uuid.UUID, revision *uint64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	args := protocol.Arguments{SelectionRevision: revision}
	if operation == protocol.CodexCommand {
		args.Codex = &protocol.CodexCommandPayload{Name: "fork"}
	}
	command := protocol.Command{ID: id.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		Operation: operation, Arguments: args, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	var dbSession any
	if source != uuid.Nil {
		command.SessionID, command.ThreadID, dbSession = source.String(), "thread-source", source
	}
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(context.Background(), `INSERT INTO commands
        (command_id, source, worker_id, runtime_id, runtime_generation, session_id,
         operation, payload, status, telegram_bot_id, telegram_user_id,
         telegram_chat_id, telegram_message_thread_id, expires_at)
        VALUES ($1,'telegram',$2,$3,1,$4,$5,$6,'pending','bot',10,20,7,now()+interval '1 hour')`,
		id, env.worker, env.runtime, dbSession, string(operation), payload); err != nil {
		t.Fatal(err)
	}
	return id
}

func autoBindTestSession(t *testing.T, store *Store, commandID, sessionID uuid.UUID) {
	t.Helper()
	tx, err := store.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := bindCommandSession(context.Background(), tx, commandID, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func testSelectedSession(t *testing.T, store *Store, in IncomingUpdate) uuid.UUID {
	t.Helper()
	id, ok := findTestSelectedSession(t, store, in)
	if !ok {
		t.Fatal("Telegram selection is absent")
	}
	return id
}

func findTestSelectedSession(t *testing.T, store *Store, in IncomingUpdate) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	err := store.pool.QueryRow(context.Background(), `SELECT session_id FROM telegram_bindings
        WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, in.TopicID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return id, true
}

func uint64Pointer(value uint64) *uint64 { return &value }
