package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

var ErrWebUICreationNotFound = errors.New("registry: web session creation request not found")

// WebUICreationTarget pins browser work to a worker and the runtime shown when
// its dialog opened. It never resolves a Telegram or browser session selection.
type WebUICreationTarget struct {
	WorkerID          uuid.UUID
	RuntimeID         uuid.UUID
	RuntimeGeneration uint64
	RequestID         uuid.UUID
}

// WebUICreationResult is a typed projection of a validated command outcome.
// Raw command payloads and worker error messages stay private.
type WebUICreationResult struct {
	CommandID         string                  `json:"command_id"`
	WorkerID          string                  `json:"worker_id"`
	RuntimeID         string                  `json:"runtime_id"`
	RuntimeGeneration uint64                  `json:"runtime_generation"`
	Status            string                  `json:"status"`
	Pending           bool                    `json:"pending"`
	ErrorCode         string                  `json:"error_code,omitempty"`
	Message           string                  `json:"message"`
	Workspace         *protocol.WorkspacePage `json:"workspace,omitempty"`
	Session           *protocol.Session       `json:"session,omitempty"`
}

func (s *Store) QueueWebUIWorkspace(ctx context.Context, target WebUICreationTarget, request protocol.WorkspaceRequest) (WebUICreationResult, error) {
	if err := request.Validate(); err != nil {
		return WebUICreationResult{}, err
	}
	if request.Path != "" && !webUIWorkspacePath(request.Path) {
		return WebUICreationResult{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "Enter an absolute folder path or a path starting with ~/ on this worker."}
	}
	return s.queueWebUICreation(ctx, target, protocol.BrowseWorkspace, protocol.Arguments{Workspace: &request})
}

func (s *Store) QueueWebUISessionCreation(ctx context.Context, target WebUICreationTarget, name, cwd string) (WebUICreationResult, error) {
	name, err := protocol.NormalizeSessionName(name)
	if err != nil {
		return WebUICreationResult{}, &protocol.Error{Code: "session_name_invalid", Message: err.Error()}
	}
	workspace := protocol.WorkspaceRequest{Path: cwd}
	if workspace.Validate() != nil || !webUIWorkspacePath(cwd) {
		return WebUICreationResult{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "Enter an absolute folder path or a path starting with ~/ on this worker."}
	}
	// Legacy history permits immediately attaching an empty newly started
	// session on runtimes that have not yet materialized its paginated rollout.
	return s.queueWebUICreation(ctx, target, protocol.NewSession, protocol.Arguments{SessionName: name, CWD: cwd, EnsureWorkspace: true, HistoryMode: "legacy"})
}

func webUIWorkspacePath(path string) bool {
	return filepath.IsAbs(path) || path == "~" || strings.HasPrefix(path, "~/")
}

func (s *Store) queueWebUICreation(ctx context.Context, target WebUICreationTarget, operation protocol.Operation, args protocol.Arguments) (WebUICreationResult, error) {
	if target.WorkerID == uuid.Nil || target.RuntimeID == uuid.Nil || target.RequestID == uuid.Nil || target.RuntimeGeneration == 0 || target.RuntimeGeneration > math.MaxInt64 {
		return WebUICreationResult{}, ErrWebUICreationNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebUICreationResult{}, err
	}
	defer tx.Rollback(ctx)
	previous, err := readWebUICreation(ctx, tx, target.WorkerID, target.RequestID, operation)
	if err == nil {
		var payload []byte
		if err := tx.QueryRow(ctx, `SELECT payload FROM commands WHERE command_id=$1`, target.RequestID).Scan(&payload); err != nil {
			return WebUICreationResult{}, err
		}
		var command protocol.Command
		if json.Unmarshal(payload, &command) != nil || command.RuntimeID != target.RuntimeID.String() || command.RuntimeGeneration != target.RuntimeGeneration || !sameCreationArguments(command.Arguments, args) {
			return WebUICreationResult{}, webUICreationConflict()
		}
		return previous, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrWebUICreationNotFound) {
		return WebUICreationResult{}, err
	}
	var collision bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE command_id=$1)`, target.RequestID).Scan(&collision); err != nil {
		return WebUICreationResult{}, err
	}
	if collision {
		return WebUICreationResult{}, webUICreationConflict()
	}
	var generation uint64
	var enabled, supportsCreation bool
	var connectivity, runtimeState string
	err = tx.QueryRow(ctx, `SELECT runtime.generation,worker.enabled,worker.connectivity,runtime.state,COALESCE(json_extract(worker.heartbeat_metadata,'$.supports_webui_session_creation')=1,FALSE)
	    FROM runtimes runtime JOIN workers worker ON worker.worker_id=runtime.worker_id
	    WHERE runtime.runtime_id=$1 AND runtime.worker_id=$2`, target.RuntimeID, target.WorkerID).
		Scan(&generation, &enabled, &connectivity, &runtimeState, &supportsCreation)
	if errors.Is(err, sql.ErrNoRows) {
		return WebUICreationResult{}, ErrWebUICreationNotFound
	}
	if err != nil {
		return WebUICreationResult{}, err
	}
	if generation != target.RuntimeGeneration {
		return WebUICreationResult{}, &protocol.Error{Code: protocol.StaleRuntime, Message: "The worker runtime changed. Refresh the worker list and start again."}
	}
	if !enabled || connectivity != "online" || runtimeState != "running" {
		return WebUICreationResult{}, &protocol.Error{Code: "worker_unavailable", Message: "The worker must be enabled, connected and running before browsing folders or creating a session."}
	}
	if !supportsCreation {
		return WebUICreationResult{}, &protocol.Error{Code: protocol.UnsupportedOperation, Message: "Update this worker to enable creating Web UI sessions from an exact working directory."}
	}
	now := time.Now().UTC()
	command := protocol.Command{ID: target.RequestID.String(), WorkerID: target.WorkerID.String(), RuntimeID: target.RuntimeID.String(), RuntimeGeneration: target.RuntimeGeneration, Operation: operation, Arguments: args, CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
	if err := command.Validate(); err != nil {
		return WebUICreationResult{}, fmt.Errorf("registry: validate web creation: %w", err)
	}
	if _, err := protocol.NewEnvelope("command", command); err != nil {
		return WebUICreationResult{}, err
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return WebUICreationResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO commands (command_id,source,worker_id,runtime_id,runtime_generation,operation,payload,status,expires_at)
	    VALUES ($1,'webui',$2,$3,$4,$5,$6,'pending',$7)`, target.RequestID, target.WorkerID, target.RuntimeID, int64(target.RuntimeGeneration), string(operation), string(payload), command.ExpiresAt); err != nil {
		return WebUICreationResult{}, err
	}
	result, err := readWebUICreation(ctx, tx, target.WorkerID, target.RequestID, operation)
	if err != nil {
		return WebUICreationResult{}, err
	}
	return result, tx.Commit(ctx)
}

