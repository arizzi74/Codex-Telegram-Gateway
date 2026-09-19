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
	WizardID                                    string
	WizardRevision                              int64
	Path                                        string
	Offset                                      int
	UserID, ChatID, TopicID                     int64
	SessionID, RuntimeID, ApprovalID            uuid.UUID
	Generation                                  int64
	SessionPage                                 int `json:"session_page,omitempty"`
	ExpiresAt                                   time.Time
	History                                     *protocol.HistoryRequest
}

type callbackPayload struct {
	WizardID        string `json:"wizard_id,omitempty"`
	WizardRevision  int64  `json:"wizard_revision,omitempty"`
	Path            string `json:"path,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	BotID           string `json:"bot_id"`
	ChatID, TopicID int64
	RuntimeID       string                   `json:"runtime_id,omitempty"`
	Generation      int64                    `json:"generation,omitempty"`
	SessionPage     int                      `json:"session_page,omitempty"`
	Decision        string                   `json:"decision,omitempty"`
	QuestionID      string                   `json:"question_id,omitempty"`
	Answer          string                   `json:"answer,omitempty"`
	History         *protocol.HistoryRequest `json:"history,omitempty"`
}

func (s *Store) CreateCallback(ctx context.Context, callback Callback) (string, error) {
	if callback.Action == "" || callback.BotID == "" || callback.UserID == 0 || callback.ChatID == 0 || callback.TopicID < 0 || callback.ExpiresAt.IsZero() || !callback.ExpiresAt.After(time.Now()) {
		return "", errors.New("registry: invalid callback")
	}
	if !validSessionPage(callback.Action, callback.SessionPage) {
		return "", errors.New("registry: invalid callback session page")
	}
	if callback.Action == "history" && (callback.SessionID == uuid.Nil || callback.RuntimeID == uuid.Nil || callback.Generation <= 0 || callback.History.Validate() != nil) {
		return "", errors.New("registry: invalid history callback")
	}
	if isWizardCallback(callback.Action) && (callback.WizardID == "" || callback.WizardRevision <= 0 || callback.Offset < 0 || callback.Offset > 1_000_000 || len(callback.Path) > 4096) {
		return "", errors.New("registry: invalid wizard callback")
	}
	payload, err := json.Marshal(callbackPayload{WizardID: callback.WizardID, WizardRevision: callback.WizardRevision, Path: callback.Path, Offset: callback.Offset, BotID: callback.BotID, ChatID: callback.ChatID, TopicID: callback.TopicID, RuntimeID: uuidText(callback.RuntimeID), Generation: callback.Generation, SessionPage: callback.SessionPage, Decision: callback.Decision, QuestionID: callback.QuestionID, Answer: callback.Answer, History: callback.History})
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

func validSessionPage(action string, page int) bool {
	return page >= 0 && page <= 1_000_000 && (page == 0 || action == "sessions" || action == "delete_sessions")
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
	if err := json.Unmarshal(payload, &context); err != nil || context.BotID != in.BotID || context.ChatID != in.ChatID || context.TopicID != in.TopicID || !validSessionPage(action, context.SessionPage) {
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
	if isWizardCallback(action) {
		result, err := s.consumeWizardCallback(ctx, tx, in, action, context, sessionID)
		if err != nil {
			return AcceptResult{}, err
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return result, nil
	}
	switch action {
	case "history":
		if sessionID == nil || context.RuntimeID == "" || context.Generation <= 0 || context.History.Validate() != nil {
			return AcceptResult{}, ErrCallbackInvalid
		}
		target, err := sessionRoute(ctx, tx, *sessionID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if !callbackMatchesTarget(context, target) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		// A page button cannot recover a previous selection by replying to its
		// message. The current topic/default selection must still be the target.
		selection := in
		selection.ReplyToMessageID = 0
		current, err := resolveRoute(ctx, tx, selection)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if current.sessionID != target.sessionID {
			return AcceptResult{}, ErrCallbackInvalid
		}
		result, err := acceptHistoryCommand(ctx, tx, in, target, context.History)
		if err != nil {
			return AcceptResult{}, err
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return result, nil
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
		return AcceptResult{View: "sessions", RuntimeID: runtime.runtimeID.String(), SessionPage: context.SessionPage}, nil
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
		in.Target = runtime.runtimeID.String()
		result, err := s.startSessionWizard(ctx, tx, in, "new")
		if err != nil {
			return AcceptResult{}, err
		}
		if err := markUsed(); err != nil {
			return AcceptResult{}, err
		}
		return result, nil
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
