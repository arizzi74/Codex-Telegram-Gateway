package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// These expressions are built only from fixed, internal SQL identifiers.
func sessionVisibleSQL(bot, chat, topic, session string) string {
	return `(EXISTS(SELECT 1 FROM sessions visible_session WHERE visible_session.session_id=` + session + ` AND visible_session.archived=FALSE) AND (EXISTS(SELECT 1 FROM telegram_bindings selected WHERE selected.bot_id=` + bot + ` AND selected.chat_id=` + chat + ` AND (selected.message_thread_id=` + topic + ` OR (` + topic + `>0 AND selected.message_thread_id=0 AND NOT EXISTS(SELECT 1 FROM telegram_bindings specific WHERE specific.bot_id=selected.bot_id AND specific.user_id=selected.user_id AND specific.chat_id=selected.chat_id AND specific.message_thread_id=` + topic + `))) AND selected.session_id=` + session + `) OR EXISTS(SELECT 1 FROM telegram_chat_modes mode WHERE mode.bot_id=` + bot + ` AND mode.chat_id=` + chat + ` AND mode.message_thread_id=` + topic + ` AND mode.multi_session=1)))`
}

func eventVisibleSQL() string {
	visible := sessionVisibleSQL("delivery.bot_id", "delivery.chat_id", "delivery.message_thread_id", "event.session_id")
	multi := `EXISTS(SELECT 1 FROM telegram_chat_modes mode WHERE mode.bot_id=delivery.bot_id AND mode.chat_id=delivery.chat_id AND mode.message_thread_id=delivery.message_thread_id AND mode.multi_session=1)`
	return `(event.session_id IS NULL OR (event.kind IN ('command_completed','command_failed') AND EXISTS(SELECT 1 FROM commands command WHERE command.command_id=json_extract(event.payload,'$.command_id') AND command.operation NOT IN ('start_turn','steer','interrupt'))) OR (EXISTS(SELECT 1 FROM sessions visible_session WHERE visible_session.session_id=event.session_id AND visible_session.archived=FALSE) AND (event.kind IN ('approval_requested','user_input_requested') OR (event.kind='user_message' AND ` + multi + `) OR (event.kind<>'user_message' AND ` + visible + `))))`
}

func rememberTelegramContext(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	_, err := tx.Exec(ctx, `INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id) VALUES($1,$2,$3,$4) ON CONFLICT(bot_id,user_id,chat_id,message_thread_id) DO UPDATE SET updated_at=`+sqliteNow, in.BotID, in.UserID, in.ChatID, in.TopicID)
	return err
}

func (s *Store) acceptMultiSession(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT multi_session FROM telegram_chat_modes WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, in.TopicID).Scan(&enabled); err != nil {
		return AcceptResult{}, err
	}
	switch strings.ToLower(strings.TrimSpace(in.Text)) {
	case "":
		enabled = !enabled
	case "on":
		enabled = true
	case "off":
		enabled = false
	default:
		return AcceptResult{View: "error", ErrorCode: "multisession_usage"}, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_chat_modes SET multi_session=$5,updated_at=`+sqliteNow+` WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, in.TopicID, enabled); err != nil {
		return AcceptResult{}, err
	}
	if err := reconcileTelegramSelection(ctx, tx, in); err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{View: "multisession", MultiSession: enabled, UserID: in.UserID}, nil
}

type TelegramSessionAlias struct{ SessionID, Name, Alias string }
type TelegramSessionPresentation struct {
	Name, Alias, Marker string
	MultiSession        bool
}

func sessionAliasBase(name string) string {
	var out strings.Builder
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if separator && out.Len() > 0 {
				out.WriteByte('_')
			}
			out.WriteRune(r)
			separator = false
		} else {
			separator = true
		}
		if out.Len() >= 22 {
			break
		}
	}
	result := strings.Trim(out.String(), "_")
	if result == "" {
		result = "session"
	}
	return result
}