func sameCreationArguments(left, right protocol.Arguments) bool {
	if left.CWD != right.CWD || left.SessionName != right.SessionName || left.CreateDirectory != right.CreateDirectory || left.HistoryMode != right.HistoryMode || left.EnsureWorkspace != right.EnsureWorkspace {
		return false
	}
	if left.Workspace == nil || right.Workspace == nil {
		return left.Workspace == nil && right.Workspace == nil
	}
	return *left.Workspace == *right.Workspace
}

func webUICreationConflict() *protocol.Error {
	return &protocol.Error{Code: "request_conflict", Message: "This request identifier has already been used for different work. Start a new request."}
}

func (s *Store) WebUICreation(ctx context.Context, workerID, requestID uuid.UUID, operation protocol.Operation) (WebUICreationResult, error) {
	return readWebUICreation(ctx, s.pool, workerID, requestID, operation)
}

func readWebUICreation(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, workerID, requestID uuid.UUID, operation protocol.Operation) (WebUICreationResult, error) {
	var result WebUICreationResult
	var code string
	err := db.QueryRow(ctx, `SELECT command_id,worker_id,runtime_id,runtime_generation,status,COALESCE(error_code,'') FROM commands
	    WHERE command_id=$1 AND worker_id=$2 AND source='webui' AND operation=$3 AND session_id IS NULL`, requestID, workerID, string(operation)).
		Scan(&result.CommandID, &result.WorkerID, &result.RuntimeID, &result.RuntimeGeneration, &result.Status, &code)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrWebUICreationNotFound
	}
	if err != nil {
		return result, err
	}
	switch result.Status {
	case "pending", "dispatched", "acknowledged":
		result.Pending = true
		result.Message = "Waiting for the worker to complete the request."
	case "completed":
		var raw []byte
		err := db.QueryRow(ctx, `SELECT payload FROM events WHERE worker_id=$1 AND runtime_id=$2 AND runtime_generation=$3 AND kind='command_completed'
		    AND json_extract(payload,'$.command_id')=$4 ORDER BY event_seq DESC LIMIT 1`, workerID, uuid.MustParse(result.RuntimeID), int64(result.RuntimeGeneration), requestID.String()).Scan(&raw)
		var completed protocol.Result
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		if err != nil || json.Unmarshal(raw, &completed) != nil || completed.CommandID != result.CommandID || completed.Error != nil {
			result.Status, result.ErrorCode, result.Message = "outcome_unknown", "command_outcome_unknown", "The result could not be confirmed. Refresh the worker and session list before trying again."
			break
		}
		if operation == protocol.BrowseWorkspace && completed.Workspace != nil {
			result.Workspace, result.Message = completed.Workspace, "Folder list ready."
		} else if operation == protocol.NewSession && completed.Session != nil && completed.Session.WorkerID == result.WorkerID && completed.Session.RuntimeID == result.RuntimeID && !completed.Session.Archived {
			result.Session, result.Message = completed.Session, "Session created."
		} else {
			result.Status, result.ErrorCode, result.Message = "outcome_unknown", "command_outcome_unknown", "The result could not be confirmed. Refresh the worker and session list before trying again."
		}
	case "expired":
		result.ErrorCode, result.Message = "expired", "The request expired. Check the session list before trying again."
	case "outcome_unknown":
		result.ErrorCode, result.Message = "command_outcome_unknown", "The request outcome is unknown. Refresh the session list and check the worker before trying again."
	default:
		result.ErrorCode, result.Message = webUICreationFailure(code)
	}
	return result, nil
}

func webUICreationFailure(code string) (string, string) {
	switch code {
	case protocol.InvalidWorkspace:
		return code, "The folder is unavailable, outside the worker's allowed roots, or cannot be created. Choose another working directory."
	case protocol.StaleRuntime, protocol.CodexUnavailable:
		return code, "The worker runtime changed or is unavailable. Refresh the worker list and start again."
	case protocol.UnsupportedOperation:
		return code, "Update the worker to enable folder browsing and session creation."
	case protocol.CommandExpired:
		return code, "The request expired. Check the session list before trying again."
	default:
		return "command_failed", "The request failed. Check the worker and session list before trying again."
	}
}
