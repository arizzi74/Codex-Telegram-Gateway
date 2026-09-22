package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/telegramcommands"
)

const defaultCommandTTL = time.Hour

// dispatchedCommandRetryAfter bounds how long a dropped acknowledgement can
// suppress a frozen command. Workers must make command IDs idempotent.
const dispatchedCommandRetryAfter = 5 * time.Second

var (
	ErrTelegramTarget  = errors.New("registry: Telegram target is unavailable")
	ErrCallbackInvalid = errors.New("registry: callback is invalid, expired, or already used")
	ErrStaleTurn       = errors.New("registry: active turn is unavailable or changed")
)

// IncomingUpdate is the normalized, authenticated Telegram update supplied by
// the webhook layer. Raw is retained solely for idempotency/audit.
type IncomingUpdate struct {
	BotID, Text, Action, Target, CallbackToken, QuestionID string
	UpdateID, UserID, ChatID, TopicID, ReplyToMessageID    int64
	CallbackMessageID                                      int64
	CommandTTL                                             time.Duration
	Raw                                                    json.RawMessage
	Images                                                 []protocol.Image
	MediaError                                             string
	imageTarget                                            *routeTarget
}

// AcceptResult is a durable UI response descriptor. The Telegram renderer owns
// presentation; the registry only records a bounded view/result code.
type AcceptResult struct {
	PreviousPickerID   string                  `json:"previous_picker_id,omitempty"`
	WorkerUpdates      []WorkerUpdateStatus    `json:"worker_updates,omitempty"`
	MultiSession       bool                    `json:"multi_session,omitempty"`
	WizardID           string                  `json:"wizard_id,omitempty"`
	WizardRevision     int64                   `json:"wizard_revision,omitempty"`
	SessionName        string                  `json:"session_name,omitempty"`
	CWD                string                  `json:"cwd,omitempty"`
	Workspace          *protocol.WorkspacePage `json:"workspace,omitempty"`
	UserID             int64                   `json:"user_id,omitempty"`
	Duplicate          bool                    `json:"duplicate,omitempty"`
	View               string                  `json:"view,omitempty"`
	Action             string                  `json:"action,omitempty"`
	SessionID          string                  `json:"session_id,omitempty"`
	RuntimeID          string                  `json:"runtime_id,omitempty"`
	SessionPage        int                     `json:"session_page,omitempty"`
	CommandID          string                  `json:"command_id,omitempty"`
	ApprovalID         string                  `json:"approval_id,omitempty"`
	QuestionID         string                  `json:"question_id,omitempty"`
	TextReply          bool                    `json:"text_reply,omitempty"`
	ProgressReposition bool                    `json:"progress_reposition,omitempty"`
	ErrorCode          string                  `json:"error_code,omitempty"`
}

// SessionStatus is the compact, current read model used by Telegram status
// rendering. Counts only include work that can still change the session.
type SessionStatus struct {
	SessionID        string
	RuntimeID        string
	PendingApprovals int
	QueuedCommands   int
	LastEventAt      *time.Time
}

// TelegramSessionStatus returns status for an existing, non-archived session.
// It deliberately does not resolve a mutable Telegram selection; callers pass
// the session ID that routing already froze.
func (s *Store) TelegramSessionStatus(ctx context.Context, sessionID uuid.UUID) (SessionStatus, error) {
	if sessionID == uuid.Nil {
		return SessionStatus{}, ErrTelegramTarget
	}
	var status SessionStatus
	err := s.pool.QueryRow(ctx, `SELECT session.session_id, session.runtime_id,
        (SELECT count(*) FROM approvals approval WHERE approval.session_id=session.session_id
            AND approval.state='pending' AND approval.response_command_id IS NULL),
        (SELECT count(*) FROM commands command WHERE command.session_id=session.session_id
            AND command.status IN ('pending','dispatched','acknowledged')),
        (SELECT max(event.occurred_at) FROM events event WHERE event.session_id=session.session_id)
        FROM sessions session WHERE session.session_id=$1 AND session.archived=FALSE`, sessionID).
		Scan(&status.SessionID, &status.RuntimeID, &status.PendingApprovals, &status.QueuedCommands, &status.LastEventAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionStatus{}, ErrTelegramTarget
	}
	if err != nil {
		return SessionStatus{}, fmt.Errorf("registry: read Telegram session status: %w", err)
	}
	return status, nil
}

// HistoryRequester returns the user who requested a private history page, so
// its pagination buttons retain the initiating user's authorization scope.
func (s *Store) HistoryRequester(ctx context.Context, commandID uuid.UUID) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx, `SELECT telegram_user_id FROM commands
        WHERE command_id=$1 AND source='telegram' AND operation='read_history'
          AND telegram_user_id IS NOT NULL`, commandID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && userID == 0) {
		return 0, ErrTelegramTarget
	}
	if err != nil {
		return 0, fmt.Errorf("registry: read history requester: %w", err)
	}
	return userID, nil
}

// PermissionsRequester scopes permission picker buttons to the user who opened
// the menu, including a confirmation menu returned by a previous choice.
func (s *Store) PermissionsRequester(ctx context.Context, commandID uuid.UUID) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx, `SELECT telegram_user_id FROM commands
        WHERE command_id=$1 AND source='telegram' AND operation='codex_command'
          AND json_extract(payload, '$.arguments.codex.name')='permissions'
          AND telegram_user_id IS NOT NULL`, commandID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && userID == 0) {
		return 0, ErrTelegramTarget
	}
	if err != nil {
		return 0, fmt.Errorf("registry: read permissions requester: %w", err)
	}
	return userID, nil
}