func ensureSessionAliases(ctx context.Context, tx *dbTx) error {
	rows, err := tx.Query(ctx, `SELECT session.session_id,COALESCE(session.name,''),COALESCE(session.preview,''),COALESCE(session.cwd,'') FROM sessions session LEFT JOIN telegram_session_aliases alias ON alias.session_id=session.session_id WHERE session.archived=FALSE AND alias.session_id IS NULL ORDER BY session.discovered_at,session.session_id`)
	if err != nil {
		return err
	}
	var missing []TelegramSessionAlias
	for rows.Next() {
		var item TelegramSessionAlias
		var preview, cwd string
		if err := rows.Scan(&item.SessionID, &item.Name, &preview, &cwd); err != nil {
			rows.Close()
			return err
		}
		item.Name = wizardSessionLabel(item.Name, preview, cwd)
		missing = append(missing, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range missing {
		base := "_" + sessionAliasBase(item.Name)
		alias := base
		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				suffix := fmt.Sprintf("_%x", fnvAlias(item.SessionID+fmt.Sprint(attempt)))
				if len(suffix) > 9 {
					suffix = suffix[:9]
				}
				alias = base + suffix
				if len(alias) > 32 {
					alias = base[:32-len(suffix)] + suffix
				}
			}
			result, err := tx.Exec(ctx, `INSERT INTO telegram_session_aliases(session_id,alias) VALUES($1,$2) ON CONFLICT DO NOTHING`, item.SessionID, alias)
			if err != nil {
				return err
			}
			if result.RowsAffected() > 0 {
				break
			}
		}
	}
	return nil
}
func fnvAlias(s string) uint32 { h := fnv.New32a(); _, _ = h.Write([]byte(s)); return h.Sum32() }

// Aliases are assigned once, remain stable across renames, and are never reused.
func (s *Store) ListTelegramSessionAliases(ctx context.Context, bot string, user, chat, topic int64) ([]TelegramSessionAlias, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_chat_modes WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4)`, bot, user, chat, topic).Scan(&known); err != nil {
		return nil, err
	}
	if !known {
		return nil, ErrTelegramTarget
	}
	if err := ensureSessionAliases(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT session.session_id,COALESCE(session.name,''),COALESCE(session.preview,''),COALESCE(session.cwd,''),alias.alias FROM telegram_session_aliases alias JOIN sessions session ON session.session_id=alias.session_id WHERE session.archived=FALSE ORDER BY alias.alias`)
	if err != nil {
		return nil, err
	}
	var result []TelegramSessionAlias
	for rows.Next() {
		var item TelegramSessionAlias
		var preview, cwd string
		if err := rows.Scan(&item.SessionID, &item.Name, &preview, &cwd, &item.Alias); err != nil {
			rows.Close()
			return nil, err
		}
		item.Name = wizardSessionLabel(item.Name, preview, cwd)
		result = append(result, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) TelegramSessionPresentation(ctx context.Context, bot string, chat, topic int64, sessionID string) (TelegramSessionPresentation, error) {
	var result TelegramSessionPresentation
	var preview, cwd string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(session.name,''),COALESCE(session.preview,''),COALESCE(session.cwd,''),COALESCE(alias.alias,''),EXISTS(SELECT 1 FROM telegram_chat_modes WHERE bot_id=$2 AND chat_id=$3 AND message_thread_id=$4 AND multi_session=1) FROM sessions session LEFT JOIN telegram_session_aliases alias ON alias.session_id=session.session_id WHERE session.session_id=$1`, sessionID, bot, chat, topic).Scan(&result.Name, &preview, &cwd, &result.Alias, &result.MultiSession)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrTelegramTarget
	}
	if err != nil {
		return result, err
	}
	result.Name = wizardSessionLabel(result.Name, preview, cwd)
	markers := []string{"🟥", "🟧", "🟨", "🟩", "🟦", "🟪", "🟫"}
	result.Marker = markers[int(fnvAlias(sessionID))%len(markers)]
	return result, nil
}

func (s *Store) acceptSessionAlias(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	if err := ensureSessionAliases(ctx, tx); err != nil {
		return AcceptResult{}, err
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT alias.session_id FROM telegram_session_aliases alias JOIN sessions session ON session.session_id=alias.session_id WHERE alias.alias=$1 AND session.archived=FALSE`, strings.TrimPrefix(strings.ToLower(strings.TrimSpace(in.Target)), "/")).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrTelegramTarget
	}
	if err != nil {
		return AcceptResult{}, err
	}
	target, err := sessionRoute(ctx, tx, id)
	if err != nil {
		return AcceptResult{}, err
	}
	if strings.TrimSpace(in.Text) == "" && len(in.Images) == 0 {
		if err := setBinding(ctx, tx, in, id); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "selected", SessionID: id.String(), RuntimeID: target.runtimeID.String()}, nil
	}
	if len(in.Images) == 0 {
		if handled, result, err := acceptAliasQuestion(ctx, tx, in, target); handled || err != nil {
			return result, err
		}
	}
	// An explicit-address prompt preserves the currently selected conversation.
	in.imageTarget = &target
	return acceptTextCommand(ctx, tx, in, protocol.StartTurn)
}

