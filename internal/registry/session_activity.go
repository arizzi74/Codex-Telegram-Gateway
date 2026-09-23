package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var ErrSessionActivityLimit = errors.New("registry: session activity subscriber limit reached")

// SessionActivity contains live indicators and confirmed current settings. It
// never reads conversation histories, event payloads, statistics or text.
type SessionActivity struct {
	SessionID          string `json:"session_id"`
	State              string `json:"state"`
	ActiveTurnID       string `json:"active_turn_id"`
	PendingQuestions   int    `json:"pending_questions"`
	PendingRevision    string `json:"pending_revision"`
	WorkerConnectivity string `json:"worker_connectivity"`
	RuntimeState       string `json:"runtime_state"`
	Model              string `json:"model"`
	ReasoningEffort    string `json:"reasoning_effort"`
	SettingsRevision   string `json:"settings_revision"`
}

type SessionActivitySnapshot struct {
	Revision uint64            `json:"revision"`
	Sessions []SessionActivity `json:"sessions"`
}

// Activity notifications carry indicator rows only. A small process-local ring
// retains short transitions even when subscriber wakeups coalesce. Reconnects
// always start with a fresh snapshot; this is not a durable event replay API.
type SessionActivityEvent struct {
	Revision  uint64           `json:"revision"`
	Event     string           `json:"event"`
	SessionID string           `json:"session_id"`
	Session   *SessionActivity `json:"session"`
}

type SessionActivityUpdates struct {
	Snapshot SessionActivitySnapshot
	Events   []SessionActivityEvent
	Reset    bool
}

const sessionActivityRingSize = 256

type sessionActivityHub struct {
	mu          sync.Mutex
	readMu      sync.Mutex
	revision    uint64
	closed      bool
	subscribers map[chan struct{}]struct{}
	cached      SessionActivitySnapshot
	cacheValid  bool
	events      []SessionActivityEvent
	dropped     uint64
}

// SubscribeSessionActivity subscribes before the caller reads its initial
// snapshot, closing the initial-read race. Signals coalesce without blocking a
// registry commit; viewers only ever need the newest state. The revision is
// monotonic within a gateway process, and starts afresh after gateway restart.
func (s *Store) SubscribeSessionActivity() (<-chan struct{}, func(), error) {
	h := &s.activity
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.subscribers) >= 128 {
		return nil, nil, ErrSessionActivityLimit
	}
	if h.subscribers == nil {
		h.subscribers = make(map[chan struct{}]struct{})
	}
	changes := make(chan struct{}, 1)
	h.subscribers[changes] = struct{}{}
	var once sync.Once
	return changes, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, ok := h.subscribers[changes]; ok {
				delete(h.subscribers, changes)
				close(changes)
				if len(h.subscribers) == 0 {
					h.cacheValid = false
				}
			}
		})
	}, nil
}

func (s *Store) notifySessionActivity() {
	s.publishSessionActivity(nil)
}