// PendingApproval returns the original, validated request for rendering the
// next question. It is absent once a response command has been claimed.
func (s *Store) PendingApproval(ctx context.Context, approvalID uuid.UUID) (protocol.Approval, error) {
	if approvalID == uuid.Nil {
		return protocol.Approval{}, ErrTelegramTarget
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT approval.request_payload`+pendingRequestFrom+` AND approval.approval_id=$1`, approvalID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Approval{}, ErrTelegramTarget
	}
	if err != nil {
		return protocol.Approval{}, fmt.Errorf("registry: read pending approval: %w", err)
	}
	var approval protocol.Approval
	if err := json.Unmarshal(raw, &approval); err != nil {
		return protocol.Approval{}, fmt.Errorf("registry: decode pending approval: %w", err)
	}
	return approval, nil
}

type routeTarget struct {
	workerID, runtimeID, sessionID uuid.UUID
	generation                     int64
	threadID, activeTurnID         string
}

// AcceptTelegram atomically deduplicates a Telegram update, resolves its
// frozen execution target, creates any command, and queues the UI response.
func (s *Store) AcceptTelegram(ctx context.Context, in IncomingUpdate) (AcceptResult, error) {
	if err := validateIncoming(in); err != nil {
		return AcceptResult{}, err
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: begin Telegram accept: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	raw := in.Raw
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	ct, err := tx.Exec(ctx, `INSERT INTO telegram_updates (bot_id, update_id, user_id, chat_id, raw)
        VALUES ($1,$2,$3,$4,$5) ON CONFLICT (bot_id, update_id) DO NOTHING`,
		in.BotID, in.UpdateID, in.UserID, in.ChatID, raw)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: dedupe Telegram update: %w", err)
	}
	if ct.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return AcceptResult{}, fmt.Errorf("registry: commit duplicate update: %w", err)
		}
		return AcceptResult{Duplicate: true}, nil
	}

	if err := rememberTelegramContext(ctx, tx, in); err != nil {
		return AcceptResult{}, err
	}

	var result AcceptResult
	if in.CallbackToken != "" {
		result, err = s.consumeCallback(ctx, tx, in)
	} else if (strings.TrimSpace(in.Action) == "" || strings.EqualFold(strings.TrimSpace(in.Action), "text")) && in.ReplyToMessageID > 0 {
		result, err = s.acceptReplyInput(ctx, tx, in)
	} else if handled, wizardResult, wizardErr := s.acceptWizardText(ctx, tx, in); handled || wizardErr != nil {
		result, err = wizardResult, wizardErr
	} else {
		result, err = s.acceptAction(ctx, tx, in)
	}
	if err != nil {
		if !deterministicTelegramError(err) {
			return AcceptResult{}, err
		}
		result = AcceptResult{View: "error", ErrorCode: telegramErrorCode(err)}
	}
	if err := queueUIResponse(ctx, tx, in, result); err != nil {
		return AcceptResult{}, err
	}
	if result.ProgressReposition {
		if err := repositionTelegramProgress(ctx, tx, in); err != nil {
			return AcceptResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AcceptResult{}, fmt.Errorf("registry: commit Telegram accept: %w", err)
	}
	return result, nil
}

func deterministicTelegramError(err error) bool {
	return errors.Is(err, ErrTelegramTarget) || errors.Is(err, ErrCallbackInvalid) || errors.Is(err, ErrStaleTurn)
}

func telegramErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrCallbackInvalid):
		return "callback_invalid"
	case errors.Is(err, ErrStaleTurn):
		return "stale_turn"
	default:
		return "target_unavailable"
	}
}

func validateIncoming(in IncomingUpdate) error {
	if strings.TrimSpace(in.BotID) == "" || in.UpdateID < 0 || in.UserID == 0 || in.ChatID == 0 || in.TopicID < 0 || in.ReplyToMessageID < 0 || in.CallbackMessageID < 0 {
		return errors.New("registry: invalid Telegram update")
	}
	if len(in.Raw) != 0 && !json.Valid(in.Raw) {
		return errors.New("registry: invalid Telegram update payload")
	}
	return nil
}

func (s *Store) acceptAction(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "media_error" {
		return mediaErrorResult(in.MediaError), nil
	}
	if len(in.Images) > 0 {
		if err := protocol.ValidateImages(in.Images); err != nil {
			for _, image := range in.Images {
				if len(image.Data) > protocol.MaxImageBytes {
					return mediaErrorResult("image_too_large"), nil
				}
			}
			return mediaErrorResult("image_unsupported"), nil
		}
		if action != "" && action != "text" && action != "start_turn" && action != "steer" && action != "session_alias" {
			return mediaErrorResult("image_unsupported"), nil
		}
	}
	switch action {
	case "update_workers":
		return acceptWorkerUpdates(ctx, tx, in)
	case "multisession":
		return s.acceptMultiSession(ctx, tx, in)
	case "session_alias":
		return s.acceptSessionAlias(ctx, tx, in)
	case "questions":
		return AcceptResult{View: "questions", UserID: in.UserID}, nil
	case "unknown_command":
		return AcceptResult{View: "error", ErrorCode: "unknown_command", Action: in.Target}, nil
	case "codex":
		return s.acceptCodexCommand(ctx, tx, in)
	case "history", "last_messages":
		limit, usage := protocol.DefaultHistoryLimit, "history_usage"
		if action == "last_messages" {
			limit, usage = protocol.DefaultLastMessagesLimit, "last_messages_usage"
		}
		if count := strings.TrimSpace(in.Text); count != "" {
			for _, digit := range count {
				if digit < '0' || digit > '9' {
					return AcceptResult{View: "error", ErrorCode: usage}, nil
				}
			}
			var err error
			limit, err = strconv.Atoi(count)
			if err != nil || limit < 1 || limit > protocol.MaxHistoryLimit {
				return AcceptResult{View: "error", ErrorCode: usage}, nil
			}
		}
		target, err := resolveRoute(ctx, tx, in)
		if err != nil {
			return AcceptResult{}, err
		}
		return acceptHistoryCommand(ctx, tx, in, target, &protocol.HistoryRequest{Limit: limit, Messages: true, NewestFirst: action == "history"})
	case "input_command":
		if strings.TrimSpace(in.Text) == "" {
			return AcceptResult{View: "questions", UserID: in.UserID}, nil
		}
		parts := strings.Fields(in.Text)
		if len(parts) < 3 {
			return AcceptResult{View: "error", ErrorCode: "input_usage"}, nil
		}
		in.Target, in.QuestionID = parts[0], parts[1]
		in.Text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(in.Text, parts[0])), parts[1]))
		return acceptInput(ctx, tx, in)
	case "help", "instances":
		return AcceptResult{View: action}, nil
	case "sessions":
		runtime, picker, err := resolveOptionalRuntime(ctx, tx, in.Target)
		if err != nil {
			return AcceptResult{}, err
		}
		if picker {
			return AcceptResult{View: "runtime_picker", Action: "sessions"}, nil
		}
		return AcceptResult{View: "sessions", RuntimeID: runtime.runtimeID.String()}, nil
	case "status":
		var target routeTarget
		var err error
		if strings.TrimSpace(in.Target) != "" {
			target, err = resolveSessionLookup(ctx, tx, in.Target)
		} else {
			target, err = resolveRoute(ctx, tx, in)
		}
		if err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "status", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String()}, nil
	case "select", "connect":
		target, err := resolveSessionLookup(ctx, tx, in.Target)
		if err != nil {
			return AcceptResult{}, err
		}
		if err := setBinding(ctx, tx, in, target.sessionID); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "selected", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String()}, nil
	case "disconnect":
		if err := bumpSelectionRevision(ctx, tx, in); err != nil {
			return AcceptResult{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM telegram_bindings WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, in.TopicID); err != nil {
			return AcceptResult{}, fmt.Errorf("registry: disconnect selection: %w", err)
		}
		if err := reconcileTelegramSelection(ctx, tx, in); err != nil {
			return AcceptResult{}, err
		}
		return AcceptResult{View: "disconnected"}, nil
	case "new", "delete_session":
		kind := "new"
		if action == "delete_session" {
			kind = "delete"
		}
		return s.startSessionWizard(ctx, tx, in, kind)
	case "input":
		return acceptInput(ctx, tx, in)
	case "steer", "interrupt":
		return acceptTextCommand(ctx, tx, in, protocol.Operation(action))
	default:
		return acceptTextCommand(ctx, tx, in, protocol.StartTurn)
	}
}

func acceptHistoryCommand(ctx context.Context, tx *dbTx, in IncomingUpdate, target routeTarget, history *protocol.HistoryRequest) (AcceptResult, error) {
	command, err := createTelegramCommand(ctx, tx, in, target, protocol.ReadHistory, target.sessionID.String(), "", protocol.Arguments{History: history})
	if err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{View: "queued", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), CommandID: command.ID}, nil
}

// Codex commands share the ordinary frozen routing transaction, but have their
// own operation so client commands can never become model prompts by accident.
func (s *Store) acceptCodexCommand(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	if len(in.Text) > 16384 {
		return AcceptResult{View: "error", ErrorCode: "command_too_long"}, nil
	}
	name, ok := telegramcommands.Canonical(in.Target)
	if !ok {
		return AcceptResult{View: "error", ErrorCode: "unknown_command", Action: in.Target}, nil
	}
	switch name {
	case "help":
		return AcceptResult{View: "codex_help"}, nil
	case "quit", "exit":
		in.Action = "disconnect"
		return s.acceptAction(ctx, tx, in)
	case "resume", "agent", "subagents":
		in.Action, in.Target = "connect", strings.TrimSpace(in.Text)
		if in.Target == "" {
			in.Action = "sessions"
		}
		return s.acceptAction(ctx, tx, in)
	case "new", "clear":
		target, err := resolveRoute(ctx, tx, in)
		in.Action, in.Target = "new", ""
		if err == nil {
			in.Target = target.runtimeID.String()
		} else if !errors.Is(err, ErrTelegramTarget) {
			return AcceptResult{}, err
		}
		return s.acceptAction(ctx, tx, in)
	case "delete":
		in.Action, in.Target = "delete_session", ""
		return s.acceptAction(ctx, tx, in)
	}
	target, err := resolveRoute(ctx, tx, in)
	if err != nil {
		return AcceptResult{}, err
	}
	return acceptRoutedCodexCommand(ctx, tx, in, target, name, in.Text)
}

func acceptRoutedCodexCommand(ctx context.Context, tx *dbTx, in IncomingUpdate, target routeTarget, name, args string) (AcceptResult, error) {
	command, err := createTelegramCommand(ctx, tx, in, target, protocol.CodexCommand, target.sessionID.String(), "", protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: name, Args: args}})
	if err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{View: "queued", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), CommandID: command.ID}, nil
}

func acceptTextCommand(ctx context.Context, tx *dbTx, in IncomingUpdate, operation protocol.Operation) (AcceptResult, error) {
	target, err := resolveImageOrCurrentRoute(ctx, tx, in)
	if err != nil {
		return AcceptResult{}, err
	}
	if operation == protocol.StartTurn || operation == protocol.Steer {
		if strings.TrimSpace(in.Text) == "" && len(in.Images) == 0 {
			return AcceptResult{}, ErrTelegramTarget
		}
	}
	if len(in.Images) > 0 {
		supported, err := workerSupportsImageInput(ctx, tx, target.workerID)
		if err != nil {
			return AcceptResult{}, err
		}
		if !supported {
			return mediaErrorResult("image_worker_upgrade"), nil
		}
	}
	expected := ""
	if operation == protocol.Steer || operation == protocol.Interrupt {
		expected = target.activeTurnID
		if expected == "" {
			return AcceptResult{}, ErrStaleTurn
		}
	}
	command, err := createTelegramCommand(ctx, tx, in, target, operation, target.sessionID.String(), expected, protocol.Arguments{Text: in.Text, Images: in.Images})
	if err != nil {
		return AcceptResult{}, err
	}
	return AcceptResult{View: "queued", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), CommandID: command.ID}, nil
}

func acceptInput(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	id, err := uuid.Parse(in.Target)
	if err != nil {
		return AcceptResult{}, ErrTelegramTarget
	}
	var delivered bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bot_message_routes WHERE bot_id=$1 AND chat_id=$2 AND approval_id=$3)`, in.BotID, in.ChatID, id).Scan(&delivered); err != nil {
		return AcceptResult{}, err
	}
	return acceptInputApproval(ctx, tx, in, id, in.QuestionID, !delivered)
}

