package registry

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

type eventTestEnv struct {
	store                                *Store
	worker, connection, runtime, session uuid.UUID
}

func newEventTestEnv(t *testing.T) eventTestEnv {
	t.Helper()
	ctx := context.Background()
	store := integrationStore(t)
	hash := sha256.Sum256([]byte("cwk_event_test"))
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: "event-worker", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime := uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []Runtime{{
		ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	return eventTestEnv{store: store, worker: worker.ID, connection: connection, runtime: runtime, session: uuid.New()}
}

func (e eventTestEnv) discovery(t *testing.T) protocol.Event {
	t.Helper()
	session := protocol.Session{ID: e.session.String(), WorkerID: e.worker.String(), RuntimeID: e.runtime.String(),
		ThreadID: "thread-1", Name: "Thread", CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{Seq: 1, ID: uuid.NewString(), WorkerID: e.worker.String(), RuntimeID: e.runtime.String(),
		RuntimeGeneration: 1, SessionID: e.session.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: data}
}

func TestIngestEventReplayAndRollbackIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	first := env.discovery(t)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, first); err != nil {
		t.Fatal(err)
	}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, first); err != nil {
		t.Fatalf("exact event replay failed: %v", err)
	}
	conflict := first
	conflict.ID = uuid.NewString()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, conflict); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("conflicting replay error = %v, want ErrEventConflict", err)
	}
	badData, _ := json.Marshal(protocol.Result{CommandID: uuid.NewString()})
	bad := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: badData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, bad); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("post-insert target error = %v, want ErrEventTarget", err)
	}
	if watermark, err := env.store.EventWatermark(ctx, env.worker); err != nil || watermark != 1 {
		t.Fatalf("rollback advanced watermark: %d, %v", watermark, err)
	}
	var events int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE worker_id = $1", env.worker).Scan(&events); err != nil || events != 1 {
		t.Fatalf("rollback persisted event count=%d err=%v", events, err)
	}
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].ID != env.session.String() {
		t.Fatalf("session snapshot = %#v, %v", sessions, err)
	}
	runtimes, err := env.store.RuntimeSnapshot(ctx)
	if err != nil || len(runtimes) != 1 || runtimes[0].ID != env.runtime.String() {
		t.Fatalf("runtime snapshot = %#v, %v", runtimes, err)
	}
}

func TestIngestEventReplayPreservesJSONSemanticsIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, stored, replay string
		equal                bool
	}{
		{"object order and whitespace", `{"a":1,"b":[true,null]}`, ` {"b": [true, null], "a": 1} `, true},
		{"decimal spelling", `{"number":1.00}`, `{"number":1e0}`, true},
		{"trailing decimal zeroes", `120.000`, `1.2e2`, true},
		{"negative zero", `-0.00`, `0e20`, true},
		{"duplicate object key", `{"a":0,"a":2}`, `{"a":2}`, true},
		{"large exact integer", `9007199254740993`, `900719925474099300e-2`, true},
		{"large distinct integer", `9007199254740993`, `9007199254740992`, false},
		{"large exponent", `1e1000000000`, `10e999999999`, true},
		{"array order", `[1,2]`, `[2,1]`, false},
		{"null versus absent", `{"a":null}`, `{}`, false},
		{"number versus text", `1`, `"1"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			event := env.discovery(t)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatal(err)
			}
			payload["extra"] = json.RawMessage(tc.stored)
			var err error
			event.Data, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			// Imported PostgreSQL event timestamps have microsecond precision.
			event.OccurredAt = event.OccurredAt.Truncate(time.Microsecond)
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
				t.Fatal(err)
			}
			payload["extra"] = json.RawMessage(tc.replay)
			event.Data, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			event.OccurredAt = event.OccurredAt.Add(123 * time.Nanosecond)
			err = env.store.IngestEvent(ctx, env.worker, env.connection, event)
			if tc.equal && err != nil {
				t.Fatalf("semantically identical replay rejected: %v", err)
			}
			if !tc.equal && !errors.Is(err, ErrEventConflict) {
				t.Fatalf("changed replay = %v, want ErrEventConflict", err)
			}
			if watermark, err := env.store.EventWatermark(ctx, env.worker); err != nil || watermark != 1 {
				t.Fatalf("replay changed watermark to %d: %v", watermark, err)
			}
		})
	}
}

func TestIngestEventDuplicateJSONKeysMatchPersistedTurnRoutingIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "turn_started", OccurredAt: time.Now().UTC(),
		Data: json.RawMessage(`{"turn_id":"stale-turn","turn_id":"active-turn"}`)}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	// Go applies the last duplicate key. Durable SQL routing and temporary
	// message cleanup must use the same turn when reading the stored JSON.
	var activeTurn, persistedTurn string
	if err := env.store.pool.QueryRow(ctx, `SELECT s.active_turn_id,json_extract(e.payload,'$.turn_id')
        FROM sessions s JOIN events e ON e.session_id=s.session_id WHERE e.event_id=$1`, event.ID).Scan(&activeTurn, &persistedTurn); err != nil {
		t.Fatal(err)
	}
	if activeTurn != "active-turn" || persistedTurn != activeTurn {
		t.Fatalf("Go session routing and SQL event routing diverged: active=%q persisted=%q", activeTurn, persistedTurn)
	}
	event.Data = json.RawMessage(`{"turn_id":"active-turn"}`)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatalf("semantically identical replay of normalized JSON failed: %v", err)
	}
}

func TestIngestEventStaleGenerationAndCrossWorkerIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{
		ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	staleData, _ := json.Marshal(protocol.Result{TurnID: "turn-1"})
	stale := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "turn_failed", OccurredAt: time.Now().UTC(), Data: staleData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, stale); err != nil {
		t.Fatal(err)
	}
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil || sessions[0].State != "idle" {
		t.Fatalf("stale event corrupted session: %#v, %v", sessions, err)
	}

	hash := sha256.Sum256([]byte("cwk_other_worker"))
	other, err := env.store.CreateWorker(ctx, CreateWorkerInput{Name: "other", OS: "linux", Arch: "amd64", TokenHash: hash[:]})
	if err != nil {
		t.Fatal(err)
	}
	otherConn, otherRuntime := uuid.New(), uuid.New()
	if err := env.store.BindConnection(ctx, other.ID, otherConn); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: other.ID, ConnectionID: otherConn, Runtimes: []Runtime{{
		ID: otherRuntime, WorkerID: other.ID, ProfileID: "other", Name: "Other", Generation: 1, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	cross := protocol.Event{Seq: 3, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: otherRuntime.String(),
		RuntimeGeneration: 1, Kind: "runtime_started", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{}`)}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, cross); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("cross-worker event error = %v, want ErrEventTarget", err)
	}
	if watermark, _ := env.store.EventWatermark(ctx, env.worker); watermark != 2 {
		t.Fatalf("cross-worker event advanced watermark: %d", watermark)
	}
}