// reconcileTelegramSelection retires old progress irrevocably and restores the
// latest two progress views for already-running visible turns. No final is replayed.
func reconcileTelegramSelection(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	if _, err := tx.Exec(ctx, `UPDATE telegram_progress_messages AS progress SET retire_requested=1,next_attempt_at=`+sqliteNow+` WHERE bot_id=$1 AND chat_id=$2 AND message_thread_id=$3 AND status<>'deleted' AND NOT `+sessionVisibleSQL("progress.bot_id", "progress.chat_id", "progress.message_thread_id", "progress.session_id"), in.BotID, in.ChatID, in.TopicID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries AS delivery SET visibility_revoked=1 WHERE bot_id=$1 AND chat_id=$2 AND message_thread_id=$3 AND status IN ('pending','failed','sending') AND EXISTS(SELECT 1 FROM events event WHERE event.event_id=delivery.event_id AND NOT `+eventVisibleSQL()+`)`, in.BotID, in.ChatID, in.TopicID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT event.event_id,event.worker_id,event.runtime_id,event.runtime_generation,event.session_id,event.event_seq,event.kind,event.payload,event.occurred_at FROM events event JOIN sessions session ON session.session_id=event.session_id JOIN runtimes runtime ON runtime.runtime_id=event.runtime_id WHERE event.kind IN ('agent_progress_message','tool_progress_message') AND session.archived=FALSE AND session.state IN ('running','waiting_input','waiting_approval') AND event.runtime_generation=runtime.generation AND json_extract(event.payload,'$.turn_id')=session.active_turn_id AND `+sessionVisibleSQL("$1", "$2", "$3", "event.session_id")+` AND NOT EXISTS(SELECT 1 FROM events newer WHERE newer.session_id=event.session_id AND newer.runtime_generation=event.runtime_generation AND newer.kind=event.kind AND json_extract(newer.payload,'$.turn_id')=session.active_turn_id AND newer.event_seq>event.event_seq) AND NOT EXISTS(SELECT 1 FROM telegram_deliveries pending WHERE pending.event_id=event.event_id AND pending.bot_id=$1 AND pending.chat_id=$2 AND pending.message_thread_id=$3 AND pending.status IN ('pending','failed','sending') AND pending.visibility_revoked=0) AND NOT EXISTS(SELECT 1 FROM telegram_progress_messages shown JOIN telegram_deliveries delivery ON delivery.delivery_id=shown.delivery_id WHERE delivery.event_id=event.event_id AND shown.bot_id=$1 AND shown.chat_id=$2 AND shown.message_thread_id=$3 AND shown.status='pending' AND shown.retire_requested=0)`, in.BotID, in.ChatID, in.TopicID)
	if err != nil {
		return err
	}
	var events []protocol.Event
	for rows.Next() {
		var event protocol.Event
		if err := rows.Scan(&event.ID, &event.WorkerID, &event.RuntimeID, &event.RuntimeGeneration, &event.SessionID, &event.Seq, &event.Kind, &event.Data, &event.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,event_id,bot_id,chat_id,message_thread_id,kind,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.New(), event.ID, in.BotID, in.ChatID, in.TopicID, event.Kind, string(payload)); err != nil {
			return err
		}
	}
	return nil
}

