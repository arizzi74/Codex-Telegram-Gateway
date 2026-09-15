package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Callback is a compact server-side callback descriptor. Telegram receives
// only the returned opaque token; all authorization context remains in SQL.
type Callback struct {
	Action, BotID, Decision, QuestionID, Answer string
	UserID, ChatID, TopicID                     int64
	SessionID, RuntimeID, ApprovalID            uuid.UUID
	Generation                                  int64
	ExpiresAt                                   time.Time
}

type callbackPayload struct {
	BotID           string `json:"bot_id"`
	ChatID, TopicID int64
	RuntimeID       string `json:"runtime_id,omitempty"`
	Generation      int64  `json:"generation,omitempty"`
	Decision        string `json:"decision,omitempty"`
	QuestionID      string `json:"question_id,omitempty"`
	Answer          string `json:"answer,omitempty"`
}

func (s *Store) CreateCallback(ctx context.Context, callback Callback) (string, error) {
	if callback.Action == "" || callback.BotID == "" || callback.UserID == 0 || callback.ChatID == 0 || callback.TopicID < 0 || callback.ExpiresAt.IsZero() || !callback.ExpiresAt.After(time.Now()) {
		return "", errors.New("registry: invalid callback")
	}
	payload, err := json.Marshal(callbackPayload{BotID: callback.BotID, ChatID: callback.ChatID, TopicID: callback.TopicID, RuntimeID: uuidText(callback.RuntimeID), Generation: callback.Generation, Decision: callback.Decision, QuestionID: callback.QuestionID, Answer: callback.Answer})
	if err != nil {
		return "", err
	}
	for range 3 {
		token, err := callbackToken()
		if err != nil {
			return "", err
		}
		var sessionID, approvalID any
		if callback.SessionID != uuid.Nil {
			sessionID = callback.SessionID
		}
		if callback.ApprovalID != uuid.Nil {
			approvalID = callback.ApprovalID
		}
		ct, err := s.pool.Exec(ctx, `INSERT INTO telegram_callbacks
            (token, action, telegram_user_id, session_id, approval_id, payload, expires_at)
            VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (token) DO NOTHING`, token, callback.Action, callback.UserID, sessionID, approvalID, string(payload), callback.ExpiresAt)
		if err != nil {
			return "", fmt.Errorf("registry: create callback: %w", err)
		}
		if ct.RowsAffected() == 1 {
			return token, nil
		}
	}
	return "", errors.New("registry: generate unique callback token")
}

func callbackToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cb_" + hex.EncodeToString(buf), nil
}