// acceptReplyInput gives a reply to an input request precedence over normal
// session routing. The route is recorded when the question is delivered.
func (s *Store) acceptReplyInput(ctx context.Context, tx *dbTx, in IncomingUpdate) (AcceptResult, error) {
	if err := awaitTelegramReplyRoute(ctx, tx, in); err != nil {
		return AcceptResult{}, err
	}
	var approvalID *uuid.UUID
	var questionID *string
	err := tx.QueryRow(ctx, `SELECT approval_id, question_id FROM bot_message_routes
        WHERE bot_id=$1 AND chat_id=$2 AND message_id=$3`, in.BotID, in.ChatID, in.ReplyToMessageID).
		Scan(&approvalID, &questionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, fmt.Errorf("registry: resolve input reply route: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) || approvalID == nil {
		if handled, result, err := s.acceptWizardText(ctx, tx, in); handled || err != nil {
			return result, err
		}
		return s.acceptAction(ctx, tx, in)
	}
	if len(in.Images) > 0 {
		return mediaErrorResult("image_input_reply"), nil
	}
	question := in.QuestionID
	if question == "" && questionID != nil {
		question = *questionID
	}
	return acceptInputApproval(ctx, tx, in, *approvalID, question, false)
}

func acceptInputApproval(ctx context.Context, tx *dbTx, in IncomingUpdate, id uuid.UUID, questionID string, requireBinding bool) (AcceptResult, error) {
	var target routeTarget
	var requestID, threadID, turnID string
	var state string
	var requestPayload, persistedAnswers []byte
	var claimed *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT approval.worker_id, approval.runtime_id, approval.runtime_generation,
        approval.session_id, approval.codex_request_id, approval.codex_thread_id, COALESCE(approval.codex_turn_id,''),
        approval.state, approval.request_payload, approval.response_command_id, approval.input_answers
        FROM approvals AS approval WHERE approval.approval_id = $1`, id).
		Scan(&target.workerID, &target.runtimeID, &target.generation, &target.sessionID, &requestID, &threadID, &turnID, &state, &requestPayload, &claimed, &persistedAnswers)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrTelegramTarget
	}
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: resolve input request: %w", err)
	}
	if state != "pending" || claimed != nil {
		return AcceptResult{}, ErrTelegramTarget
	}
	if requireBinding {
		matches, err := bindingMatches(ctx, tx, in, target.sessionID)
		if err != nil {
			return AcceptResult{}, err
		}
		if !matches {
			return AcceptResult{}, ErrTelegramTarget
		}
	}
	var approval protocol.Approval
	if err := json.Unmarshal(requestPayload, &approval); err != nil || len(approval.Questions) == 0 || strings.TrimSpace(approval.Questions[0].ID) == "" {
		return AcceptResult{}, ErrTelegramTarget
	}
	if approval.Async {
		turnID = ""
	}
	if err := inputApprovalCurrent(ctx, tx, target, turnID); err != nil {
		return AcceptResult{}, err
	}
	answers, complete, err := mergeInputAnswer(approval, persistedAnswers, questionID, in.Text)
	if err != nil {
		return AcceptResult{}, err
	}
	answersJSON, err := json.Marshal(answers)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: encode input answers: %w", err)
	}
	if err := preserveInputQuestions(ctx, tx, id, approval.Questions); err != nil {
		return AcceptResult{}, err
	}
	if err := recordInputAnswerSummaries(ctx, tx, id, answers, true); err != nil {
		return AcceptResult{}, err
	}
	target.threadID = threadID
	if !complete {
		if _, err := tx.Exec(ctx, "UPDATE approvals SET input_answers=$2 WHERE approval_id=$1", id, string(answersJSON)); err != nil {
			return AcceptResult{}, fmt.Errorf("registry: persist partial input answers: %w", err)
		}
		return AcceptResult{View: "input_pending", UserID: in.UserID, SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), ApprovalID: id.String(), QuestionID: nextUnansweredQuestion(approval, answers), ProgressReposition: true}, nil
	}
	command, err := createTelegramCommand(ctx, tx, in, target, protocol.InputResponse, target.sessionID.String(), turnID, protocol.Arguments{ApprovalID: id.String(), RequestID: requestID, Answers: answers})
	if err != nil {
		return AcceptResult{}, err
	}
	ct, err := tx.Exec(ctx, `UPDATE approvals SET response_command_id=$2
		, input_answers=$3 WHERE approval_id=$1 AND state='pending' AND response_command_id IS NULL`, id, uuid.MustParse(command.ID), string(answersJSON))
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: claim input response: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return AcceptResult{}, ErrTelegramTarget
	}
	return AcceptResult{View: "queued", SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), CommandID: command.ID, ApprovalID: id.String(), ProgressReposition: true}, nil
}

func inputApprovalCurrent(ctx context.Context, tx *dbTx, target routeTarget, turnID string) error {
	var generation int64
	err := tx.QueryRow(ctx, "SELECT generation FROM runtimes WHERE runtime_id=$1 AND worker_id=$2", target.runtimeID, target.workerID).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTelegramTarget
	}
	if err != nil {
		return fmt.Errorf("registry: resolve input runtime: %w", err)
	}
	if generation != target.generation {
		return ErrTelegramTarget
	}
	var activeTurn string
	err = tx.QueryRow(ctx, `SELECT COALESCE(active_turn_id,'') FROM sessions WHERE session_id=$1 AND runtime_id=$2 AND worker_id=$3 AND archived=FALSE`, target.sessionID, target.runtimeID, target.workerID).Scan(&activeTurn)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTelegramTarget
	}
	if err != nil {
		return fmt.Errorf("registry: resolve input session: %w", err)
	}
	if turnID != "" && activeTurn != turnID {
		return ErrStaleTurn
	}
	return nil
}

func mergeInputAnswer(approval protocol.Approval, persisted json.RawMessage, questionID, text string) (map[string][]string, bool, error) {
	if strings.TrimSpace(text) == "" {
		return nil, false, ErrTelegramTarget
	}
	questionID = strings.TrimSpace(questionID)
	if questionID == "" && len(approval.Questions) == 1 {
		questionID = approval.Questions[0].ID
	}
	if questionID == "" {
		return nil, false, ErrTelegramTarget
	}
	answers := make(map[string][]string)
	if len(persisted) > 0 && string(persisted) != "null" {
		if err := json.Unmarshal(persisted, &answers); err != nil {
			return nil, false, fmt.Errorf("registry: decode persisted input answers: %w", err)
		}
	}
	valid := false
	questionIDs := make(map[string]struct{}, len(approval.Questions))
	for _, question := range approval.Questions {
		id := strings.TrimSpace(question.ID)
		if id == "" {
			return nil, false, ErrTelegramTarget
		}
		if _, duplicate := questionIDs[id]; duplicate {
			return nil, false, ErrTelegramTarget
		}
		questionIDs[id] = struct{}{}
		if id == questionID {
			valid = true
		}
	}
	if !valid {
		return nil, false, ErrTelegramTarget
	}
	answers[questionID] = []string{text}
	complete := true
	for id := range questionIDs {
		if len(answers[id]) == 0 {
			complete = false
		}
	}
	return answers, complete, nil
}

func nextUnansweredQuestion(approval protocol.Approval, answers map[string][]string) string {
	for _, question := range approval.Questions {
		if len(answers[question.ID]) == 0 {
			return question.ID
		}
	}
	return ""
}

func resolveRoute(ctx context.Context, tx *dbTx, in IncomingUpdate) (routeTarget, error) {
	if err := awaitTelegramReplyRoute(ctx, tx, in); err != nil {
		return routeTarget{}, err
	}
	if in.TopicID > 0 {
		if target, ok, err := bindingRoute(ctx, tx, in, in.TopicID); err != nil {
			return routeTarget{}, err
		} else if ok {
			return target, nil
		}
	}
	if in.ReplyToMessageID > 0 {
		var sessionID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT session_id FROM bot_message_routes WHERE bot_id=$1 AND chat_id=$2 AND message_id=$3`, in.BotID, in.ChatID, in.ReplyToMessageID).Scan(&sessionID)
		if err == nil {
			return sessionRoute(ctx, tx, sessionID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return routeTarget{}, fmt.Errorf("registry: resolve reply route: %w", err)
		}
	}
	target, ok, err := bindingRoute(ctx, tx, in, 0)
	if err != nil {
		return routeTarget{}, err
	}
	if !ok {
		return routeTarget{}, ErrTelegramTarget
	}
	return target, nil
}