func TestIngestEventCommandOutcomeAndApprovalCleanupIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	commandID := uuid.New()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
        (command_id, source, worker_id, runtime_id, runtime_generation, session_id,
         operation, payload, status, telegram_bot_id, telegram_chat_id, telegram_message_thread_id)
        VALUES ($1,'telegram',$2,$3,1,$4,'start_turn','{}','pending','bot',123,0)`,
		commandID, env.worker, env.runtime, env.session); err != nil {
		t.Fatal(err)
	}
	resultData, _ := json.Marshal(protocol.Result{CommandID: commandID.String(), TurnID: "turn-1", State: "idle", Text: "done"})
	completed := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: resultData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, completed); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id = $1", commandID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("command status = %q, %v", status, err)
	}
	var deliveries int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id = $1", uuid.MustParse(completed.ID)).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("command delivery count = %d, %v", deliveries, err)
	}
	approvalID := uuid.New()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO approvals
        (approval_id, worker_id, runtime_id, runtime_generation, session_id,
         codex_request_id, codex_thread_id, approval_type, request_payload, state, requested_at)
        VALUES ($1,$2,$3,1,$4,'req-1','thread-1','permissions','{}','pending',(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))`,
		approvalID, env.worker, env.runtime, env.session); err != nil {
		t.Fatal(err)
	}
	failed := protocol.Event{Seq: 3, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, Kind: "runtime_failed", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"reason":"exit"}`)}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, failed); err != nil {
		t.Fatal(err)
	}
	if err := env.store.pool.QueryRow(ctx, "SELECT state FROM approvals WHERE approval_id = $1", approvalID).Scan(&status); err != nil || status != "cleared" {
		t.Fatalf("approval state = %q, %v", status, err)
	}
}

func TestIngestEventSessionInputAndTurnGuardsIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	state := protocol.Session{ID: env.session.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		ThreadID: "thread-1", State: "running", ActiveTurnID: "turn-1", Loaded: true, UpdatedAt: time.Now().UTC()}
	stateData, _ := json.Marshal(state)
	changed := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		SessionID: env.session.String(), Kind: "session_state_changed", OccurredAt: time.Now().UTC(), Data: stateData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id, user_id, chat_id, message_thread_id, session_id) VALUES ('bot', 7, 99, 0, $1)`, env.session); err != nil {
		t.Fatal(err)
	}
	input := protocol.Approval{ID: uuid.NewString(), RequestID: "input-1", ThreadID: "thread-1", TurnID: "turn-1", Type: "input"}
	inputData, _ := json.Marshal(input)
	requested := protocol.Event{Seq: 3, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		SessionID: env.session.String(), Kind: "user_input_requested", OccurredAt: time.Now().UTC(), Data: inputData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, requested); err != nil {
		t.Fatal(err)
	}
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil || sessions[0].State != "waiting_input" {
		t.Fatalf("input state = %#v, %v", sessions, err)
	}
	var approvalState string
	if err := env.store.pool.QueryRow(ctx, "SELECT state FROM approvals WHERE approval_id = $1", uuid.MustParse(input.ID)).Scan(&approvalState); err != nil || approvalState != "pending" {
		t.Fatalf("input approval = %q, %v", approvalState, err)
	}
	var deliveries int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id = $1", uuid.MustParse(requested.ID)).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("input deliveries = %d, %v", deliveries, err)
	}
	differentTime := requested
	differentTime.OccurredAt = differentTime.OccurredAt.Add(time.Second)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, differentTime); !errors.Is(err, ErrEventConflict) {
		t.Fatalf("replay with changed timestamp = %v, want ErrEventConflict", err)
	}
	future := protocol.Event{Seq: 4, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 2,
		SessionID: env.session.String(), Kind: "turn_started", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"turn_id":"future"}`)}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, future); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("future non-start event = %v, want ErrEventTarget", err)
	}

	// A newer active turn must survive a late completion for an earlier turn.
	turnEnv := newEventTestEnv(t)
	if err := turnEnv.store.IngestEvent(ctx, turnEnv.worker, turnEnv.connection, turnEnv.discovery(t)); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []struct {
		seq        uint64
		kind, turn string
	}{{2, "turn_started", "T1"}, {3, "turn_started", "T2"}, {4, "turn_completed", "T1"}} {
		data, _ := json.Marshal(protocol.Result{TurnID: turn.turn})
		event := protocol.Event{Seq: turn.seq, ID: uuid.NewString(), WorkerID: turnEnv.worker.String(), RuntimeID: turnEnv.runtime.String(), RuntimeGeneration: 1,
			SessionID: turnEnv.session.String(), Kind: turn.kind, OccurredAt: time.Now().UTC(), Data: data}
		if err := turnEnv.store.IngestEvent(ctx, turnEnv.worker, turnEnv.connection, event); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err = turnEnv.store.SessionSnapshot(ctx)
	if err != nil || sessions[0].State != "running" || sessions[0].ActiveTurnID != "T2" {
		t.Fatalf("late terminal event overwrote active turn: %#v, %v", sessions, err)
	}
}

func TestIngestEventHistoricalReplayRuntimeFailureAndNewSessionBindingIntegration(t *testing.T) {
	ctx := context.Background()
	// A generation-8 hello can arrive before replay of generation-7 discovery.
	history := newEventTestEnv(t)
	if err := history.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: history.worker, ConnectionID: history.connection, Runtimes: []Runtime{{
		ID: history.runtime, WorkerID: history.worker, ProfileID: "main", Name: "Main", Generation: 8, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	historical := history.discovery(t)
	historical.RuntimeGeneration = 7
	if err := history.store.IngestEvent(ctx, history.worker, history.connection, historical); err != nil {
		t.Fatal(err)
	}
	sessions, err := history.store.SessionSnapshot(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].State != "not_loaded" || sessions[0].ActiveTurnID != "" {
		t.Fatalf("historical session identity = %#v, %v", sessions, err)
	}

	// Runtime failures notify every bound session in that runtime.
	if _, err := history.store.pool.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id, user_id, chat_id, message_thread_id, session_id) VALUES ('bot', 7, 99, 0, $1)`, history.session); err != nil {
		t.Fatal(err)
	}
	failed := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: history.worker.String(), RuntimeID: history.runtime.String(), RuntimeGeneration: 8,
		Kind: "runtime_failed", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"reason":"exit"}`)}
	if err := history.store.IngestEvent(ctx, history.worker, history.connection, failed); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	if err := history.store.pool.QueryRow(ctx, "SELECT count(*) FROM telegram_deliveries WHERE event_id = $1", uuid.MustParse(failed.ID)).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("runtime failure delivery count = %d, %v", deliveries, err)
	}

	// A new_session command has no immutable session target until the worker
	// returns its session snapshot; its Telegram context is bound atomically.
	env := newEventTestEnv(t)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	commandID, returnedSession := uuid.New(), uuid.New()
	revision := uint64(0)
	createdAt := time.Now().UTC()
	commandPayload, _ := json.Marshal(protocol.Command{
		ID: commandID.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		Operation: protocol.NewSession, Arguments: protocol.Arguments{SelectionRevision: &revision}, CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour),
	})
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO commands
        (command_id, source, worker_id, runtime_id, runtime_generation, operation, payload, status,
         telegram_bot_id, telegram_user_id, telegram_chat_id, telegram_message_thread_id)
		VALUES ($1,'telegram',$2,$3,1,'new_session',$4,'pending','bot',77,88,0)`, commandID, env.worker, env.runtime, json.RawMessage(commandPayload)); err != nil {
		t.Fatal(err)
	}
	returned := protocol.Session{ID: returnedSession.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), ThreadID: "thread-new", State: "running", Loaded: true, UpdatedAt: time.Now().UTC()}
	resultData, _ := json.Marshal(protocol.Result{CommandID: commandID.String(), TurnID: "new-turn", Session: &returned})
	started := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
		Kind: "turn_started", OccurredAt: time.Now().UTC(), Data: resultData}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, started); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id = $1", commandID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("new session accepted command = %q, %v", status, err)
	}
	var bound uuid.UUID
	if err := env.store.pool.QueryRow(ctx, "SELECT session_id FROM telegram_bindings WHERE bot_id = 'bot' AND user_id = 77 AND chat_id = 88").Scan(&bound); err != nil || bound != returnedSession {
		t.Fatalf("new session telegram binding = %s, %v", bound, err)
	}
}