func (s *Store) publishSessionActivity(event *SessionActivityEvent) {
	h := &s.activity
	h.mu.Lock()
	defer h.mu.Unlock()
	h.revision++
	if event != nil && len(h.subscribers) > 0 {
		event.Revision = h.revision
		if len(h.events) == sessionActivityRingSize {
			h.dropped = h.events[0].Revision
			copy(h.events, h.events[1:])
			h.events = h.events[:len(h.events)-1]
		}
		h.events = append(h.events, *event)
	}
	for changed := range h.subscribers {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
}

func (s *Store) activityObserved() bool {
	s.activity.mu.Lock()
	defer s.activity.mu.Unlock()
	return len(s.activity.subscribers) > 0
}

func (s *Store) activityRowTx(ctx context.Context, tx *dbTx, sessionID string) (*SessionActivity, error) {
	if sessionID == "" {
		return nil, nil
	}
	row := tx.QueryRow(ctx, sessionActivityColumns+` AND session.session_id=$1`, sessionID)
	session, err := scanSessionActivity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &session, err
}

func sessionActivityChanged(before, after *SessionActivity) bool {
	return (before == nil) != (after == nil) || (before != nil && after != nil && *before != *after)
}

func sessionActivityEventName(before, after *SessionActivity) string {
	if after == nil {
		return "session_removed"
	}
	if before == nil {
		return "session_changed"
	}
	if after.ActiveTurnID != before.ActiveTurnID {
		if after.ActiveTurnID != "" {
			return "turn_started"
		}
		return "turn_ended"
	}
	if after.PendingRevision != before.PendingRevision {
		if after.PendingQuestions > before.PendingQuestions {
			return "question_requested"
		}
		return "question_resolved"
	}
	if after.SettingsRevision != before.SettingsRevision {
		return "session_settings_changed"
	}
	return "session_changed"
}

// SessionActivityEventName describes a reconciled row changed outside worker
// event ingestion, such as a Telegram answer or a worker disconnect.
func SessionActivityEventName(before, after *SessionActivity) string {
	return sessionActivityEventName(before, after)
}

func (s *Store) publishActivityTransition(kind, sessionID string, before, after *SessionActivity) {
	if !sessionActivityChanged(before, after) {
		s.notifySessionActivity()
		return
	}
	name := sessionActivityEventName(before, after)
	if before != nil && after != nil {
		switch kind {
		case "turn_started":
			name = "turn_started"
		case "turn_completed", "turn_interrupted", "turn_failed":
			name = "turn_ended"
		case "approval_requested", "user_input_requested":
			name = "question_requested"
		case "approval_resolved", "user_input_answered":
			name = "question_resolved"
		}
	}
	s.publishSessionActivity(&SessionActivityEvent{Event: name, SessionID: sessionID, Session: after})
}

func (s *Store) closeSessionActivity() {
	h := &s.activity
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for changed := range h.subscribers {
		delete(h.subscribers, changed)
		close(changed)
	}
}

// Activity-worthy events exclude tokens, tool calls and commentary. Updating a
// terminal dashboard must never scale with the streamed conversation volume.
func activityEvent(kind string) bool {
	switch kind {
	case "session_discovered", "session_state_changed", "runtime_started", "runtime_stopped", "runtime_failed", "runtime_degraded",
		"turn_started", "turn_completed", "turn_interrupted", "turn_failed", "approval_requested", "approval_resolved", "user_input_requested", "user_input_answered",
		"command_completed", "command_failed":
		return true
	default:
		return false
	}
}

// Heartbeats often contain unchanged runtime snapshots. Compare just their
// live fields when a browser snapshot exists; steady heartbeats never wake the
// feed or query sessions/approvals. New generations invalidate pending inputs.
func (s *Store) heartbeatActivityChanged(ctx context.Context, tx *dbTx, heartbeat Heartbeat) (bool, error) {
	h := &s.activity
	h.mu.Lock()
	observed := len(h.subscribers) > 0 || h.cacheValid
	h.mu.Unlock()
	if !observed {
		return false, nil
	}
	type status struct {
		ID         string `json:"id"`
		Generation int64  `json:"generation"`
		State      string `json:"state"`
	}
	runtimes := make([]status, 0, len(heartbeat.Runtimes))
	for _, runtime := range heartbeat.Runtimes {
		runtimes = append(runtimes, status{runtime.ID.String(), runtime.Generation, runtime.State})
	}
	raw, err := json.Marshal(runtimes)
	if err != nil {
		return false, err
	}
	var changed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workers WHERE worker_id=$1 AND connectivity<>'online')
      OR EXISTS(SELECT 1 FROM json_each($2) incoming LEFT JOIN runtimes runtime ON runtime.runtime_id=json_extract(incoming.value,'$.id')
        WHERE runtime.runtime_id IS NULL OR (json_extract(incoming.value,'$.generation')>=runtime.generation AND
          (runtime.generation<>json_extract(incoming.value,'$.generation') OR runtime.state<>json_extract(incoming.value,'$.state'))))`, heartbeat.WorkerID, string(raw)).Scan(&changed)
	return changed, err
}

// Only pending approvals are inspected, via approvals_session_pending_idx.
// The visibility and current request predicates match /tgquestions: old runtime
// generations, old blocking turns, and already answered async input are omitted.
const sessionActivityColumns = `SELECT session.session_id,session.state,COALESCE(session.active_turn_id,''),
    CASE WHEN worker.enabled=FALSE THEN 'disabled' ELSE worker.connectivity END,runtime.state,
    COALESCE(settings.model,''),COALESCE(settings.reasoning_effort,''),
    CASE WHEN settings.revision IS NOT NULL THEN CAST(settings.runtime_generation AS TEXT)||':'||CAST(settings.revision AS TEXT) ELSE '' END,
    (SELECT json_group_array(json_object('id',approval.approval_id,'answered',json((
        SELECT json_group_array(answer.key) FROM json_each(approval.input_answers) answer
        WHERE json_array_length(answer.value)>0))))
      FROM approvals approval WHERE approval.session_id=session.session_id
      AND approval.state='pending' AND approval.response_command_id IS NULL
      AND approval.worker_id=session.worker_id AND approval.runtime_id=session.runtime_id
      AND approval.codex_thread_id=session.codex_thread_id AND approval.runtime_generation=runtime.generation
      AND (json_extract(approval.request_payload,'$.async')=1 OR COALESCE(approval.codex_turn_id,'')=''
        OR approval.codex_turn_id=COALESCE(session.active_turn_id,''))
      AND (COALESCE(json_array_length(approval.request_payload,'$.questions'),0)=0 OR EXISTS(
        SELECT 1 FROM json_each(approval.request_payload,'$.questions') question WHERE NOT EXISTS(
          SELECT 1 FROM json_each(approval.input_answers) answer
          WHERE answer.key=json_extract(question.value,'$.id') AND json_array_length(answer.value)>0))))
    FROM sessions session JOIN workers worker ON worker.worker_id=session.worker_id
    JOIN runtimes runtime ON runtime.runtime_id=session.runtime_id AND runtime.worker_id=session.worker_id
    LEFT JOIN session_settings settings ON settings.session_id=session.session_id AND settings.worker_id=session.worker_id
      AND settings.runtime_id=session.runtime_id AND settings.runtime_generation=runtime.generation
    WHERE session.archived=FALSE`

const sessionActivityQuery = sessionActivityColumns + ` ORDER BY session.session_id`

func scanSessionActivity(row interface{ Scan(...any) error }) (SessionActivity, error) {
	var session SessionActivity
	var pending []byte
	if err := row.Scan(&session.SessionID, &session.State, &session.ActiveTurnID, &session.WorkerConnectivity, &session.RuntimeState,
		&session.Model, &session.ReasoningEffort, &session.SettingsRevision, &pending); err != nil {
		return session, err
	}
	var err error
	session.PendingQuestions, session.PendingRevision, err = pendingActivitySignature(pending)
	return session, err
}

// LiveSessionActivity shares one narrow snapshot among all browser viewers.
// There is no periodic database polling: committed activity changes invalidate
// the snapshot, and each coalesced revision is read at most once.
func (s *Store) LiveSessionActivity(ctx context.Context) (SessionActivitySnapshot, error) {
	h := &s.activity
	h.readMu.Lock()
	defer h.readMu.Unlock()
	return s.liveSessionActivity(ctx)
}

func (s *Store) liveSessionActivity(ctx context.Context) (SessionActivitySnapshot, error) {
	h := &s.activity
	h.mu.Lock()
	revision := h.revision
	if h.cacheValid && h.cached.Revision == revision {
		cached := h.cached
		cached.Sessions = append([]SessionActivity{}, cached.Sessions...)
		h.mu.Unlock()
		return cached, nil
	}
	h.mu.Unlock()
	rows, err := s.pool.Query(ctx, sessionActivityQuery)
	if err != nil {
		return SessionActivitySnapshot{}, fmt.Errorf("registry: read live session activity: %w", err)
	}
	defer rows.Close()
	snapshot := SessionActivitySnapshot{Revision: revision, Sessions: make([]SessionActivity, 0)}
	for rows.Next() {
		session, err := scanSessionActivity(rows)
		if err != nil {
			return SessionActivitySnapshot{}, err
		}
		snapshot.Sessions = append(snapshot.Sessions, session)
	}
	if err := rows.Err(); err != nil {
		return SessionActivitySnapshot{}, err
	}
	h.mu.Lock()
	h.cached, h.cacheValid = snapshot, true
	h.mu.Unlock()
	snapshot.Sessions = append([]SessionActivity{}, snapshot.Sessions...)
	return snapshot, nil
}

// SessionActivitySince pairs retained committed edges with the latest narrow
// snapshot. Consumers reconcile that snapshot after edges, covering generic
// invalidations such as accepted answers and connectivity changes. No idle
// polling or additional worker subscriptions are needed.
func (s *Store) SessionActivitySince(ctx context.Context, after uint64) (SessionActivityUpdates, error) {
	h := &s.activity
	h.readMu.Lock()
	defer h.readMu.Unlock()
	snapshot, err := s.liveSessionActivity(ctx)
	if err != nil {
		return SessionActivityUpdates{}, err
	}
	updates := SessionActivityUpdates{Snapshot: snapshot}
	h.mu.Lock()
	defer h.mu.Unlock()
	updates.Reset = after < h.dropped
	if updates.Reset {
		return updates, nil
	}
	for _, event := range h.events {
		if event.Revision <= after || event.Revision > snapshot.Revision {
			continue
		}
		copy := event
		if event.Session != nil {
			row := *event.Session
			copy.Session = &row
		}
		updates.Events = append(updates.Events, copy)
	}
	return updates, nil
}

// Hash identifiers, never prompts or answer values. A replacement request or a
// partial answer changes the signature even when the pending count is equal.
// Sorting makes it independent of SQLite's aggregation/query plan ordering.
func pendingActivitySignature(raw []byte) (int, string, error) {
	var pending []struct {
		ID       string   `json:"id"`
		Answered []string `json:"answered"`
	}
	if err := json.Unmarshal(raw, &pending); err != nil {
		return 0, "", fmt.Errorf("registry: decode pending activity identities: %w", err)
	}
	if len(pending) == 0 {
		return 0, "", nil
	}
	for index := range pending {
		sort.Strings(pending[index].Answered)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	canonical, err := json.Marshal(pending)
	if err != nil {
		return 0, "", err
	}
	hash := sha256.Sum256(canonical)
	return len(pending), hex.EncodeToString(hash[:]), nil
}