func bindingRoute(ctx context.Context, tx *dbTx, in IncomingUpdate, topicID int64) (routeTarget, bool, error) {
	var sessionID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, topicID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return routeTarget{}, false, nil
	}
	if err != nil {
		return routeTarget{}, false, fmt.Errorf("registry: resolve binding: %w", err)
	}
	target, err := sessionRoute(ctx, tx, sessionID)
	return target, err == nil, err
}

func sessionRoute(ctx context.Context, tx *dbTx, sessionID uuid.UUID) (routeTarget, error) {
	var target routeTarget
	err := tx.QueryRow(ctx, `SELECT session.worker_id, session.runtime_id, runtime.generation,
        session.codex_thread_id, COALESCE(session.active_turn_id, '')
        FROM sessions AS session JOIN runtimes AS runtime ON runtime.runtime_id=session.runtime_id
        WHERE session.session_id=$1 AND session.archived=FALSE`, sessionID).
		Scan(&target.workerID, &target.runtimeID, &target.generation, &target.threadID, &target.activeTurnID)
	if errors.Is(err, sql.ErrNoRows) {
		return routeTarget{}, ErrTelegramTarget
	}
	if err != nil {
		return routeTarget{}, fmt.Errorf("registry: resolve session route: %w", err)
	}
	target.sessionID = sessionID
	return target, nil
}

