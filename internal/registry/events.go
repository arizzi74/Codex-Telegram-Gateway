package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

var (
	// ErrEventSequence means a durable event skipped an unacknowledged sequence.
	ErrEventSequence = errors.New("registry: unexpected event sequence")
	// ErrEventConflict means an already acknowledged sequence was replayed with
	// different immutable event contents.
	ErrEventConflict = errors.New("registry: conflicting event replay")
	// ErrEventTarget means an event names a runtime, session, or command that is
	// absent or belongs to another worker.
	ErrEventTarget = errors.New("registry: invalid event target")
)

// IngestEvent durably records a worker event and applies its normalized state
// transition in one transaction. A durable event is acknowledged by the hub
// only after this method returns nil.
func (s *Store) IngestEvent(ctx context.Context, workerID, connectionID uuid.UUID, event protocol.Event) error {
	if err := validateEvent(workerID, connectionID, event); err != nil {
		return err
	}
	// JSONB and Go both use the final occurrence of a duplicate object key.
	// Normalize before persistence so SQLite JSON paths read the same values
	// as the normalized Go projections used to apply this event.
	data, err := normalizeEventJSON(event.Data)
	if err != nil {
		return fmt.Errorf("registry: normalize event payload: %w", err)
	}
	event.Data = data
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin event ingestion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkLeaseTx(ctx, tx, workerID, connectionID); err != nil {
		return err
	}
	// Class B events are intentionally not placed in the ordered durable ledger
	// and do not advance the worker watermark.
	if !event.Durable() {
		return tx.Commit(ctx)
	}

	sequence := int64(event.Seq)
	var watermark int64
	err = tx.QueryRow(ctx, `SELECT event_seq FROM worker_event_watermarks
        WHERE worker_id = $1`, workerID).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkerNotFound
	}
	if err != nil {
		return fmt.Errorf("registry: lock event watermark: %w", err)
	}
	if sequence <= watermark {
		return verifyEventReplay(ctx, tx, workerID, sequence, event)
	}
	if sequence != watermark+1 {
		return ErrEventSequence
	}

	target, err := resolveEventTarget(ctx, tx, workerID, event)
	if err != nil {
		return err
	}
	// The event ledger has a composite session ownership FK. A discovery creates
	// that normalized session in this transaction before its event row is added;
	// the transaction still rolls back both writes if any later step fails.
	if event.Kind == "session_discovered" && target.runtimeID != nil {
		var session protocol.Session
		if err := json.Unmarshal(event.Data, &session); err != nil {
			return fmt.Errorf("registry: decode session discovery: %w", err)
		}
		if target.runtimeCurrent {
			if err := upsertProtocolSession(ctx, tx, workerID, *target.runtimeID, target.sessionID, session); err != nil {
				return err
			}
		} else if target.runtimeHistorical {
			if err := ensureHistoricalSessionIdentity(ctx, tx, workerID, *target.runtimeID, target.sessionID, session); err != nil {
				return err
			}
		}
	}
	eventID, _ := uuid.Parse(event.ID) // validateEvent already checked it.
	var runtimeID any
	if target.runtimeID != nil {
		runtimeID = *target.runtimeID
	}
	var sessionID any
	if target.sessionID != nil {
		sessionID = *target.sessionID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events
        (event_id, worker_id, runtime_id, runtime_generation, session_id, event_seq,
         kind, payload, occurred_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		eventID, workerID, runtimeID, target.generation, sessionID, sequence,
		event.Kind, event.Data, event.OccurredAt); err != nil {
		return fmt.Errorf("registry: persist event: %w", err)
	}

	notify, commandID, err := s.applyEvent(ctx, tx, workerID, event, target)
	if err != nil {
		return err
	}
	if notify {
		if err := enqueueEventDeliveries(ctx, tx, eventID, event, target.sessionID, target.runtimeID, commandID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE worker_event_watermarks
        SET event_seq = $2, updated_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE worker_id = $1`, workerID, sequence); err != nil {
		return fmt.Errorf("registry: advance event watermark: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("registry: commit event ingestion: %w", err)
	}
	return nil
}

func validateEvent(workerID, connectionID uuid.UUID, event protocol.Event) error {
	if workerID == uuid.Nil || connectionID == uuid.Nil || event.Seq == 0 || event.Seq > math.MaxInt64 ||
		event.OccurredAt.IsZero() || strings.TrimSpace(event.Kind) == "" || !json.Valid(event.Data) {
		return errors.New("registry: invalid worker event")
	}
	if event.WorkerID != workerID.String() {
		return ErrEventTarget
	}
	if _, err := uuid.Parse(event.ID); err != nil {
		return errors.New("registry: invalid event ID")
	}
	if event.RuntimeGeneration > math.MaxInt64 {
		return errors.New("registry: event generation exceeds database range")
	}
	if event.RuntimeID != "" {
		if _, err := uuid.Parse(event.RuntimeID); err != nil {
			return ErrEventTarget
		}
	}
	if event.SessionID != "" {
		if _, err := uuid.Parse(event.SessionID); err != nil {
			return ErrEventTarget
		}
	}
	return nil
}

func checkLeaseTx(ctx context.Context, tx *dbTx, workerID, connectionID uuid.UUID) error {
	var current *uuid.UUID
	var enabled bool
	err := tx.QueryRow(ctx, `SELECT connection_id, enabled FROM workers
        WHERE worker_id = $1`, workerID).Scan(&current, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConnectionFenced
	}
	if err != nil {
		return fmt.Errorf("registry: check event connection: %w", err)
	}
	if !enabled || current == nil || *current != connectionID {
		return ErrConnectionFenced
	}
	return nil
}

func verifyEventReplay(ctx context.Context, tx *dbTx, workerID uuid.UUID, sequence int64, event protocol.Event) error {
	eventID, _ := uuid.Parse(event.ID)
	var payload json.RawMessage
	var occurredAt time.Time
	err := tx.QueryRow(ctx, `SELECT payload, occurred_at FROM events
        WHERE worker_id = $1 AND event_seq = $2 AND event_id = $3 AND kind = $4
          AND runtime_id IS $5 AND runtime_generation IS $6 AND session_id IS $7`,
		workerID, sequence, eventID, event.Kind, nullUUID(event.RuntimeID),
		nullGeneration(event), nullUUID(event.SessionID)).Scan(&payload, &occurredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEventConflict
	}
	if err != nil {
		return fmt.Errorf("registry: verify replay: %w", err)
	}
	// Earlier PostgreSQL releases stored timestamps at microsecond precision.
	// Keep replay compatibility for migrated events, including worker retries
	// whose original payload contains more precise timestamps.
	if !occurredAt.Truncate(time.Microsecond).Equal(event.OccurredAt.Truncate(time.Microsecond)) ||
		!equalEventJSON(payload, event.Data) {
		return ErrEventConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("registry: commit replay: %w", err)
	}
	return nil
}

func normalizeEventJSON(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// equalEventJSON preserves the semantic equality previously supplied by JSONB:
// object key order, whitespace, duplicate keys (last wins), and equivalent
// decimal number spellings do not turn a valid durable replay into a conflict.
// UseNumber avoids losing precision for event sequence numbers and other IDs.
func equalEventJSON(left, right []byte) bool {
	if !json.Valid(left) || !json.Valid(right) {
		return false
	}
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	a, err := decode(left)
	if err != nil {
		return false
	}
	b, err := decode(right)
	return err == nil && equalJSONValue(a, b)
}

func equalJSONValue(left, right any) bool {
	switch a := left.(type) {
	case json.Number:
		b, ok := right.(json.Number)
		return ok && normalizedJSONNumber(a) == normalizedJSONNumber(b)
	case map[string]any:
		b, ok := right.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, ok := b[key]
			if !ok || !equalJSONValue(value, other) {
				return false
			}
		}
		return true
	case []any:
		b, ok := right.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i, value := range a {
			if !equalJSONValue(value, b[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}

// Normalize the decimal coefficient and exponent without expanding exponents.
// This remains bounded by input size even for values such as 1e1000000000.
func normalizedJSONNumber(number json.Number) string {
	raw := string(number)
	sign := ""
	if strings.HasPrefix(raw, "-") {
		sign, raw = "-", raw[1:]
	}
	var exponent big.Int
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		exponent.SetString(raw[index+1:], 10) // The JSON decoder validated the number.
		raw = raw[:index]
	}
	if index := strings.IndexByte(raw, '.'); index >= 0 {
		exponent.Sub(&exponent, big.NewInt(int64(len(raw)-index-1)))
		raw = raw[:index] + raw[index+1:]
	}
	raw = strings.TrimLeft(raw, "0")
	if raw == "" {
		return "0"
	}
	coefficient := strings.TrimRight(raw, "0")
	exponent.Add(&exponent, big.NewInt(int64(len(raw)-len(coefficient))))
	return sign + coefficient + "e" + exponent.String()
}

type eventTarget struct {
	runtimeID         *uuid.UUID
	sessionID         *uuid.UUID
	generation        any
	runtimeCurrent    bool
	runtimeHistorical bool
	runtimeFuture     bool
}

func resolveEventTarget(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event) (eventTarget, error) {
	target := eventTarget{}
	if event.RuntimeID != "" {
		id, _ := uuid.Parse(event.RuntimeID)
		target.runtimeID = &id
		var generation int64
		err := tx.QueryRow(ctx, `SELECT generation FROM runtimes
            WHERE runtime_id = $1 AND worker_id = $2`, id, workerID).Scan(&generation)
		if errors.Is(err, sql.ErrNoRows) {
			return eventTarget{}, ErrEventTarget
		}
		if err != nil {
			return eventTarget{}, fmt.Errorf("registry: resolve event runtime: %w", err)
		}
		target.generation = int64(event.RuntimeGeneration)
		incoming := int64(event.RuntimeGeneration)
		target.runtimeCurrent = incoming == generation
		target.runtimeHistorical = incoming < generation
		target.runtimeFuture = incoming > generation
		if target.runtimeFuture && event.Kind != "runtime_started" {
			return eventTarget{}, ErrEventTarget
		}
	} else if event.RuntimeGeneration != 0 {
		return eventTarget{}, ErrEventTarget
	}
	if event.SessionID != "" {
		id, _ := uuid.Parse(event.SessionID)
		target.sessionID = &id
		// A discovery event owns its incoming snapshot, so it may introduce the
		// session. All other session-targeted events require an existing session.
		if event.Kind != "session_discovered" {
			var owner, runtime uuid.UUID
			err := tx.QueryRow(ctx, `SELECT worker_id, runtime_id FROM sessions
                WHERE session_id = $1`, id).Scan(&owner, &runtime)
			if errors.Is(err, sql.ErrNoRows) || owner != workerID || target.runtimeID == nil || runtime != *target.runtimeID {
				return eventTarget{}, ErrEventTarget
			}
			if err != nil {
				return eventTarget{}, fmt.Errorf("registry: resolve event session: %w", err)
			}
		}
	}
	return target, nil
}

func nullUUID(raw string) any {
	if raw == "" {
		return nil
	}
	id, _ := uuid.Parse(raw)
	return id
}

func nullGeneration(event protocol.Event) any {
	if event.RuntimeID == "" {
		return nil
	}
	return int64(event.RuntimeGeneration)
}

func (s *Store) applyEvent(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event, target eventTarget) (notify bool, commandID *uuid.UUID, err error) {
	if handled, notify, commandID, err := applyHistoryEvent(ctx, tx, workerID, event, target); handled || err != nil {
		return notify, commandID, err
	}
	// Older generation events remain in events for audit and may resolve their
	// own immutable command, but may not overwrite current runtime/session or
	// approval state.
	if event.Kind == "runtime_started" || event.Kind == "runtime_stopped" || event.Kind == "runtime_failed" || event.Kind == "runtime_degraded" {
		if target.runtimeID == nil {
			return false, nil, ErrEventTarget
		}
		if target.runtimeCurrent || target.runtimeFuture {
			state := map[string]string{"runtime_started": "running", "runtime_stopped": "stopped", "runtime_failed": "failed", "runtime_degraded": "degraded"}[event.Kind]
			if _, err := tx.Exec(ctx, `UPDATE runtimes
                SET generation = $3, state = $4, last_seen_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
                    started_at = CASE WHEN $4 = 'running' THEN (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') ELSE started_at END,
                    stopped_at = CASE WHEN $4 IN ('stopped', 'failed') THEN (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') ELSE stopped_at END
                WHERE runtime_id = $1 AND worker_id = $2 AND generation <= $3`,
				*target.runtimeID, workerID, int64(event.RuntimeGeneration), state); err != nil {
				return false, nil, fmt.Errorf("registry: apply runtime event: %w", err)
			}
			if event.Kind == "runtime_started" {
				if _, err := tx.Exec(ctx, `UPDATE approvals SET state = 'cleared', resolved_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), resolution = $3
                    WHERE worker_id = $1 AND runtime_id = $2 AND state = 'pending' AND runtime_generation < $4`,
					workerID, *target.runtimeID, event.Data, int64(event.RuntimeGeneration)); err != nil {
					return false, nil, fmt.Errorf("registry: clear replaced approvals: %w", err)
				}
			}
			if event.Kind == "runtime_failed" {
				if _, err := tx.Exec(ctx, `UPDATE approvals SET state = 'cleared', resolved_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), resolution = $3
                    WHERE worker_id = $1 AND runtime_id = $2 AND state = 'pending' AND runtime_generation <= $4`,
					workerID, *target.runtimeID, event.Data, int64(event.RuntimeGeneration)); err != nil {
					return false, nil, fmt.Errorf("registry: clear failed runtime approvals: %w", err)
				}
			}
			return event.Kind == "runtime_failed" || event.Kind == "runtime_degraded", nil, nil
		}
		return false, nil, nil
	}

	if event.Kind == "session_discovered" || event.Kind == "session_state_changed" {
		if !target.runtimeCurrent || target.runtimeID == nil {
			return false, nil, nil
		}
		var session protocol.Session
		if err := json.Unmarshal(event.Data, &session); err != nil {
			return false, nil, fmt.Errorf("registry: decode session snapshot: %w", err)
		}
		if err := upsertProtocolSession(ctx, tx, workerID, *target.runtimeID, target.sessionID, session); err != nil {
			return false, nil, err
		}
		return false, nil, nil
	}

	if event.Kind == "approval_requested" || event.Kind == "approval_resolved" || event.Kind == "user_input_requested" {
		if !target.runtimeCurrent || target.runtimeID == nil || target.sessionID == nil {
			return false, nil, nil
		}
		var approval protocol.Approval
		if err := json.Unmarshal(event.Data, &approval); err != nil {
			return false, nil, fmt.Errorf("registry: decode approval: %w", err)
		}
		if err := applyApproval(ctx, tx, workerID, *target.runtimeID, *target.sessionID, int64(event.RuntimeGeneration), event, approval); err != nil {
			return false, nil, err
		}
		if event.Kind == "approval_requested" {
			if err := setSessionWaiting(ctx, tx, *target.sessionID, "waiting_approval"); err != nil {
				return false, nil, err
			}
		}
		if event.Kind == "user_input_requested" {
			if err := setSessionWaiting(ctx, tx, *target.sessionID, "waiting_input"); err != nil {
				return false, nil, err
			}
		}
		return event.Kind != "approval_resolved", nil, nil
	}

	var result protocol.Result
	if err := json.Unmarshal(event.Data, &result); err != nil {
		return false, nil, fmt.Errorf("registry: decode event result: %w", err)
	}
	if event.Kind == "agent_progress_message" && (target.runtimeID == nil || target.sessionID == nil || strings.TrimSpace(result.TurnID) == "" || strings.TrimSpace(result.Text) == "") {
		return false, nil, ErrEventTarget
	}
	originalTarget := target
	if result.CommandID != "" {
		id, err := uuid.Parse(result.CommandID)
		if err != nil {
			return false, nil, ErrEventTarget
		}
		if err := updateCommandOutcome(ctx, tx, id, workerID, event, originalTarget, result); err != nil {
			return false, nil, err
		}
		commandID = &id
	}
	if result.Session != nil && target.runtimeCurrent && target.runtimeID != nil {
		expectedID := target.sessionID
		forked := expectedID != nil && result.Session.ID != expectedID.String()
		if forked {
			if event.Kind != "command_completed" || commandID == nil {
				return false, nil, ErrEventTarget
			}
			var allowed bool
			if err := tx.QueryRow(ctx, `SELECT operation='codex_command' AND json_extract(payload, '$.arguments.codex.name')='fork'
                FROM commands WHERE command_id=$1`, *commandID).Scan(&allowed); err != nil {
				return false, nil, err
			}
			if !allowed {
				return false, nil, ErrEventTarget
			}
			expectedID = nil
		}
		if err := upsertProtocolSession(ctx, tx, workerID, *target.runtimeID, expectedID, *result.Session); err != nil {
			return false, nil, err
		}
		var parseErr error
		target.sessionID, parseErr = uuidPointer(result.Session.ID)
		if parseErr != nil {
			return false, nil, ErrEventTarget
		}
		if commandID != nil && (originalTarget.sessionID == nil || forked) {
			if err := bindCommandSession(ctx, tx, *commandID, *target.sessionID); err != nil {
				return false, nil, err
			}
		}
	}
	if target.runtimeCurrent && target.sessionID != nil {
		state, active, terminal := sessionTransition(event.Kind, result)
		if state != "" {
			if err := setSessionState(ctx, tx, *target.sessionID, state, active, terminal); err != nil {
				return false, nil, err
			}
		}
	}
	notify = notificationRequired(event.Kind) || (event.Kind == "command_completed" && result.Session != nil)
	if !notify && event.Kind == "command_completed" && result.Text != "" && commandID != nil {
		if err := tx.QueryRow(ctx, "SELECT operation='codex_command' FROM commands WHERE command_id=$1", *commandID).Scan(&notify); err != nil {
			return false, nil, err
		}
	}
	return notify, commandID, nil
}

// History responses are private command results. Handle them before any
// session snapshots or turn transitions so reading saved input cannot change
// execution state, even when a malformed worker result names another kind.
func applyHistoryEvent(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event, target eventTarget) (handled, notify bool, commandID *uuid.UUID, err error) {
	var envelope struct {
		CommandID string          `json:"command_id"`
		History   json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(event.Data, &envelope); err != nil {
		return false, false, nil, ErrEventTarget
	}
	hasHistory := len(envelope.History) != 0 && !bytes.Equal(envelope.History, []byte("null"))
	if envelope.CommandID == "" {
		if hasHistory {
			return false, false, nil, ErrEventTarget
		}
		return false, false, nil, nil
	}
	id, parseErr := uuid.Parse(envelope.CommandID)
	if parseErr != nil {
		return false, false, nil, ErrEventTarget
	}
	var operation string
	var payload []byte
	err = tx.QueryRow(ctx, `SELECT operation, payload FROM commands
        WHERE command_id=$1 AND worker_id=$2 AND runtime_id IS $3
          AND runtime_generation=$4 AND session_id IS $5`, id, workerID,
		target.runtimeID, int64(event.RuntimeGeneration), target.sessionID).Scan(&operation, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil, ErrEventTarget
	}
	if err != nil {
		return false, false, nil, fmt.Errorf("registry: resolve history result command: %w", err)
	}
	if protocol.Operation(operation) != protocol.ReadHistory {
		if hasHistory {
			return false, false, nil, ErrEventTarget
		}
		return false, false, nil, nil
	}
	var command protocol.Command
	var result protocol.Result
	if json.Unmarshal(payload, &command) != nil || command.Arguments.History.Validate() != nil || json.Unmarshal(event.Data, &result) != nil || result.Session != nil || result.TurnID != "" {
		return true, false, nil, ErrEventTarget
	}
	switch event.Kind {
	case "command_completed":
		if result.Error != nil || !validHistoryPage(result.History, command.Arguments.History) {
			return true, false, nil, ErrEventTarget
		}
	case "command_failed", "command_result_unknown":
		if hasHistory {
			return true, false, nil, ErrEventTarget
		}
	default:
		return true, false, nil, ErrEventTarget
	}
	if err := updateCommandOutcome(ctx, tx, id, workerID, event, target, result); err != nil {
		return true, false, nil, err
	}
	return true, target.runtimeCurrent, &id, nil
}

func validHistoryPage(page *protocol.HistoryPage, request *protocol.HistoryRequest) bool {
	if page == nil || request.Validate() != nil || page.Limit != request.Limit || len(page.Prompts) > page.Limit {
		return false
	}
	seen := make(map[protocol.HistoryCursor]struct{}, len(page.Prompts))
	total := 0
	for _, prompt := range page.Prompts {
		cursor := protocol.HistoryCursor{TurnID: prompt.TurnID, ItemID: prompt.ItemID}
		if !validHistoryCursor(cursor) || strings.TrimSpace(prompt.Text) == "" || !utf8.ValidString(prompt.Text) {
			return false
		}
		if _, duplicate := seen[cursor]; duplicate || (request.Before != nil && cursor == *request.Before) {
			return false
		}
		seen[cursor] = struct{}{}
		length := utf8.RuneCountInString(prompt.Text)
		total += length
		if length > 16000 || total > 64000 {
			return false
		}
	}
	if page.Next != nil {
		if !validHistoryCursor(*page.Next) || len(page.Prompts) == 0 {
			return false
		}
		first := page.Prompts[0]
		if page.Next.TurnID != first.TurnID || page.Next.ItemID != first.ItemID {
			return false
		}
	}
	return true
}

func validHistoryCursor(cursor protocol.HistoryCursor) bool {
	return strings.TrimSpace(cursor.TurnID) != "" && len(cursor.TurnID) <= 512 &&
		strings.TrimSpace(cursor.ItemID) != "" && len(cursor.ItemID) <= 512
}

func sessionTransition(kind string, result protocol.Result) (state, activeTurn, terminalTurn string) {
	switch kind {
	case "turn_started":
		if result.State != "" && validSessionState(result.State) {
			state = result.State
		}
		if state == "" {
			state = "running"
		}
		return state, result.TurnID, ""
	case "turn_completed", "turn_interrupted":
		if result.State != "" && validSessionState(result.State) {
			state = result.State
		}
		if state == "" {
			state = "idle"
		}
		return state, "", result.TurnID
	case "turn_failed":
		if result.State != "" && validSessionState(result.State) {
			state = result.State
		}
		if state == "" {
			state = "failed"
		}
		return state, "", result.TurnID
	default:
		return state, "", ""
	}
}

func notificationRequired(kind string) bool {
	switch kind {
	case "agent_progress_message", "turn_completed", "turn_interrupted", "approval_requested", "user_input_requested", "turn_failed", "runtime_failed", "runtime_degraded", "command_failed", "command_result_unknown":
		return true
	default:
		return false
	}
}

func updateCommandOutcome(ctx context.Context, tx *dbTx, commandID, workerID uuid.UUID, event protocol.Event, target eventTarget, result protocol.Result) error {
	if target.runtimeID == nil {
		return ErrEventTarget
	}
	status := ""
	switch event.Kind {
	case "turn_started", "turn_completed", "turn_interrupted", "command_completed":
		status = "completed"
	case "turn_failed", "command_failed":
		status = "failed"
	case "command_result_unknown":
		status = "outcome_unknown"
	}
	if status == "" {
		var matching bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands
            WHERE command_id = $1 AND worker_id = $2 AND runtime_id = $3
              AND runtime_generation = $4 AND session_id IS $5)`,
			commandID, workerID, *target.runtimeID, int64(event.RuntimeGeneration), target.sessionID).Scan(&matching)
		if err != nil {
			return fmt.Errorf("registry: verify command event target: %w", err)
		}
		if !matching {
			return ErrEventTarget
		}
		return nil
	}
	var errorCode, errorMessage *string
	if result.Error != nil {
		errorCode, errorMessage = &result.Error.Code, &result.Error.Message
	}
	ct, err := tx.Exec(ctx, `UPDATE commands SET status = $2, completed_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),
        error_code = $3, error_message = $4
        WHERE command_id = $1 AND worker_id = $5 AND runtime_id = $6
          AND runtime_generation = $7 AND session_id IS $8`,
		commandID, status, errorCode, errorMessage, workerID, *target.runtimeID,
		int64(event.RuntimeGeneration), target.sessionID)
	if err != nil {
		return fmt.Errorf("registry: update command outcome: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrEventTarget
	}
	return nil
}

func applyApproval(ctx context.Context, tx *dbTx, workerID, runtimeID, sessionID uuid.UUID, generation int64, event protocol.Event, approval protocol.Approval) error {
	id, err := uuid.Parse(approval.ID)
	if err != nil || approval.RequestID == "" || approval.ThreadID == "" {
		return ErrEventTarget
	}
	if event.Kind == "approval_requested" || event.Kind == "user_input_requested" {
		state := approval.State
		if state == "" {
			state = "pending"
		}
		if !validApprovalState(state) || approval.Type == "" {
			return ErrEventTarget
		}
		ct, err := tx.Exec(ctx, `INSERT INTO approvals
            (approval_id, worker_id, runtime_id, runtime_generation, session_id,
             codex_request_id, codex_thread_id, codex_turn_id, codex_item_id,
             approval_type, request_payload, state, requested_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13)
            ON CONFLICT (runtime_id, runtime_generation, codex_request_id) DO UPDATE SET
                request_payload = EXCLUDED.request_payload, state = EXCLUDED.state
            WHERE approvals.worker_id = EXCLUDED.worker_id AND approvals.session_id = EXCLUDED.session_id`,
			id, workerID, runtimeID, generation, sessionID, approval.RequestID, approval.ThreadID,
			approval.TurnID, approval.ItemID, approval.Type, event.Data, state, event.OccurredAt)
		if err != nil {
			return fmt.Errorf("registry: persist approval: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return ErrEventTarget
		}
		return nil
	}
	state := approval.State
	if state == "" {
		state = "cleared"
	}
	if !validApprovalState(state) {
		return ErrEventTarget
	}
	ct, err := tx.Exec(ctx, `UPDATE approvals SET state = $2, resolved_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), resolution = $3
        WHERE approval_id = $1 AND worker_id = $4 AND runtime_id = $5
          AND runtime_generation = $6 AND session_id = $7`, id, state, event.Data, workerID, runtimeID, generation, sessionID)
	if err != nil {
		return fmt.Errorf("registry: resolve approval: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrEventTarget
	}
	return nil
}

func enqueueEventDeliveries(ctx context.Context, tx *dbTx, eventID uuid.UUID, event protocol.Event, sessionID, runtimeID, commandID *uuid.UUID) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("registry: encode delivery payload: %w", err)
	}
	rows, err := tx.Query(ctx, `WITH targets AS (
        SELECT bot_id, chat_id, message_thread_id FROM telegram_bindings
        WHERE session_id = $1 AND $3 <> 'command_completed'
		  AND NOT EXISTS (SELECT 1 FROM commands WHERE command_id=$4 AND operation='read_history')
        UNION
        SELECT binding.bot_id, binding.chat_id, binding.message_thread_id
        FROM telegram_bindings AS binding
        JOIN sessions AS session ON session.session_id = binding.session_id
        WHERE session.runtime_id = $2 AND $3 IN ('runtime_failed','runtime_degraded')
        UNION
        SELECT telegram_bot_id, telegram_chat_id, COALESCE(telegram_message_thread_id, 0)
        FROM commands WHERE command_id = $4 AND telegram_bot_id IS NOT NULL AND telegram_chat_id IS NOT NULL
        UNION
        SELECT delivery.bot_id, delivery.chat_id, delivery.message_thread_id
        FROM telegram_deliveries delivery JOIN events progress ON progress.event_id=delivery.event_id
        WHERE delivery.kind='agent_progress_message' AND progress.runtime_id=$2
          AND progress.runtime_generation=$5
          AND (($3 IN ('turn_completed','turn_failed','turn_interrupted') AND progress.session_id=$1
                AND json_extract(progress.payload, '$.turn_id')=$6)
               OR $3='runtime_failed')
    ) SELECT bot_id, chat_id, message_thread_id FROM targets`, sessionID, runtimeID, event.Kind, commandID, int64(event.RuntimeGeneration), eventTurnID(event))
	if err != nil {
		return fmt.Errorf("registry: find notification targets: %w", err)
	}
	type deliveryTarget struct {
		botID           string
		chatID, topicID int64
	}
	targets := make([]deliveryTarget, 0)
	for rows.Next() {
		var botID string
		var chatID, topicID int64
		if err := rows.Scan(&botID, &chatID, &topicID); err != nil {
			rows.Close()
			return fmt.Errorf("registry: scan notification target: %w", err)
		}
		targets = append(targets, deliveryTarget{botID, chatID, topicID})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("registry: find notification targets: %w", err)
	}
	rows.Close()
	for _, target := range targets {
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_deliveries
            (delivery_id, event_id, bot_id, chat_id, message_thread_id, kind, payload)
	            VALUES ($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), eventID, target.botID, target.chatID, target.topicID, event.Kind, string(payload)); err != nil {
			return fmt.Errorf("registry: enqueue event delivery: %w", err)
		}
	}
	return nil
}

func validSessionState(state string) bool {
	switch state {
	case "unknown", "idle", "running", "waiting_approval", "waiting_input", "failed", "not_loaded":
		return true
	default:
		return false
	}
}

func validApprovalState(state string) bool {
	switch state {
	case "pending", "approved", "declined", "cancelled", "expired", "cleared":
		return true
	default:
		return false
	}
}

func uuidPointer(raw string) (*uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
