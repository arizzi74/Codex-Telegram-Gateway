package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// QuestionAnswerEdit replaces a previously delivered question in place. It has
// no event delivery target: answering a background session must edit that
// session's original question even after the user changes their selection.
type QuestionAnswerEdit struct {
	MessageID int64  `json:"message_id"`
	Question  string `json:"question"`
	Answer    string `json:"answer"`
}

func preserveInputQuestions(ctx context.Context, tx *dbTx, id uuid.UUID, questions []protocol.Question) error {
	for _, question := range questions {
		if strings.TrimSpace(question.ID) == "" {
			return ErrEventTarget
		}
		raw, err := json.Marshal(question)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_question_answers(approval_id,question_id,question)
            VALUES($1,$2,$3) ON CONFLICT(approval_id,question_id) DO NOTHING`, id, question.ID, string(raw)); err != nil {
			return fmt.Errorf("registry: preserve input question: %w", err)
		}
	}
	return nil
}

func recordInputAnswerSummaries(ctx context.Context, tx *dbTx, id uuid.UUID, answers map[string][]string, replace bool) error {
	for questionID, values := range answers {
		// An empty response is a dismissal, not an answer to display.
		answer := strings.Join(values, "\n")
		if strings.TrimSpace(answer) == "" {
			continue
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT question FROM telegram_question_answers WHERE approval_id=$1 AND question_id=$2`, id, questionID).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEventTarget
			}
			return err
		}
		var question protocol.Question
		if err := json.Unmarshal(raw, &question); err != nil {
			return err
		}
		if question.Secret {
			answer = "[REDACTED]"
		}
		// A fresh Telegram answer after a definitely failed submission may
		// replace its draft. Replayed worker acknowledgements cannot do so.
		if _, err := tx.Exec(ctx, `UPDATE telegram_question_answers SET answer=$3,version=version+1
            WHERE approval_id=$1 AND question_id=$2 AND (answer IS NULL OR ($4 AND answer<>$3))`, id, questionID, answer, replace); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries SET visibility_revoked=1,
                status=CASE WHEN status IN ('pending','failed') THEN 'cancelled' ELSE status END
            WHERE delivery_id IN (SELECT edit.delivery_id FROM telegram_question_answer_edits edit
                JOIN telegram_question_answers summary USING(approval_id,question_id)
                WHERE edit.approval_id=$1 AND edit.question_id=$2 AND edit.version<summary.version)
              AND status IN ('pending','failed','sending')`, id, questionID); err != nil {
			return err
		}
	}
	return enqueueQuestionAnswerEdits(ctx, tx, id, "", 0, 0)
}

// Restricting by approval uses the question route index; the optional exact
// Telegram identity is used when checkpointing a message sent after its answer.
func enqueueQuestionAnswerEdits(ctx context.Context, tx *dbTx, id uuid.UUID, bot string, chat, message int64) error {
	rows, err := tx.Query(ctx, `SELECT route.bot_id,route.chat_id,route.message_id,summary.question_id,summary.question,summary.answer,summary.version
        FROM telegram_question_answers summary JOIN bot_message_routes route
          ON route.approval_id=summary.approval_id AND route.question_id=summary.question_id
        WHERE summary.approval_id=$1 AND summary.answer IS NOT NULL
          AND NOT EXISTS(SELECT 1 FROM telegram_input_reply_messages helper
            WHERE helper.bot_id=route.bot_id AND helper.chat_id=route.chat_id AND helper.message_id=route.message_id)
          AND ($2='' OR (route.bot_id=$2 AND route.chat_id=$3 AND route.message_id=$4))`, id, bot, chat, message)
	if err != nil {
		return err
	}
	type editTarget struct {
		bot, questionID string
		chat, version   int64
		edit            QuestionAnswerEdit
	}
	var targets []editTarget
	for rows.Next() {
		var target editTarget
		var raw []byte
		if err := rows.Scan(&target.bot, &target.chat, &target.edit.MessageID, &target.questionID, &raw, &target.edit.Answer, &target.version); err != nil {
			rows.Close()
			return err
		}
		var question protocol.Question
		if err := json.Unmarshal(raw, &question); err != nil {
			rows.Close()
			return err
		}
		target.edit.Question = question.Prompt
		targets = append(targets, target)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, target := range targets {
		payload, err := json.Marshal(target.edit)
		if err != nil {
			return err
		}
		key, _ := json.Marshal([]any{"question_answered", id.String(), target.questionID, target.bot, target.chat, target.edit.MessageID, target.version})
		deliveryID := uuid.NewSHA1(uuid.NameSpaceOID, key)
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload)
            VALUES($1,$2,$3,'question_answered',$4) ON CONFLICT(delivery_id) DO NOTHING`, deliveryID, target.bot, target.chat, string(payload)); err != nil {
			return fmt.Errorf("registry: enqueue question answer edit: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO telegram_question_answer_edits(delivery_id,approval_id,question_id,version)
            VALUES($1,$2,$3,$4) ON CONFLICT(delivery_id) DO NOTHING`, deliveryID, id, target.questionID, target.version); err != nil {
			return err
		}
	}
	return nil
}

func applyInputAnswered(ctx context.Context, tx *dbTx, workerID, runtimeID, sessionID uuid.UUID, generation int64, approval protocol.Approval) error {
	id, err := uuid.Parse(approval.ID)
	if err != nil || approval.RequestID == "" || approval.ThreadID == "" || (approval.Type != "input" && approval.Type != "user_input") {
		return ErrEventTarget
	}
	var requestRaw, answersRaw []byte
	err = tx.QueryRow(ctx, `SELECT request_payload,input_answers FROM approvals
        WHERE approval_id=$1 AND worker_id=$2 AND runtime_id=$3 AND session_id=$4 AND runtime_generation=$5
          AND codex_request_id=$6 AND codex_thread_id=$7 AND COALESCE(codex_turn_id,'')=$8
          AND approval_type=$9`, id, workerID, runtimeID, sessionID, generation, approval.RequestID, approval.ThreadID, approval.TurnID, approval.Type).Scan(&requestRaw, &answersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEventTarget
	}
	if err != nil {
		return err
	}
	var original protocol.Approval
	if err := json.Unmarshal(requestRaw, &original); err != nil {
		return err
	}
	if err := preserveInputQuestions(ctx, tx, id, original.Questions); err != nil {
		return err
	}
	answers := make(map[string][]string)
	if err := json.Unmarshal(answersRaw, &answers); err != nil {
		return err
	}
	if err := recordInputAnswerSummaries(ctx, tx, id, approval.Answers, false); err != nil {
		return err
	}
	for questionID, values := range approval.Answers {
		if len(answers[questionID]) == 0 && strings.TrimSpace(strings.Join(values, "\n")) != "" {
			answers[questionID] = values
		}
	}
	raw, err := json.Marshal(answers)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE approvals SET input_answers=$2 WHERE approval_id=$1`, id, string(raw))
	return err
}