func uuidText(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func (s *Store) consumeCallback(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	var action string
	var actor int64
	var sessionID, approvalID *uuid.UUID
	var payload []byte
	var expires time.Time
	var used *time.Time
	err := tx.QueryRow(ctx, `SELECT action, telegram_user_id, session_id, approval_id, payload, expires_at, used_at
        FROM telegram_callbacks WHERE token=$1`, in.CallbackToken).
		Scan(&action, &actor, &sessionID, &approvalID, &payload, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: consume callback: %w", err)
	}
	if actor != in.UserID || used != nil || !expires.After(time.Now()) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var context callbackPayload
	if err := json.Unmarshal(payload, &context); err != nil || context.BotID != in.BotID || context.ChatID != in.ChatID || context.TopicID != in.TopicID {
		return AcceptResult{}, ErrCallbackInvalid
	}
	markUsed := func() error {
		ct, err := tx.Exec(ctx, "UPDATE telegram_callbacks SET used_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE token=$1 AND used_at IS NULL", in.CallbackToken)
		if err != nil {
			return fmt.Errorf("registry: mark callback used: %w", err)
		}
		if ct.RowsAffected() != 1 {
			return ErrCallbackInvalid
		}
		return nil
	}
	switch action {
	case "select", "connect":
		if sessionID == nil {
			return AcceptResult{}, ErrCallbackInvalid
		}
		target, err := sessionRoute(ctx, tx, *sessionID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if !callbackMatchesTarget(context, target) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		if err := setBinding(ctx, tx, in, target.sessionID); err != nil {
			return AcceptResult{}, err
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "selected", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String()}, nil
	case "status":
		if sessionID == nil {
			return AcceptResult{}, ErrCallbackInvalid
		}
		target, err := sessionRoute(ctx, tx, *sessionID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if !callbackMatchesTarget(context, target) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "status", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String()}, nil
	case "sessions":
		if context.RuntimeID == "" {
			return AcceptResult{}, ErrCallbackInvalid
		}
		runtime, err := resolveRuntime(ctx, tx, context.RuntimeID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if !callbackMatchesTarget(context, runtime) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "sessions", RuntimeID: runtime.runtimeID.String()}, nil
	case "new":
		if context.RuntimeID == "" {
			return AcceptResult{}, ErrCallbackInvalid
		}
		runtime, err := resolveRuntime(ctx, tx, context.RuntimeID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if !callbackMatchesTarget(context, runtime) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		command, err := createTelegramCommand(ctx, tx, in, runtime, protocol.NewSession, "", "", protocol.Arguments{})
		if err != nil {
			return AcceptResult{}, err
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "queued", RuntimeID: runtime.runtimeID.String(), CommandID: command.ID}, nil
	case "approval", "input", "input_prompt":
	default:
		return AcceptResult{}, ErrCallbackInvalid
	}
	if approvalID == nil || sessionID == nil {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var target routeTarget
	var requestID, threadID, turnID, state string
	var requestPayload []byte
	var claimed *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT worker_id, runtime_id, runtime_generation, session_id,
        codex_request_id, codex_thread_id, COALESCE(codex_turn_id,''), state, request_payload, response_command_id
        FROM approvals WHERE approval_id=$1`, *approvalID).
		Scan(&target.workerID, &target.runtimeID, &target.generation, &target.sessionID, &requestID, &threadID, &turnID, &state, &requestPayload, &claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: lock callback approval: %w", err)
	}
	if state != "pending" || claimed != nil || target.sessionID != *sessionID {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if !callbackMatchesTarget(context, target) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var currentGeneration int64
	err = tx.QueryRow(ctx, "SELECT generation FROM runtimes WHERE runtime_id=$1 AND worker_id=$2", target.runtimeID, target.workerID).Scan(&currentGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: resolve callback runtime: %w", err)
	}
	if currentGeneration != target.generation {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var activeTurn string
	err = tx.QueryRow(ctx, `SELECT COALESCE(active_turn_id,'') FROM sessions WHERE session_id=$1 AND runtime_id=$2`, target.sessionID, target.runtimeID).Scan(&activeTurn)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: resolve callback session: %w", err)
	}
	if turnID != "" && activeTurn != turnID {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var approval protocol.Approval
	if err := json.Unmarshal(requestPayload, &approval); err != nil {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if action == "input_prompt" {
		questionID := context.QuestionID
		if questionID == "" && len(approval.Questions) == 1 {
			questionID = approval.Questions[0].ID
		}
		if !approvalQuestionKnown(approval, questionID) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "input_prompt", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), ApprovalID: approvalID.String(), QuestionID: questionID}, nil
	}
	if action == "input" {
		questionID := context.QuestionID
		if questionID == "" {
			questionID = in.QuestionID
		}
		answer := in
		if context.Answer != "" {
			answer.Text = context.Answer
		}
		result, err := acceptInputApproval(ctx, tx, answer, *approvalID, questionID, false)
		if err != nil {
			return AcceptResult{}, err
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return result, nil
	}
	target.threadID = threadID
	op := protocol.ApprovalResponse
	args := protocol.Arguments{ApprovalID: approvalID.String(), RequestID: requestID, Decision: context.Decision}
	if !approvalDecisionAllowed(approval, context.Decision) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	command, err := createTelegramCommand(ctx, tx, in, target, op, target.sessionID.String(), turnID, args)
	if err != nil {
		return AcceptResult{}, err
	}
	ct, err := tx.Exec(ctx, `UPDATE approvals SET response_command_id=$2
        WHERE approval_id=$1 AND state='pending' AND response_command_id IS NULL`, *approvalID, uuid.MustParse(command.ID))
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: claim approval response: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err := markUsed(); err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{View: "queued", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), CommandID: command.ID}, nil
}

func callbackMatchesTarget(context callbackPayload, target routeTarget) bool {
	return (context.RuntimeID == "" || context.RuntimeID == target.runtimeID.String()) &&
		(context.Generation == 0 || context.Generation == target.generation)
}

func callbackTargetError(err error) error {
	if errors.Is(err, ErrTelegramTarget) {
		return ErrCallbackInvalid
	}
	return err
}

func approvalDecisionAllowed(approval protocol.Approval, decision string) bool {
	if decision == "" {
		return false
	}
	for _, allowed := range approval.Decisions {
		if decision == allowed {
			return true
		}
	}
	return false
}

func approvalQuestionKnown(approval protocol.Approval, questionID string) bool {
	if questionID == "" {
		return false
	}
	for _, question := range approval.Questions {
		if question.ID == questionID {
			return true
		}
	}
	return false
}

// RecordBotMessageRoute preserves explicit reply routing independently of the
// user's current selection.
func (s *Store) RecordBotMessageRoute(ctx context.Context, botID string, chatID, messageID int64, sessionID uuid.UUID, turnID string, approvalID uuid.UUID) error {
	return s.recordBotMessageRoute(ctx, botID, chatID, messageID, sessionID, turnID, approvalID, "")
}

// RecordBotInputRoute records a prompt whose plain Telegram reply should be
// treated as an answer to questionID, before ordinary session routing.
func (s *Store) RecordBotInputRoute(ctx context.Context, botID string, chatID, messageID int64, sessionID uuid.UUID, turnID string, approvalID uuid.UUID, questionID string) error {
	if questionID == "" {
		return errors.New("registry: invalid input question route")
	}
	return s.recordBotMessageRoute(ctx, botID, chatID, messageID, sessionID, turnID, approvalID, questionID)
}

func (s *Store) recordBotMessageRoute(ctx context.Context, botID string, chatID, messageID int64, sessionID uuid.UUID, turnID string, approvalID uuid.UUID, questionID string) error {
	if botID == "" || chatID == 0 || messageID <= 0 || sessionID == uuid.Nil {
		return errors.New("registry: invalid bot message route")
	}
	var approval any
	if approvalID != uuid.Nil {
		approval = approvalID
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bot_message_routes
        (bot_id,chat_id,message_id,session_id,turn_id,approval_id,question_id)
        VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,NULLIF($7,''))
        ON CONFLICT (bot_id,chat_id,message_id) DO UPDATE SET
	        session_id=EXCLUDED.session_id, turn_id=EXCLUDED.turn_id, approval_id=EXCLUDED.approval_id, question_id=EXCLUDED.question_id`, botID, chatID, messageID, sessionID, turnID, approval, questionID)
	if err != nil {
		return fmt.Errorf("registry: record bot message route: %w", err)
	}
	return nil
}