func resolveRuntime(ctx context.Context, tx *dbTx, raw string) (routeTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return routeTarget{}, ErrTelegramTarget
	}
	rows, err := tx.Query(ctx, `SELECT runtime_id, worker_id, generation FROM runtimes
        WHERE runtime_id=$1 OR profile_id=$1 OR name=$1`, raw)
	if err != nil {
		return routeTarget{}, fmt.Errorf("registry: resolve runtime: %w", err)
	}
	defer rows.Close()
	var found []routeTarget
	for rows.Next() {
		var r routeTarget
		if err := rows.Scan(&r.runtimeID, &r.workerID, &r.generation); err != nil {
			return routeTarget{}, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return routeTarget{}, err
	}
	if len(found) != 1 {
		return routeTarget{}, ErrTelegramTarget
	}
	return found[0], nil
}

func resolveOptionalRuntime(ctx context.Context, tx *dbTx, raw string) (routeTarget, bool, error) {
	if strings.TrimSpace(raw) != "" {
		runtime, err := resolveRuntime(ctx, tx, raw)
		return runtime, false, err
	}
	rows, err := tx.Query(ctx, `SELECT runtime_id, worker_id, generation FROM runtimes ORDER BY worker_id, profile_id`)
	if err != nil {
		return routeTarget{}, false, fmt.Errorf("registry: list runtimes: %w", err)
	}
	defer rows.Close()
	var runtimes []routeTarget
	for rows.Next() {
		var runtime routeTarget
		if err := rows.Scan(&runtime.runtimeID, &runtime.workerID, &runtime.generation); err != nil {
			return routeTarget{}, false, err
		}
		runtimes = append(runtimes, runtime)
	}
	if err := rows.Err(); err != nil {
		return routeTarget{}, false, err
	}
	if len(runtimes) == 1 {
		return runtimes[0], false, nil
	}
	if len(runtimes) > 1 {
		return routeTarget{}, true, nil
	}
	return routeTarget{}, false, ErrTelegramTarget
}

