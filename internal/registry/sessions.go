package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func upsertProtocolSession(ctx context.Context, tx *dbTx, workerID, runtimeID uuid.UUID, expectedID *uuid.UUID, session protocol.Session, generation uint64) error {
	id, err := uuid.Parse(session.ID)
	if err != nil || session.WorkerID != workerID.String() || session.RuntimeID != runtimeID.String() ||
		(expectedID != nil && id != *expectedID) || strings.TrimSpace(session.ThreadID) == "" || !validSessionState(session.State) {
		return ErrEventTarget
	}
	activity := session.UpdatedAt
	if activity.IsZero() {
		activity = time.Now().UTC()
	}
	metadata, err := json.Marshal(struct {
		Stats *protocol.SessionStats `json:"stats,omitempty"`
	}{Stats: session.Stats})
	if err != nil {
		return fmt.Errorf("registry: encode session statistics: %w", err)
	}
	ct, err := tx.Exec(ctx, `INSERT INTO sessions
        (session_id, worker_id, runtime_id, codex_thread_id, name, preview, cwd,
         git_branch, git_root, state, active_turn_id, loaded, archived,
         discovered_at, last_activity_at, last_reconciled_at, metadata)
        VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),
            NULLIF($9,''),$10,NULLIF($11,''),$12,$13,(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),$14,(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),$15)
        ON CONFLICT (session_id) DO UPDATE SET
            codex_thread_id = EXCLUDED.codex_thread_id, name = EXCLUDED.name,
            preview = EXCLUDED.preview, cwd = EXCLUDED.cwd, git_branch = EXCLUDED.git_branch,
            git_root = EXCLUDED.git_root, state = EXCLUDED.state,
            active_turn_id = EXCLUDED.active_turn_id, loaded = EXCLUDED.loaded,
            archived = EXCLUDED.archived, last_activity_at = EXCLUDED.last_activity_at,
            metadata = CASE WHEN json_type(EXCLUDED.metadata, '$.stats')='object'
                AND (json_type(sessions.metadata, '$.stats') IS NOT 'object'
                    OR julianday(json_extract(sessions.metadata, '$.stats.observed_at')) IS NULL
                    OR julianday(json_extract(EXCLUDED.metadata, '$.stats.observed_at')) >= julianday(json_extract(sessions.metadata, '$.stats.observed_at')))
                THEN json_set(sessions.metadata, '$.stats', json_extract(EXCLUDED.metadata, '$.stats')) ELSE sessions.metadata END,
            last_reconciled_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE sessions.worker_id = EXCLUDED.worker_id AND sessions.runtime_id = EXCLUDED.runtime_id
          AND COALESCE(json_extract(sessions.metadata, '$.deleted'), 0) = 0`,
		id, workerID, runtimeID, session.ThreadID, session.Name, session.Preview, session.CWD,
		session.GitBranch, session.GitRoot, session.State, session.ActiveTurnID,
		session.Loaded, session.Archived, activity, json.RawMessage(metadata))
	if err != nil {
		return fmt.Errorf("registry: upsert session: %w", err)
	}
	if ct.RowsAffected() == 0 {
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(json_extract(metadata,'$.deleted'),0)=1 FROM sessions WHERE session_id=$1 AND worker_id=$2 AND runtime_id=$3 AND codex_thread_id=$4`, id, workerID, runtimeID, session.ThreadID).Scan(&deleted)
		if err == nil && deleted {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return ErrEventTarget
	}
	return upsertSessionSettings(ctx, tx, workerID, runtimeID, id, generation, session.Settings)
}

func upsertSessionSettings(ctx context.Context, tx *dbTx, workerID, runtimeID, sessionID uuid.UUID, generation uint64, settings *protocol.SessionSettings) error {
	// Old workers and discovery snapshots without confirmed preferences must
	// not clear a setting. Nor may a carried snapshot cross a runtime restart.
	if settings == nil || settings.RuntimeGeneration != generation {
		return nil
	}
	if settings.Validate() != nil {
		return ErrEventTarget
	}
	_, err := tx.Exec(ctx, `INSERT INTO session_settings (session_id,worker_id,runtime_id,runtime_generation,revision,model,reasoning_effort)
        SELECT $1,$2,$3,$4,$5,$6,$7 FROM runtimes WHERE runtime_id=$3 AND worker_id=$2 AND generation=$4
        ON CONFLICT(session_id) DO UPDATE SET runtime_generation=excluded.runtime_generation,revision=excluded.revision,
          model=excluded.model,reasoning_effort=excluded.reasoning_effort
        WHERE session_settings.worker_id=excluded.worker_id AND session_settings.runtime_id=excluded.runtime_id
          AND (excluded.runtime_generation>session_settings.runtime_generation OR
            (excluded.runtime_generation=session_settings.runtime_generation AND excluded.revision>session_settings.revision))`,
		sessionID, workerID, runtimeID, int64(settings.RuntimeGeneration), int64(settings.Revision), settings.Model, settings.ReasoningEffort)
	if err != nil {
		return fmt.Errorf("registry: save current session settings: %w", err)
	}
	return nil
}

func ensureHistoricalSessionIdentity(ctx context.Context, tx *dbTx, workerID, runtimeID uuid.UUID, expectedID *uuid.UUID, session protocol.Session) error {
	id, err := uuid.Parse(session.ID)
	if err != nil || session.WorkerID != workerID.String() || session.RuntimeID != runtimeID.String() ||
		(expectedID != nil && id != *expectedID) || strings.TrimSpace(session.ThreadID) == "" {
		return ErrEventTarget
	}
	// A replayed snapshot from an older runtime generation may be the only
	// source of this session identity. Keep enough normalized ownership for FK
	// validation, while refusing to overwrite the live generation's snapshot.
	_, err = tx.Exec(ctx, `INSERT INTO sessions
        (session_id, worker_id, runtime_id, codex_thread_id, state, loaded,
         archived, discovered_at, last_reconciled_at, metadata)
        VALUES ($1,$2,$3,$4,'not_loaded',FALSE,FALSE,(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),'{}')
        ON CONFLICT (session_id) DO NOTHING`, id, workerID, runtimeID, session.ThreadID)
	if err != nil {
		return fmt.Errorf("registry: retain historical session identity: %w", err)
	}
	return nil
}

func setSessionWaiting(ctx context.Context, tx *dbTx, sessionID uuid.UUID, state string) error {
	if state != "waiting_approval" && state != "waiting_input" {
		return ErrEventTarget
	}
	ct, err := tx.Exec(ctx, `UPDATE sessions SET state = $2, last_activity_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE session_id = $1`, sessionID, state)
	if err != nil {
		return fmt.Errorf("registry: update waiting session state: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrEventTarget
	}
	return nil
}

func setSessionState(ctx context.Context, tx *dbTx, sessionID uuid.UUID, state, activeTurnID, terminalTurnID string) error {
	if !validSessionState(state) {
		return ErrEventTarget
	}
	if terminalTurnID != "" {
		ct, err := tx.Exec(ctx, `UPDATE sessions SET state = $2,
            active_turn_id = NULL, last_activity_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
            WHERE session_id = $1 AND (active_turn_id IS NULL OR active_turn_id = $3)`, sessionID, state, terminalTurnID)
		if err != nil {
			return fmt.Errorf("registry: update terminal session state: %w", err)
		}
		// An older turn completing after a newer turn started is valid history;
		// it must not clear or downgrade that newer active turn.
		if ct.RowsAffected() == 0 {
			return nil
		}
		return nil
	}
	ct, err := tx.Exec(ctx, `UPDATE sessions SET state = $2,
        active_turn_id = NULLIF($3, ''), last_activity_at = (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE session_id = $1`, sessionID, state, activeTurnID)
	if err != nil {
		return fmt.Errorf("registry: update session state: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrEventTarget
	}
	return nil
}

// bindCommandSession transfers the immutable command's Telegram context to a
// newly returned session only while the selection revision captured at accept
// time is still current. This prevents a late new/fork result from undoing a
// subsequent selection or disconnect.
func bindCommandSession(ctx context.Context, tx *dbTx, commandID, sessionID uuid.UUID) error {
	var (
		botID                   *string
		userID, chatID, topicID *int64
		sourceSession           *uuid.UUID
		operation               string
		payload                 []byte
	)
	err := tx.QueryRow(ctx, `SELECT telegram_bot_id, telegram_user_id,
        telegram_chat_id, telegram_message_thread_id, session_id, operation, payload
        FROM commands WHERE command_id=$1`, commandID).
		Scan(&botID, &userID, &chatID, &topicID, &sourceSession, &operation, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEventTarget
	}
	if err != nil {
		return fmt.Errorf("registry: read automatic Telegram binding command: %w", err)
	}
	// Local commands have no Telegram selection to update.
	if botID == nil || userID == nil || chatID == nil {
		return nil
	}
	var command protocol.Command
	if err := json.Unmarshal(payload, &command); err != nil {
		return fmt.Errorf("registry: decode automatic Telegram binding command: %w", err)
	}
	if command.Arguments.SelectionRevision == nil {
		// Commands accepted before the revision migration conservatively do not
		// change a selection when their result arrives after an upgrade.
		return nil
	}
	isFork := operation == string(protocol.CodexCommand) && command.Arguments.Codex != nil && command.Arguments.Codex.Name == "fork"
	if operation != string(protocol.NewSession) && !isFork {
		return ErrEventTarget
	}
	topic := int64(0)
	if topicID != nil {
		topic = *topicID
	}
	in := IncomingUpdate{BotID: *botID, UserID: *userID, ChatID: *chatID, TopicID: topic}
	current, err := selectionRevision(ctx, tx, in)
	if err != nil {
		return err
	}
	if current != *command.Arguments.SelectionRevision {
		return nil
	}
	if isFork {
		if sourceSession == nil {
			return ErrEventTarget
		}
		var matches bool
		// Match the same exact-topic-then-default selection priority used when
		// the fork command was accepted.
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT session_id=$5 FROM telegram_bindings
            WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3
              AND (message_thread_id=$4 OR ($4 > 0 AND message_thread_id=0))
            ORDER BY (message_thread_id=$4) DESC LIMIT 1), FALSE)`,
			*botID, *userID, *chatID, topic, *sourceSession).Scan(&matches); err != nil {
			return fmt.Errorf("registry: verify fork source selection: %w", err)
		}
		if !matches {
			return nil
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bindings
        (bot_id, user_id, chat_id, message_thread_id, session_id, selected_at)
        VALUES ($1,$2,$3,$4,$5,(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))
        ON CONFLICT (bot_id, user_id, chat_id, message_thread_id) DO UPDATE
		SET session_id = EXCLUDED.session_id, selected_at = EXCLUDED.selected_at`,
		*botID, *userID, *chatID, topic, sessionID); err != nil {
		return fmt.Errorf("registry: bind returned session: %w", err)
	}
	if err := bumpSelectionRevision(ctx, tx, in); err != nil {
		return err
	}
	return reconcileTelegramSelection(ctx, tx, in)
}

// SessionSnapshot returns the normalized session registry state for worker
// reconciliation and user-facing selection. Connectivity remains worker state.
func (s *Store) SessionSnapshot(ctx context.Context) ([]protocol.Session, error) {
	rows, err := s.pool.Query(ctx, `SELECT session_id, worker_id, runtime_id, codex_thread_id,
        COALESCE(name, ''), COALESCE(preview, ''), COALESCE(cwd, ''),
        COALESCE(git_branch, ''), COALESCE(git_root, ''), state,
        COALESCE(active_turn_id, ''), loaded, archived,
        COALESCE(last_activity_at, last_reconciled_at, discovered_at),
        COALESCE(json_extract(metadata, '$.stats'), 'null')
        FROM sessions ORDER BY last_activity_at DESC NULLS LAST, session_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list session snapshot: %w", err)
	}
	defer rows.Close()
	result := make([]protocol.Session, 0)
	for rows.Next() {
		var session protocol.Session
		var id, workerID, runtimeID uuid.UUID
		var stats []byte
		if err := rows.Scan(&id, &workerID, &runtimeID, &session.ThreadID, &session.Name,
			&session.Preview, &session.CWD, &session.GitBranch, &session.GitRoot, &session.State,
			&session.ActiveTurnID, &session.Loaded, &session.Archived, &session.UpdatedAt, &stats); err != nil {
			return nil, fmt.Errorf("registry: scan session snapshot: %w", err)
		}
		session.ID, session.WorkerID, session.RuntimeID = id.String(), workerID.String(), runtimeID.String()
		session.Stats = decodeSessionStats(stats)
		result = append(result, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list session snapshot: %w", err)
	}
	return result, nil
}

// Statistics are optional and read through a typed allowlist. Old workers have
// no metrics, and malformed optional metadata must not hide their sessions.
func decodeSessionStats(raw []byte) *protocol.SessionStats {
	var stats *protocol.SessionStats
	if err := json.Unmarshal(raw, &stats); err != nil {
		return nil
	}
	return stats
}

// RuntimeSnapshot returns the normalized runtime registry state for worker
// reconciliation and operator inspection.
func (s *Store) RuntimeSnapshot(ctx context.Context) ([]protocol.Runtime, error) {
	rows, err := s.pool.Query(ctx, `SELECT runtime_id, worker_id, profile_id, name,
        generation, pid, state, COALESCE(codex_version, ''), COALESCE(default_cwd, '')
        FROM runtimes ORDER BY worker_id, profile_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list runtime snapshot: %w", err)
	}
	defer rows.Close()
	result := make([]protocol.Runtime, 0)
	for rows.Next() {
		var runtime protocol.Runtime
		var id, workerID uuid.UUID
		var generation int64
		var pid *int64
		if err := rows.Scan(&id, &workerID, &runtime.ProfileID, &runtime.Name, &generation,
			&pid, &runtime.State, &runtime.CodexVersion, &runtime.DefaultCWD); err != nil {
			return nil, fmt.Errorf("registry: scan runtime snapshot: %w", err)
		}
		if generation < 0 || generation > math.MaxInt64 {
			return nil, errors.New("registry: invalid runtime generation")
		}
		runtime.ID, runtime.WorkerID, runtime.Generation = id.String(), workerID.String(), uint64(generation)
		if pid != nil {
			if *pid > int64(math.MaxInt) || *pid < int64(math.MinInt) {
				return nil, errors.New("registry: runtime PID exceeds Go int range")
			}
			runtime.PID = int(*pid)
		}
		result = append(result, runtime)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list runtime snapshot: %w", err)
	}
	return result, nil
}

// Permanent deletion is a durable fact about one exact Codex thread. A worker
// restart does not undo it, so confirmed tombstones also apply across runtime
// generations. Preserve descriptive metadata and retain the row for audit FKs.
func applyDeletedSession(ctx context.Context, tx *dbTx, workerID, runtimeID uuid.UUID, expectedID *uuid.UUID, session protocol.Session) error {
	id, err := uuid.Parse(session.ID)
	if err != nil || expectedID == nil || id != *expectedID || session.WorkerID != workerID.String() || session.RuntimeID != runtimeID.String() || session.ThreadID == "" || !session.Deleted || !session.Archived || session.Loaded || session.ActiveTurnID != "" || session.State != "not_loaded" {
		return ErrEventTarget
	}
	ct, err := tx.Exec(ctx, `UPDATE sessions SET archived=TRUE,loaded=FALSE,state='not_loaded',active_turn_id=NULL,last_reconciled_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'),metadata=json_set(metadata,'$.deleted',json('true')) WHERE session_id=$1 AND worker_id=$2 AND runtime_id=$3 AND codex_thread_id=$4`, id, workerID, runtimeID, session.ThreadID)
	if err != nil {
		return fmt.Errorf("registry: record deleted session: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return ErrEventTarget
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_selection_revisions (bot_id,user_id,chat_id,message_thread_id,revision)
  SELECT bot_id,user_id,chat_id,message_thread_id,1 FROM telegram_bindings WHERE session_id=$1
  ON CONFLICT(bot_id,user_id,chat_id,message_thread_id) DO UPDATE SET revision=telegram_selection_revisions.revision+1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM telegram_bindings WHERE session_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state='cleared',resolved_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE session_id=$1 AND state='pending'`, id); err != nil {
		return err
	}
	return nil
}
