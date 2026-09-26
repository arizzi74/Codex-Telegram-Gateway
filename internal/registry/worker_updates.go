package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const workerUpdateRetryAfter = 5 * time.Second

// WorkerUpdateStatus freezes a maintenance notice independently of the active
// session. The renderer bounds and redacts worker names before presentation.
type WorkerUpdateStatus struct {
	WorkerID  string                      `json:"worker_id"`
	RequestID string                      `json:"request_id,omitempty"`
	Name      string                      `json:"name"`
	State     string                      `json:"state"`
	Version   string                      `json:"version,omitempty"`
	ErrorCode string                      `json:"error_code,omitempty"`
	Codex     *protocol.CodexUpdateReport `json:"codex,omitempty"`
}

// CodexSummary formats only the structured, bounded runtime result. Callers
// still apply their normal presentation limits and secret redaction. Keeping
// this separate from State preserves a successful worker update when the
// independent Codex check or installation fails.
func (w WorkerUpdateStatus) CodexSummary() string {
	if w.Codex == nil {
		switch w.State {
		case "completed", "up_to_date", "failed":
			return "Codex runtime: check not reported by this worker."
		default:
			return ""
		}
	}
	r := w.Codex
	status := "result unavailable"
	switch r.State {
	case "up_to_date":
		status = "up to date"
	case "completed":
		status = "updated"
	case "failed":
		switch r.ErrorCode {
		case "check_failed":
			status = "version check failed; see the worker update service logs"
		case "inspection_failed":
			status = "installed or running version could not be verified"
		case "worker_update_failed":
			status = "runtime installation skipped because the worker update failed"
		default:
			status = "runtime update failed; see the worker update service logs"
		}
	case "unsupported":
		status = "automatic updates unavailable for the configured runtimes"
	case "withheld":
		status = "update withheld; this release previously failed validation"
	case "no_runtimes":
		status = "no runtimes configured"
	}
	lines := []string{"Codex runtime: " + status}
	if r.LatestVersion != "" {
		lines = append(lines, "Latest stable: "+r.LatestVersion)
	}
	if !r.CheckedAt.IsZero() {
		lines = append(lines, "Checked: "+r.CheckedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	}
	for _, p := range r.Profiles {
		installed, running := p.InstalledVersion, p.RunningVersion
		if installed == "" {
			installed = "unavailable"
		}
		if running == "" {
			running = "unavailable"
		}
		line := p.ProfileID + " · installed " + installed + " · running " + running
		switch p.Support {
		case "external":
			line += " · externally managed; not updated"
		case "unavailable":
			line += " · update support unavailable"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func acceptWorkerUpdates(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	if strings.TrimSpace(in.Text) != "" || strings.TrimSpace(in.Target) != "" {
		return AcceptResult{View: "error", ErrorCode: "worker_updates_usage"}, nil
	}
	workers, err := queueWorkerUpdates(ctx, tx, &in)
	return AcceptResult{View: "worker_updates", WorkerUpdates: workers}, err
}

// QueueWebUIWorkerUpdates shares the durable Telegram maintenance queue, but
// deliberately creates no Telegram subscription or message. Repeated browser
// requests join the same pending update and do not force a running worker down.
func (s *Store) QueueWebUIWorkerUpdates(ctx context.Context) ([]WorkerUpdateStatus, error) {
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	workers, err := queueWorkerUpdates(ctx, tx, nil)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return workers, nil
}

func queueWorkerUpdates(ctx context.Context, tx *dbTx, subscriber *IncomingUpdate) ([]WorkerUpdateStatus, error) {
	rows, err := tx.Query(ctx, `SELECT worker_id,name,COALESCE(worker_version,'') FROM workers WHERE enabled=TRUE ORDER BY name,worker_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list workers for update: %w", err)
	}
	workers := make([]WorkerUpdateStatus, 0)
	for rows.Next() {
		var worker WorkerUpdateStatus
		if err := rows.Scan(&worker.WorkerID, &worker.Name, &worker.Version); err != nil {
			rows.Close()
			return nil, err
		}
		workers = append(workers, worker)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range workers {
		worker := &workers[index]
		var requestID string
		err := tx.QueryRow(ctx, `SELECT request_id FROM worker_update_requests WHERE worker_id=$1 AND state='pending'`, worker.WorkerID).Scan(&requestID)
		if errors.Is(err, sql.ErrNoRows) {
			requestID = uuid.NewString()
			_, err = tx.Exec(ctx, `INSERT INTO worker_update_requests (request_id,worker_id,worker_name) VALUES ($1,$2,$3)`, requestID, worker.WorkerID, worker.Name)
			worker.State = "queued"
		} else {
			worker.State = "already_queued"
		}
		if err != nil {
			return nil, fmt.Errorf("registry: queue worker update: %w", err)
		}
		worker.RequestID = requestID
		if subscriber != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO worker_update_watchers (request_id,bot_id,user_id,chat_id,message_thread_id)
            VALUES ($1,$2,$3,$4,$5) ON CONFLICT (request_id,bot_id,chat_id,message_thread_id) DO NOTHING`, requestID, subscriber.BotID, subscriber.UserID, subscriber.ChatID, subscriber.TopicID); err != nil {
				return nil, fmt.Errorf("registry: subscribe to worker update: %w", err)
			}
		}
	}
	return workers, nil
}

// WorkerUpdateSnapshot returns only the latest maintenance request per enabled
// worker. It excludes subscriber identities and internal dispatch metadata.
func (s *Store) WorkerUpdateSnapshot(ctx context.Context) ([]WorkerUpdateStatus, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.worker_id,r.request_id,w.name,r.state,COALESCE(r.version,''),COALESCE(r.error_code,''),r.codex_report
        FROM workers w JOIN worker_update_requests r ON r.request_id=(
            SELECT request_id FROM worker_update_requests WHERE worker_id=w.worker_id
            ORDER BY (state='pending') DESC,created_at DESC,rowid DESC LIMIT 1)
        WHERE w.enabled=TRUE ORDER BY w.name,w.worker_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: read worker update status: %w", err)
	}
	defer rows.Close()
	updates := make([]WorkerUpdateStatus, 0)
	for rows.Next() {
		var update WorkerUpdateStatus
		var codex []byte
		if err := rows.Scan(&update.WorkerID, &update.RequestID, &update.Name, &update.State, &update.Version, &update.ErrorCode, &codex); err != nil {
			return nil, err
		}
		if len(codex) != 0 {
			if err := json.Unmarshal(codex, &update.Codex); err != nil {
				return nil, fmt.Errorf("registry: decode worker Codex update report: %w", err)
			}
		}
		updates = append(updates, update)
	}
	return updates, rows.Err()
}

// PendingWorkerUpdates includes requests queued while the worker was offline
// or had no runtimes. Disabled workers cannot receive maintenance requests.
func (s *Store) PendingWorkerUpdates(ctx context.Context, workerID uuid.UUID) ([]protocol.WorkerUpdateRequest, error) {
	if workerID == uuid.Nil {
		return nil, ErrWorkerNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT request.request_id,request.worker_id FROM worker_update_requests request
        JOIN workers worker ON worker.worker_id=request.worker_id
        WHERE request.worker_id=$1 AND request.state='pending' AND worker.enabled=TRUE
          AND request.next_attempt_at <= $2 ORDER BY request.created_at,request.request_id`, workerID, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("registry: list pending worker updates: %w", err)
	}
	defer rows.Close()
	requests := make([]protocol.WorkerUpdateRequest, 0)
	for rows.Next() {
		var request protocol.WorkerUpdateRequest
		if err := rows.Scan(&request.RequestID, &request.WorkerID); err != nil {
			return nil, err
		}
		if err := request.Validate(); err != nil {
			return nil, fmt.Errorf("registry: invalid persisted worker update: %w", err)
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

// MarkWorkerUpdateDispatched leases a due request before sending. A dropped
// frame or acknowledgement retries the same durable identity after five seconds.
func (s *Store) MarkWorkerUpdateDispatched(ctx context.Context, workerID, requestID uuid.UUID) error {
	now := time.Now().UTC()
	changed, err := s.pool.Exec(ctx, `UPDATE worker_update_requests SET next_attempt_at=$3
        WHERE request_id=$1 AND worker_id=$2 AND state='pending' AND next_attempt_at <= $4
          AND EXISTS (SELECT 1 FROM workers WHERE worker_id=$2 AND enabled=TRUE)`, requestID, workerID, now.Add(workerUpdateRetryAfter), now)
	if err != nil {
		return fmt.Errorf("registry: mark worker update dispatched: %w", err)
	}
	if changed.RowsAffected() != 1 {
		return ErrTelegramTarget
	}
	return nil
}

// FailWorkerUpdate reports a terminal gateway-side dispatch rejection, such as
// an older worker which does not advertise maintenance support.
func (s *Store) FailWorkerUpdate(ctx context.Context, workerID, requestID uuid.UUID, code string) error {
	result := protocol.WorkerUpdateResult{RequestID: requestID.String(), State: "failed", ErrorCode: code}
	if err := result.Validate(); err != nil || workerID == uuid.Nil {
		return ErrEventTarget
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := completeWorkerUpdate(ctx, tx, workerID, result); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func applyWorkerUpdateEvent(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event) error {
	if event.RuntimeID != "" || event.RuntimeGeneration != 0 || event.SessionID != "" {
		return ErrEventTarget
	}
	var result protocol.WorkerUpdateResult
	if err := json.Unmarshal(event.Data, &result); err != nil || result.Validate() != nil {
		return ErrEventTarget
	}
	return completeWorkerUpdate(ctx, tx, workerID, result)
}

func completeWorkerUpdate(ctx context.Context, tx *dbTx, workerID uuid.UUID, result protocol.WorkerUpdateResult) error {
	var state, name string
	err := tx.QueryRow(ctx, `SELECT state,worker_name FROM worker_update_requests WHERE request_id=$1 AND worker_id=$2`, result.RequestID, workerID).Scan(&state, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEventTarget
	}
	if err != nil {
		return fmt.Errorf("registry: resolve worker update result: %w", err)
	}
	if state != "pending" {
		// A durable replay or late result cannot notify twice, reopen maintenance,
		// or overwrite the outcome of this request or a later request.
		return nil
	}
	var codex any
	if result.Codex != nil {
		raw, err := json.Marshal(result.Codex)
		if err != nil {
			return fmt.Errorf("registry: encode worker Codex update report: %w", err)
		}
		codex = string(raw)
	}
	if _, err := tx.Exec(ctx, `UPDATE worker_update_requests SET state=$3,version=$4,error_code=$5,codex_report=$6,
		completed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE request_id=$1 AND worker_id=$2`, result.RequestID, workerID, result.State, result.Version, result.ErrorCode, codex); err != nil {
		return fmt.Errorf("registry: complete worker update: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT bot_id,user_id,chat_id,message_thread_id FROM worker_update_watchers WHERE request_id=$1`, result.RequestID)
	if err != nil {
		return fmt.Errorf("registry: list worker update subscribers: %w", err)
	}
	var destinations []IncomingUpdate
	for rows.Next() {
		var destination IncomingUpdate
		if err := rows.Scan(&destination.BotID, &destination.UserID, &destination.ChatID, &destination.TopicID); err != nil {
			rows.Close()
			return err
		}
		destinations = append(destinations, destination)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	response := AcceptResult{View: "worker_update_result", WorkerUpdates: []WorkerUpdateStatus{{WorkerID: workerID.String(), RequestID: result.RequestID, Name: name, State: result.State, Version: result.Version, ErrorCode: result.ErrorCode, Codex: result.Codex}}}
	for _, destination := range destinations {
		if err := queueUIResponse(ctx, tx, destination, response); err != nil {
			return err
		}
	}
	return nil
}
