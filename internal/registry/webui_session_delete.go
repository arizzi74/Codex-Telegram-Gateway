package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

var ErrWebUIDeletionNotFound = errors.New("registry: web session deletion not found")

// WebUISessionDeletion deliberately exposes no command payload or raw worker
// error. It remains readable after the deleted session leaves the inventory.
type WebUISessionDeletion struct {
	CommandID string `json:"command_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Pending   bool   `json:"pending"`
	Deleted   bool   `json:"deleted"`
	ErrorCode string `json:"error_code,omitempty"`
	Message   string `json:"message"`
}

// QueueWebUISessionDeletion uses the worker's existing durable delete operation.
// The client request UUID is its immutable command UUID, so a lost response can
// be resolved by a read-only lookup, without repeating a destructive request.
func (s *Store) QueueWebUISessionDeletion(ctx context.Context, sessionID, requestID uuid.UUID) (WebUISessionDeletion, error) {
	if sessionID == uuid.Nil || requestID == uuid.Nil {
		return WebUISessionDeletion{}, ErrWebUIDeletionNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebUISessionDeletion{}, err
	}
	defer tx.Rollback(ctx)
	previous, err := readWebUISessionDeletion(ctx, tx, sessionID, requestID)
	if err == nil {
		return previous, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrWebUIDeletionNotFound) {
		return WebUISessionDeletion{}, err
	}
	var collision bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE command_id=$1)`, requestID).Scan(&collision); err != nil {
		return WebUISessionDeletion{}, err
	}
	if collision {
		return WebUISessionDeletion{}, &protocol.Error{Code: "request_conflict", Message: "This request identifier has already been used. Start a new confirmation."}
	}
	var target routeTarget
	var cwd, state, connectivity, runtimeState string
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT session.worker_id,session.runtime_id,runtime.generation,session.codex_thread_id,
        COALESCE(session.active_turn_id,''),COALESCE(session.cwd,''),session.state,worker.enabled,worker.connectivity,runtime.state
        FROM sessions session JOIN workers worker ON worker.worker_id=session.worker_id
        JOIN runtimes runtime ON runtime.runtime_id=session.runtime_id AND runtime.worker_id=session.worker_id
        WHERE session.session_id=$1 AND session.archived=FALSE`, sessionID).
		Scan(&target.workerID, &target.runtimeID, &target.generation, &target.threadID, &target.activeTurnID, &cwd, &state, &enabled, &connectivity, &runtimeState)
	if errors.Is(err, sql.ErrNoRows) {
		return WebUISessionDeletion{}, ErrWebUIDeletionNotFound
	}
	if err != nil {
		return WebUISessionDeletion{}, err
	}
	if !enabled || connectivity != "online" || runtimeState != "running" {
		return WebUISessionDeletion{}, &protocol.Error{Code: "worker_unavailable", Message: "The worker must be enabled, connected and running before deleting this session."}
	}
	if target.activeTurnID != "" || (state != "idle" && state != "not_loaded" && state != "failed" && state != "") {
		return WebUISessionDeletion{}, webUIDeletionBusy()
	}
	var queued bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE worker_id=$1 AND session_id=$2
        AND status IN ('pending','dispatched','acknowledged') AND operation='delete_session')`, target.workerID, sessionID).Scan(&queued); err != nil {
		return WebUISessionDeletion{}, err
	}
	if queued {
		return WebUISessionDeletion{}, webUIDeletionBusy()
	}
	now := time.Now().UTC()
	command := protocol.Command{ID: requestID.String(), WorkerID: target.workerID.String(), RuntimeID: target.runtimeID.String(),
		RuntimeGeneration: uint64(target.generation), SessionID: sessionID.String(), ThreadID: target.threadID, Operation: protocol.DeleteSession,
		Arguments: protocol.Arguments{CWD: cwd}, CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
	if err := command.Validate(); err != nil {
		return WebUISessionDeletion{}, fmt.Errorf("registry: validate web deletion: %w", err)
	}
	if _, err := protocol.NewEnvelope("command", command); err != nil {
		return WebUISessionDeletion{}, err
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return WebUISessionDeletion{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO commands
        (command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status,expires_at)
        VALUES ($1,'webui',$2,$3,$4,$5,'delete_session',$6,'pending',$7)`, requestID, target.workerID, target.runtimeID, target.generation, sessionID, string(payload), command.ExpiresAt); err != nil {
		return WebUISessionDeletion{}, err
	}
	result, err := readWebUISessionDeletion(ctx, tx, sessionID, requestID)
	if err != nil {
		return WebUISessionDeletion{}, err
	}
	return result, tx.Commit(ctx)
}

func webUIDeletionBusy() *protocol.Error {
	return &protocol.Error{Code: protocol.SessionBusy, Message: "This session has a running turn or pending deletion. Wait for it to finish before deleting it."}
}

func (s *Store) WebUISessionDeletion(ctx context.Context, sessionID, requestID uuid.UUID) (WebUISessionDeletion, error) {
	return readWebUISessionDeletion(ctx, s.pool, sessionID, requestID)
}

func readWebUISessionDeletion(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, sessionID, requestID uuid.UUID) (WebUISessionDeletion, error) {
	var result WebUISessionDeletion
	var code string
	err := db.QueryRow(ctx, `SELECT command.command_id,command.session_id,command.status,COALESCE(command.error_code,''),
        session.archived=TRUE AND COALESCE(json_extract(session.metadata,'$.deleted'),0)=1
        FROM commands command JOIN sessions session ON session.session_id=command.session_id
          AND session.worker_id=command.worker_id AND session.runtime_id=command.runtime_id
        WHERE command.command_id=$1 AND command.session_id=$2 AND command.source='webui' AND command.operation='delete_session'`, requestID, sessionID).
		Scan(&result.CommandID, &result.SessionID, &result.Status, &code, &result.Deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrWebUIDeletionNotFound
	}
	if err != nil {
		return result, err
	}
	// Only a successful validated deletion result proves this operation deleted
	// the session. Another client's later deletion must not turn a failed job
	// into an apparent success.
	result.Deleted = result.Deleted && result.Status == "completed"
	switch result.Status {
	case "pending", "dispatched", "acknowledged":
		result.Pending = true
		result.Message = "Deleting the conversation and its child sessions. The working directory and files will be kept."
	case "completed":
		if result.Deleted {
			result.Message = "Session deleted. The working directory and its files were kept."
		} else {
			result.Status, result.ErrorCode = "outcome_unknown", "command_outcome_unknown"
			result.Message = "Deletion could not be confirmed. Refresh the session list and check the worker before trying again."
		}
	case "expired":
		result.ErrorCode, result.Message = "expired", "The deletion request expired before the worker acknowledged it. Check the session list before trying again."
	case "outcome_unknown":
		result.ErrorCode, result.Message = "command_outcome_unknown", "The deletion outcome is unknown. Refresh the session list and check the worker before trying again."
	default:
		result.ErrorCode, result.Message = webUIDeletionFailure(code)
	}
	return result, nil
}

func webUIDeletionFailure(code string) (string, string) {
	switch code {
	case protocol.SessionBusy:
		return code, "The session or a child session has a running turn or pending work. Wait for it to finish before deleting it."
	case protocol.UnsupportedOperation:
		return code, "Update the worker to support session deletion, then try again."
	case protocol.InvalidWorkspace:
		return code, "The working directory changed or is not allowed. Refresh the session list before trying again."
	case protocol.UnknownSession, protocol.StaleRuntime:
		return code, "The session or runtime changed. Refresh the session list before trying again."
	default:
		return "command_failed", "Deletion failed. The session has not been confirmed deleted; check the worker before trying again."
	}
}