func resolveSessionLookup(ctx context.Context, tx *dbTx, raw string) (routeTarget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return routeTarget{}, ErrTelegramTarget
	}
	rows, err := tx.Query(ctx, `SELECT session.session_id, session.worker_id, session.runtime_id,
        runtime.generation, session.codex_thread_id, COALESCE(session.active_turn_id,'')
        FROM sessions AS session JOIN runtimes AS runtime ON runtime.runtime_id=session.runtime_id
        WHERE session.archived=FALSE AND (session.session_id=$1 OR session.codex_thread_id=$1 OR session.name=$1)`, raw)
	if err != nil {
		return routeTarget{}, fmt.Errorf("registry: look up session: %w", err)
	}
	defer rows.Close()
	var matches []routeTarget
	for rows.Next() {
		var target routeTarget
		if err := rows.Scan(&target.sessionID, &target.workerID, &target.runtimeID, &target.generation, &target.threadID, &target.activeTurnID); err != nil {
			return routeTarget{}, err
		}
		matches = append(matches, target)
	}
	if err := rows.Err(); err != nil {
		return routeTarget{}, err
	}
	if len(matches) != 1 {
		return routeTarget{}, ErrTelegramTarget
	}
	return matches[0], nil
}

func createTelegramCommand(ctx context.Context, tx *dbTx, in IncomingUpdate, target routeTarget, operation protocol.Operation, sessionID, expectedTurn string, args protocol.Arguments) (protocol.Command, error) {
	if operation == protocol.NewSession || (operation == protocol.CodexCommand && args.Codex != nil && args.Codex.Name == "fork") {
		revision, err := selectionRevision(ctx, tx, in)
		if err != nil {
			return protocol.Command{}, err
		}
		args.SelectionRevision = &revision
	}
	if target.generation < 0 || target.generation > math.MaxInt64 {
		return protocol.Command{}, ErrTelegramTarget
	}
	now := time.Now().UTC()
	ttl := in.CommandTTL
	if ttl == 0 {
		ttl = defaultCommandTTL
	}
	if ttl <= 0 {
		return protocol.Command{}, ErrTelegramTarget
	}
	command := protocol.Command{ID: uuid.NewString(), WorkerID: target.workerID.String(), RuntimeID: target.runtimeID.String(), RuntimeGeneration: uint64(target.generation), SessionID: sessionID, ThreadID: target.threadID, Operation: operation, ExpectedTurnID: expectedTurn, Arguments: args, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := command.Validate(); err != nil {
		return protocol.Command{}, fmt.Errorf("registry: create command: %w", err)
	}
	if _, err := protocol.NewEnvelope("command", command); err != nil {
		return protocol.Command{}, fmt.Errorf("registry: command frame: %w", err)
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return protocol.Command{}, err
	}
	id, _ := uuid.Parse(command.ID)
	var dbSession any
	if sessionID != "" {
		dbSession, _ = uuid.Parse(sessionID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO commands
        (command_id, source, worker_id, runtime_id, runtime_generation, session_id,
         operation, expected_turn_id, payload, status, telegram_bot_id, telegram_update_id,
         telegram_user_id, telegram_chat_id, telegram_message_thread_id, expires_at)
        VALUES ($1,'telegram',$2,$3,$4,$5,$6,NULLIF($7,''),$8,'pending',$9,$10,$11,$12,$13,$14)`,
		id, target.workerID, target.runtimeID, target.generation, dbSession, string(operation), expectedTurn,
		string(payload), in.BotID, in.UpdateID, in.UserID, in.ChatID, in.TopicID, command.ExpiresAt); err != nil {
		return protocol.Command{}, fmt.Errorf("registry: persist command: %w", err)
	}
	return command, nil
}

func setBinding(ctx context.Context, tx *dbTx, in IncomingUpdate, sessionID uuid.UUID) error {
	if err := bumpSelectionRevision(ctx, tx, in); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO telegram_bindings (bot_id,user_id,chat_id,message_thread_id,session_id)
        VALUES ($1,$2,$3,$4,$5) ON CONFLICT (bot_id,user_id,chat_id,message_thread_id)
        DO UPDATE SET session_id=EXCLUDED.session_id, selected_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')`, in.BotID, in.UserID, in.ChatID, in.TopicID, sessionID)
	if err != nil {
		return fmt.Errorf("registry: set binding: %w", err)
	}
	return reconcileTelegramSelection(ctx, tx, in)
}

func bindingMatches(ctx context.Context, tx *dbTx, in IncomingUpdate, sessionID uuid.UUID) (bool, error) {
	topics := []int64{in.TopicID}
	if in.TopicID > 0 {
		topics = append(topics, 0)
	}
	for _, topic := range topics {
		var exists bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_bindings WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4 AND session_id=$5)`, in.BotID, in.UserID, in.ChatID, topic, sessionID).Scan(&exists)
		if err != nil {
			return false, fmt.Errorf("registry: validate input binding: %w", err)
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func queueUIResponse(ctx context.Context, tx *dbTx, in IncomingUpdate, result AcceptResult) error {
	if err := retireSelectionConfirmations(ctx, tx, in, result); err != nil {
		return err
	}
	if result.View == "queued" && result.CommandID != "" {
		var prompt bool
		if err := tx.QueryRow(ctx, `SELECT operation='start_turn' FROM commands WHERE command_id=$1`, result.CommandID).Scan(&prompt); err != nil {
			return fmt.Errorf("registry: inspect queued response: %w", err)
		}
		if prompt {
			return nil
		}
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO telegram_deliveries
        (delivery_id, bot_id, chat_id, message_thread_id, kind, payload)
        VALUES ($1,$2,$3,$4,'ui_response',$5)`, uuid.New(), in.BotID, in.ChatID, in.TopicID, string(payload))
	if err != nil {
		return fmt.Errorf("registry: queue UI response: %w", err)
	}
	return nil
}

// PendingCommands returns frozen commands across workers. Dispatchers that own
// one worker should use PendingCommandsForWorker; callers must never reroute a
// returned command based on mutable selections.
func (s *Store) PendingCommands(ctx context.Context, limit int) ([]protocol.Command, error) {
	return s.pendingCommands(ctx, nil, limit)
}

// PendingCommandsForWorker is the worker-scoped dispatcher query.
func (s *Store) PendingCommandsForWorker(ctx context.Context, workerID uuid.UUID, limit int) ([]protocol.Command, error) {
	return s.pendingCommands(ctx, &workerID, limit)
}

func (s *Store) pendingCommands(ctx context.Context, workerID *uuid.UUID, limit int) ([]protocol.Command, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("registry: invalid command limit")
	}
	query := `SELECT payload FROM commands WHERE
        status IN ('pending','dispatched','acknowledged')
        AND (status='pending' OR (status='dispatched' AND dispatched_at <= $1))
        AND (expires_at IS NULL OR expires_at > (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))`
	args := []any{time.Now().UTC().Add(-dispatchedCommandRetryAfter)}
	if workerID != nil {
		query += " AND worker_id=$2"
		args = append(args, *workerID)
	}
	query += fmt.Sprintf(" ORDER BY created_at LIMIT $%d", len(args)+1)
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("registry: list pending commands: %w", err)
	}
	defer rows.Close()
	result := make([]protocol.Command, 0)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var command protocol.Command
		if err := json.Unmarshal(raw, &command); err != nil || command.Validate() != nil {
			return nil, errors.New("registry: invalid persisted command payload")
		}
		result = append(result, command)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list pending commands: %w", err)
	}
	return result, nil
}