// SuppressTelegramDelivery rechecks visibility after queueing and immediately
// before each Telegram API call. Private command responses and questions retain
// their frozen destinations, independently of the current selection.
func (s *Store) SuppressTelegramDelivery(ctx context.Context, id string) (bool, error) {
	if suppress, err := s.SuppressProgressDelivery(ctx, id); err != nil || suppress {
		return suppress, err
	}
	var suppress, awaitingSelection bool
	err := s.pool.QueryRow(ctx, `SELECT delivery.status IN ('cancelled','sent') OR delivery.visibility_revoked=1 OR NOT `+eventVisibleSQL()+`,
        delivery.kind IN ('agent_progress_message','tool_progress_message') AND `+pendingSelectionConfirmationSQL+`
        FROM telegram_deliveries delivery LEFT JOIN events event ON event.event_id=delivery.event_id WHERE delivery.delivery_id=$1`, id).Scan(&suppress, &awaitingSelection)
	if err != nil || suppress {
		return suppress, err
	}
	if awaitingSelection {
		return false, ErrTelegramSelectionPending
	}
	return false, nil
}

// A session-addressed answer can satisfy exactly one current question. Ambiguous
// requests keep their original buttons/reply routes instead of starting a turn.
func acceptAliasQuestion(ctx context.Context, tx *dbTx, in IncomingUpdate, target routeTarget) (bool, AcceptResult, error) {
	rows, err := tx.Query(ctx, `SELECT approval_id,request_payload,input_answers FROM approvals
        WHERE session_id=$1 AND runtime_id=$2 AND runtime_generation=$3 AND state='pending' AND response_command_id IS NULL
          AND (json_extract(request_payload,'$.async')=1 OR COALESCE(codex_turn_id,'')=$4)
          AND json_array_length(request_payload,'$.questions')>0`, target.sessionID, target.runtimeID, target.generation, target.activeTurnID)
	if err != nil {
		return true, AcceptResult{}, err
	}
	type questionTarget struct {
		approval uuid.UUID
		question string
	}
	var pending []questionTarget
	for rows.Next() {
		var id uuid.UUID
		var request, answersRaw []byte
		if err := rows.Scan(&id, &request, &answersRaw); err != nil {
			rows.Close()
			return true, AcceptResult{}, err
		}
		var approval protocol.Approval
		var answers map[string][]string
		if err := json.Unmarshal(request, &approval); err != nil {
			rows.Close()
			return true, AcceptResult{}, err
		}
		if err := json.Unmarshal(answersRaw, &answers); err != nil {
			rows.Close()
			return true, AcceptResult{}, err
		}
		for _, question := range approval.Questions {
			if len(answers[question.ID]) == 0 {
				pending = append(pending, questionTarget{id, question.ID})
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return true, AcceptResult{}, err
	}
	if len(pending) == 0 {
		return false, AcceptResult{}, nil
	}
	if len(pending) > 1 {
		return true, AcceptResult{View: "error", ErrorCode: "session_answer_ambiguous", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String()}, nil
	}
	result, err := acceptInputApproval(ctx, tx, in, pending[0].approval, pending[0].question, false)
	return true, result, err
}
