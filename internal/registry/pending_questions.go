package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const PendingQuestionsPageSize = 8

type PendingQuestionRequest struct {
	SessionID, RuntimeID, SessionName string
	Generation                        int64
	Approval                          protocol.Approval
}

type PendingQuestionsPage struct {
	Requests []PendingQuestionRequest
	HasMore  bool
}

// Shared by listing, replay, and rendering so an old notification cannot
// restore an archived session or a request from an earlier runtime generation.
const pendingRequestFrom = ` FROM approvals approval
    JOIN sessions session ON session.session_id=approval.session_id
      AND session.runtime_id=approval.runtime_id AND session.worker_id=approval.worker_id
      AND session.codex_thread_id=approval.codex_thread_id
    JOIN runtimes runtime ON runtime.runtime_id=approval.runtime_id
      AND runtime.worker_id=approval.worker_id AND runtime.generation=approval.runtime_generation
    WHERE approval.state='pending' AND approval.response_command_id IS NULL
      AND session.archived=FALSE
      AND (json_extract(approval.request_payload,'$.async')=1
        OR COALESCE(approval.codex_turn_id,'')=''
        OR approval.codex_turn_id=COALESCE(session.active_turn_id,''))`

// ListPendingQuestions serves only a previously authenticated Telegram context.
// It intentionally does not depend on the selected session or multisession mode.
func (s *Store) ListPendingQuestions(ctx context.Context, bot string, user, chat, topic int64, page int) (PendingQuestionsPage, error) {
	if !validSessionPage("questions", page) {
		return PendingQuestionsPage{}, ErrTelegramTarget
	}
	var known bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_chat_modes WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4)`, bot, user, chat, topic).Scan(&known); err != nil {
		return PendingQuestionsPage{}, err
	}
	if !known {
		return PendingQuestionsPage{}, ErrTelegramTarget
	}
	rows, err := s.pool.Query(ctx, `SELECT approval.session_id,approval.runtime_id,approval.runtime_generation,
        COALESCE(session.name,''),COALESCE(session.preview,''),COALESCE(session.cwd,''),approval.request_payload`+pendingRequestFrom+`
        AND (COALESCE(json_array_length(approval.request_payload,'$.questions'),0)=0 OR EXISTS(
          SELECT 1 FROM json_each(approval.request_payload,'$.questions') question WHERE NOT EXISTS(
            SELECT 1 FROM json_each(approval.input_answers) answer
            WHERE answer.key=json_extract(question.value,'$.id') AND json_array_length(answer.value)>0)))
        ORDER BY approval.requested_at,approval.approval_id LIMIT $1 OFFSET $2`, PendingQuestionsPageSize+1, page*PendingQuestionsPageSize)
	if err != nil {
		return PendingQuestionsPage{}, fmt.Errorf("registry: list pending questions: %w", err)
	}
	defer rows.Close()
	var result PendingQuestionsPage
	for rows.Next() {
		var item PendingQuestionRequest
		var preview, cwd string
		var raw []byte
		if err := rows.Scan(&item.SessionID, &item.RuntimeID, &item.Generation, &item.SessionName, &preview, &cwd, &raw); err != nil {
			return PendingQuestionsPage{}, err
		}
		if len(result.Requests) == PendingQuestionsPageSize {
			result.HasMore = true
			break
		}
		if err := json.Unmarshal(raw, &item.Approval); err != nil {
			return PendingQuestionsPage{}, fmt.Errorf("registry: decode pending question: %w", err)
		}
		item.SessionName = wizardSessionLabel(item.SessionName, preview, cwd)
		result.Requests = append(result.Requests, item)
	}
	return result, rows.Err()
}

func consumePendingQuestion(ctx context.Context, tx *dbTx, in IncomingUpdate, callback callbackPayload, sessionID, approvalID *uuid.UUID) (AcceptResult, error) {
	if sessionID == nil || approvalID == nil {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var target routeTarget
	var raw, answered []byte
	err := tx.QueryRow(ctx, `SELECT approval.session_id,approval.runtime_id,approval.runtime_generation,
        approval.request_payload,approval.input_answers`+pendingRequestFrom+` AND approval.approval_id=$1`, *approvalID).
		Scan(&target.sessionID, &target.runtimeID, &target.generation, &raw, &answered)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err != nil {
		return AcceptResult{}, err
	}
	if target.sessionID != *sessionID || !callbackMatchesTarget(callback, target) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	var approval protocol.Approval
	var answers map[string][]string
	if json.Unmarshal(raw, &approval) != nil || json.Unmarshal(answered, &answers) != nil {
		return AcceptResult{}, ErrCallbackInvalid
	}
	result := AcceptResult{View: "approval_prompt", UserID: in.UserID, SessionID: target.sessionID.String(), RuntimeID: target.runtimeID.String(), ApprovalID: approvalID.String()}
	if len(approval.Questions) > 0 {
		result.View = "input_prompt"
		result.QuestionID = nextUnansweredQuestion(approval, answers)
		if result.QuestionID == "" {
			return AcceptResult{}, ErrCallbackInvalid
		}
	}
	return result, nil
}