func (s *Store) MarkDispatched(ctx context.Context, workerID, commandID uuid.UUID) error {
	ct, err := s.pool.Exec(ctx, `UPDATE commands SET status='dispatched', dispatched_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
		WHERE command_id=$1 AND worker_id=$2 AND status IN ('pending','dispatched')`, commandID, workerID)
	if err != nil {
		return fmt.Errorf("registry: mark command dispatched: %w", err)
	}
	if ct.RowsAffected() == 1 {
		return nil
	}
	return commandTerminalNoop(ctx, s.pool, workerID, commandID, "mark command dispatched")
}

func (s *Store) AcknowledgeCommand(ctx context.Context, workerID, connectionID uuid.UUID, ack protocol.CommandAck) error {
	commandID, err := uuid.Parse(ack.CommandID)
	if err != nil {
		return ErrEventTarget
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return fmt.Errorf("registry: begin command acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkLeaseTx(ctx, tx, workerID, connectionID); err != nil {
		return err
	}
	status, code, message := "acknowledged", "", ""
	if ack.Error != nil {
		status, code, message = "failed", ack.Error.Code, ack.Error.Message
	} else if ack.Status != "accepted" && ack.Status != "duplicate" {
		status, code, message = "failed", ack.Status, "worker rejected command"
	}
	ct, err := tx.Exec(ctx, `UPDATE commands SET status=$3, acknowledged_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), error_code=NULLIF($4,''), error_message=NULLIF($5,'')
        WHERE command_id=$1 AND worker_id=$2 AND status IN ('pending','dispatched','acknowledged')`, commandID, workerID, status, code, message)
	if err != nil {
		return fmt.Errorf("registry: acknowledge command: %w", err)
	}
	if ct.RowsAffected() > 0 && status == "failed" {
		if err := recoverAsyncQuestionResponse(ctx, tx, commandID); err != nil {
			return err
		}
		if err := recoverSessionWizardAcknowledgement(ctx, tx, commandID, code); err != nil {
			return err
		}
		if err := notifyUnsupportedHistoryAcknowledgement(ctx, tx, commandID, code); err != nil {
			return err
		}
	}
	if ct.RowsAffected() == 0 {
		var commandStatus string
		err = tx.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1 AND worker_id=$2", commandID, workerID).Scan(&commandStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEventTarget
		}
		if err != nil {
			return fmt.Errorf("registry: inspect command acknowledgement: %w", err)
		}
		if !terminalCommandStatus(commandStatus) {
			return ErrEventTarget
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("registry: commit command acknowledgement: %w", err)
	}
	return nil
}

func commandTerminalNoop(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, workerID, commandID uuid.UUID, action string) error {
	var status string
	err := db.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1 AND worker_id=$2", commandID, workerID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEventTarget
	}
	if err != nil {
		return fmt.Errorf("registry: %s inspect: %w", action, err)
	}
	if terminalCommandStatus(status) {
		return nil
	}
	return ErrEventTarget
}

