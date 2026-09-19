package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// AdminWorker deliberately omits raw heartbeat metadata. Keep the established
// capitalized field names used by the console's worker controls.
type AdminWorker struct {
	ID           uuid.UUID
	Name         string
	Hostname     string
	OS           string
	Arch         string
	Version      string
	Enabled      bool
	Connectivity string
	ConnectionID *uuid.UUID
	ConnectedAt  *time.Time
	LastSeenAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AdminSession joins the visible session inventory to operational information.
// Stats contain saved conversation summaries, never raw event/command payloads.
type AdminSession struct {
	protocol.Session
	WorkerName         string     `json:"worker_name"`
	RuntimeName        string     `json:"runtime_name"`
	WorkerConnectivity string     `json:"worker_connectivity"`
	CodexVersion       string     `json:"codex_version"`
	RuntimeGeneration  uint64     `json:"runtime_generation"`
	LastSeen           *time.Time `json:"last_seen"`
	DiscoveredAt       time.Time  `json:"discovered_at"`
	LastEventAt        *time.Time `json:"last_event_at"`
	PendingApprovals   int        `json:"pending_approvals"`
	QueuedCommands     int        `json:"queued_commands"`
}

// AdminDashboard exposes an explicit set of operational fields. Raw worker
// metadata, enrollment secrets and durable command/event payloads stay private.
type AdminDashboard struct {
	Workers          []AdminWorker      `json:"workers"`
	Runtimes         []protocol.Runtime `json:"runtimes"`
	Sessions         []AdminSession     `json:"sessions"`
	PendingApprovals int                `json:"pending_approvals"`
	QueuedCommands   int                `json:"queued_commands"`
}

func (s *Store) AdminDashboardSnapshot(ctx context.Context) (AdminDashboard, error) {
	workers, err := s.adminWorkerSnapshot(ctx)
	if err != nil {
		return AdminDashboard{}, err
	}
	runtimes, err := s.RuntimeSnapshot(ctx)
	if err != nil {
		return AdminDashboard{}, err
	}
	sessions, err := s.adminSessionSnapshot(ctx)
	if err != nil {
		return AdminDashboard{}, err
	}
	var approvals, commands int
	if err = s.pool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM approvals WHERE state='pending' AND response_command_id IS NULL),
        (SELECT count(*) FROM commands WHERE status IN ('pending','dispatched','acknowledged'))`).Scan(&approvals, &commands); err != nil {
		return AdminDashboard{}, fmt.Errorf("registry: read admin pending totals: %w", err)
	}
	return AdminDashboard{Workers: workers, Runtimes: runtimes, Sessions: sessions, PendingApprovals: approvals, QueuedCommands: commands}, nil
}

func (s *Store) adminWorkerSnapshot(ctx context.Context) ([]AdminWorker, error) {
	rows, err := s.pool.Query(ctx, `SELECT worker_id, name, COALESCE(hostname, ''), os, arch,
        COALESCE(worker_version, ''), enabled, connectivity, connection_id,
        connected_at, last_seen_at, created_at, updated_at
        FROM workers ORDER BY name, worker_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list admin workers: %w", err)
	}
	defer rows.Close()
	result := make([]AdminWorker, 0)
	for rows.Next() {
		var w AdminWorker
		if err := rows.Scan(&w.ID, &w.Name, &w.Hostname, &w.OS, &w.Arch, &w.Version,
			&w.Enabled, &w.Connectivity, &w.ConnectionID, &w.ConnectedAt, &w.LastSeenAt,
			&w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("registry: scan admin worker: %w", err)
		}
		result = append(result, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list admin workers: %w", err)
	}
	return result, nil
}

// A single projection uses narrow covering indexes for pending counts and the
// latest event. It never decodes historical payloads or issues one query per
// session. The archive predicate matches Telegram's current session inventory;
// reconciliation tombstones (including retired helpers) remain in the database.
const adminSessionQuery = `SELECT s.session_id, s.worker_id, s.runtime_id, s.codex_thread_id,
    COALESCE(s.name, ''), COALESCE(s.preview, ''), COALESCE(s.cwd, ''),
    COALESCE(s.git_branch, ''), COALESCE(s.git_root, ''), s.state,
    COALESCE(s.active_turn_id, ''), s.loaded, s.archived,
    COALESCE(s.last_activity_at, s.last_reconciled_at, s.discovered_at),
    COALESCE(json_extract(s.metadata, '$.stats'), 'null'),
    w.name, r.name, w.connectivity, COALESCE(r.codex_version, ''),
    r.generation, w.last_seen_at, s.discovered_at,
    (SELECT e.occurred_at FROM events e WHERE e.session_id=s.session_id ORDER BY e.occurred_at DESC LIMIT 1),
    (SELECT count(*) FROM approvals a WHERE a.session_id=s.session_id AND a.state='pending' AND a.response_command_id IS NULL),
    (SELECT count(*) FROM commands c WHERE c.session_id=s.session_id AND c.status IN ('pending','dispatched','acknowledged'))
    FROM sessions s
    JOIN workers w ON w.worker_id=s.worker_id
    JOIN runtimes r ON r.runtime_id=s.runtime_id
    WHERE s.archived=FALSE
    ORDER BY s.last_activity_at DESC NULLS LAST, s.session_id`

func (s *Store) adminSessionSnapshot(ctx context.Context) ([]AdminSession, error) {
	rows, err := s.pool.Query(ctx, adminSessionQuery)
	if err != nil {
		return nil, fmt.Errorf("registry: list admin sessions: %w", err)
	}
	defer rows.Close()
	result := make([]AdminSession, 0)
	for rows.Next() {
		var session AdminSession
		var stats []byte
		if err := rows.Scan(&session.ID, &session.WorkerID, &session.RuntimeID, &session.ThreadID,
			&session.Name, &session.Preview, &session.CWD, &session.GitBranch, &session.GitRoot,
			&session.State, &session.ActiveTurnID, &session.Loaded, &session.Archived, &session.UpdatedAt,
			&stats, &session.WorkerName, &session.RuntimeName, &session.WorkerConnectivity,
			&session.CodexVersion, &session.RuntimeGeneration, &session.LastSeen, &session.DiscoveredAt,
			&session.LastEventAt, &session.PendingApprovals, &session.QueuedCommands); err != nil {
			return nil, fmt.Errorf("registry: scan admin session: %w", err)
		}
		session.Stats = decodeSessionStats(stats)
		if session.Stats != nil && session.ActiveTurnID == "" {
			// Terminal events can arrive before the next statistics snapshot.
			// An idle session must not display the previous turn's duration.
			session.Stats.ActiveSince = nil
		}
		result = append(result, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list admin sessions: %w", err)
	}
	return result, nil
}