func terminalCommandStatus(status string) bool {
	switch status {
	case "acknowledged", "completed", "failed", "expired", "outcome_unknown":
		return true
	default:
		return false
	}
}

// ExpireCommands makes timed-out work terminal and queues a single durable
// Telegram response for its original conversation. It never consults a
// mutable binding, so an expired command cannot notify a later selection.
func (s *Store) ExpireCommands(ctx context.Context) error {
	_, err := s.expireCommands(ctx, 1000)
	return err
}

func (s *Store) expireCommands(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("registry: invalid command expiry limit")
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("registry: begin command expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `WITH due AS (
            SELECT command_id FROM commands
            WHERE status IN ('pending','dispatched') AND expires_at IS NOT NULL AND expires_at <= (strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
            ORDER BY expires_at, created_at LIMIT $1
        )
        UPDATE commands SET status='expired', completed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'), error_code='expired', error_message='command expired before acknowledgement'
        WHERE command_id IN (SELECT command_id FROM due)
        RETURNING command_id, telegram_bot_id, telegram_chat_id, telegram_message_thread_id`, limit)
	if err != nil {
		return 0, fmt.Errorf("registry: expire commands: %w", err)
	}
	type expiredCommand struct {
		id      uuid.UUID
		botID   *string
		chatID  *int64
		topicID *int64
	}
	expired := make([]expiredCommand, 0)
	count := 0
	for rows.Next() {
		var command expiredCommand
		if err := rows.Scan(&command.id, &command.botID, &command.chatID, &command.topicID); err != nil {
			return 0, fmt.Errorf("registry: scan expired command: %w", err)
		}
		count++
		expired = append(expired, command)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("registry: expire command rows: %w", err)
	}
	rows.Close()
	for _, command := range expired {
		if err := recoverAsyncQuestionResponse(ctx, tx, command.id); err != nil {
			return 0, err
		}
		if handled, err := expireSessionWizardCommand(ctx, tx, command.id); err != nil {
			return 0, err
		} else if handled {
			continue
		}
		if command.botID == nil || command.chatID == nil {
			continue
		}
		payload, err := json.Marshal(AcceptResult{View: "error", CommandID: command.id.String(), ErrorCode: "command_expired"})
		if err != nil {
			return 0, fmt.Errorf("registry: encode command expiry response: %w", err)
		}
		var topic int64
		if command.topicID != nil {
			topic = *command.topicID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_deliveries
            (delivery_id, bot_id, chat_id, message_thread_id, kind, payload)
			VALUES ($1,$2,$3,$4,'ui_response',$5)`, uuid.New(), *command.botID, *command.chatID, topic, string(payload)); err != nil {
			return 0, fmt.Errorf("registry: queue command expiry response: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("registry: commit command expiry: %w", err)
	}
	return count, nil
}
