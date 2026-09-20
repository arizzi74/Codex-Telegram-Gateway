package registry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func insertPendingQuestion(t *testing.T, env eventTestEnv, session uuid.UUID, turn string, async bool, questions ...protocol.Question) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var thread string
	if err := env.store.pool.QueryRow(context.Background(), `SELECT codex_thread_id FROM sessions WHERE session_id=$1`, session).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	approval := protocol.Approval{ID: id.String(), RequestID: "request-" + id.String(), ThreadID: thread, TurnID: turn, Type: "input", Async: async, Questions: questions}
	if async {
		approval.RequestID = "async:" + id.String()
	}
	if len(questions) == 0 {
		approval.Type, approval.Decisions = "command", []string{"accept", "decline"}
	}
	raw, _ := json.Marshal(approval)
	if _, err := env.store.pool.Exec(context.Background(), `INSERT INTO approvals
      (approval_id,worker_id,runtime_id,runtime_generation,session_id,codex_request_id,codex_thread_id,codex_turn_id,approval_type,request_payload,state,requested_at)
      VALUES($1,$2,$3,1,$4,$5,$6,$7,$8,$9,'pending',`+sqliteNow+`)`, id, env.worker, env.runtime, session, approval.RequestID, thread, turn, approval.Type, string(raw)); err != nil {
		t.Fatal(err)
	}
	return id
}

func pendingQuestionCallback(t *testing.T, env eventTestEnv, session, approval uuid.UUID, action, question, answer string) string {
	t.Helper()
	token, err := env.store.CreateCallback(context.Background(), Callback{Action: action, BotID: "bot", UserID: 10, ChatID: 20, SessionID: session, RuntimeID: env.runtime, Generation: 1, ApprovalID: approval, QuestionID: question, Answer: answer, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestPendingQuestionsNoSelectionAndPartialCrossSessionReply(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='waiting-turn',state='waiting_input',name='A very long complete session name' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	id := insertPendingQuestion(t, env, env.session, "waiting-turn", false, protocol.Question{ID: "first", Prompt: "First?"}, protocol.Question{ID: "second", Prompt: "Second?"})
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET input_answers='{"first":["Already answered"]}' WHERE approval_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("unknown context exposed pending questions: %v", err)
	}
	in := telegramUpdate(env, 1)
	in.Action = "questions"
	result, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || result.View != "questions" || result.UserID != 10 || result.SessionID != "" {
		t.Fatalf("list without selection: %#v %v", result, err)
	}
	page, err := env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0)
	if err != nil || len(page.Requests) != 1 || page.Requests[0].SessionName != "A very long complete session name" {
		t.Fatalf("pending page: %#v %v", page, err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	in.UpdateID, in.Action, in.Target = 2, "select", other.String()
	if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
		t.Fatal(err)
	}
	token := pendingQuestionCallback(t, env, env.session, id, "question_open", "", "")
	for i, mutate := range []func(*IncomingUpdate){func(v *IncomingUpdate) { v.UserID++ }, func(v *IncomingUpdate) { v.ChatID++ }, func(v *IncomingUpdate) { v.TopicID++ }, func(v *IncomingUpdate) { v.BotID = "other-bot" }} {
		bad := telegramUpdate(env, int64(3+i))
		bad.CallbackToken = token
		mutate(&bad)
		if rejected, err := env.store.AcceptTelegram(ctx, bad); err != nil || rejected.ErrorCode != "callback_invalid" {
			t.Fatalf("unauthorized replay: %#v %v", rejected, err)
		}
	}
	open := telegramUpdate(env, 7)
	open.CallbackToken = token
	replayed, err := env.store.AcceptTelegram(ctx, open)
	if err != nil || replayed.View != "input_prompt" || replayed.QuestionID != "second" || replayed.SessionID != env.session.String() {
		t.Fatalf("resume first unanswered: %#v %v", replayed, err)
	}
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 300, env.session, "waiting-turn", id, replayed.QuestionID); err != nil {
		t.Fatal(err)
	}
	answer := telegramUpdate(env, 8)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 300, "second answer"
	accepted, err := env.store.AcceptTelegram(ctx, answer)
	if err != nil || accepted.CommandID == "" || accepted.SessionID != env.session.String() {
		t.Fatalf("answer route: %#v %v", accepted, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Arguments.Answers["first"][0] != "Already answered" || commands[0].Arguments.Answers["second"][0] != "second answer" {
		t.Fatalf("preserved answers: %#v %v", commands, err)
	}
	var selected string
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=0`).Scan(&selected); err != nil || selected != other.String() {
		t.Fatalf("answer changed selection: %s %v", selected, err)
	}
	page, err = env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0)
	if err != nil || len(page.Requests) != 0 {
		t.Fatalf("claimed response remained visible: %#v %v", page, err)
	}
	open.UpdateID = 9
	if result, err := env.store.AcceptTelegram(ctx, open); err != nil || result.ErrorCode != "callback_invalid" {
		t.Fatalf("used replay button accepted: %#v %v", result, err)
	}
}

func TestPendingQuestionsAsyncAcrossTurnsAndStaleRequests(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='new-turn' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	question := protocol.Question{ID: "q1", Prompt: "Choose?", Options: []string{"One", "Two"}}
	id := insertPendingQuestion(t, env, env.session, "old-turn", true, question)
	insertPendingQuestion(t, env, env.session, "old-turn", false, question)
	stale := insertPendingQuestion(t, env, env.session, "new-turn", false, question)
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET runtime_generation=0 WHERE approval_id=$1`, stale); err != nil {
		t.Fatal(err)
	}
	archived := uuid.New()
	insertRouteSession(t, env, archived, "archived-thread", "old-turn")
	insertPendingQuestion(t, env, archived, "old-turn", true, question)
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=TRUE WHERE session_id=$1`, archived); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 1)
	in.Action = "input_command"
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.View != "questions" {
		t.Fatalf("bare tginput alias: %#v %v", result, err)
	}
	page, err := env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0)
	if err != nil || len(page.Requests) != 1 || page.Requests[0].Approval.ID != id.String() {
		t.Fatalf("async/stale list: %#v %v", page, err)
	}
	in.UpdateID, in.CallbackToken = 2, pendingQuestionCallback(t, env, env.session, id, "question_open", "", "")
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.QuestionID != "q1" {
		t.Fatalf("async replay: %#v %v", result, err)
	}
	in.UpdateID, in.CallbackToken = 3, pendingQuestionCallback(t, env, env.session, id, "input", "q1", "One")
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.CommandID == "" {
		t.Fatalf("async answer: %#v %v", result, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].ExpectedTurnID != "" || commands[0].Arguments.Answers["q1"][0] != "One" {
		t.Fatalf("async response turn restriction: %#v %v", commands, err)
	}
}

func TestPendingQuestionsPaginationApprovalAndRuntimeChange(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for i := 0; i < PendingQuestionsPageSize+1; i++ {
		ids = append(ids, insertPendingQuestion(t, env, env.session, "", false))
	}
	in := telegramUpdate(env, 1)
	in.Action = "questions"
	if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
		t.Fatal(err)
	}
	for pageNo, count := range []int{PendingQuestionsPageSize, 1, 0} {
		page, err := env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, pageNo)
		if err != nil || len(page.Requests) != count || page.HasMore != (pageNo == 0) {
			t.Fatalf("page %d: %#v %v", pageNo, page, err)
		}
	}
	in.UpdateID, in.CallbackToken = 2, pendingQuestionCallback(t, env, env.session, ids[0], "question_open", "", "")
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.View != "approval_prompt" || result.ApprovalID != ids[0].String() {
		t.Fatalf("approval replay: %#v %v", result, err)
	}
	in.UpdateID, in.CallbackToken = 3, pendingQuestionCallback(t, env, env.session, ids[1], "question_open", "", "")
	if _, err := env.store.pool.Exec(ctx, `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime); err != nil {
		t.Fatal(err)
	}
	if result, err := env.store.AcceptTelegram(ctx, in); err != nil || result.ErrorCode != "callback_invalid" {
		t.Fatalf("superseded runtime replay: %#v %v", result, err)
	}
	if _, err := env.store.PendingApproval(ctx, ids[1]); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("stale approval still renderable: %v", err)
	}
}

func TestPendingAsyncQuestionDismissDoesNotAnswerOrSelect(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	id := insertPendingQuestion(t, env, env.session, "past-turn", true, protocol.Question{ID: "q1", Prompt: "A past question?"})
	in := telegramUpdate(env, 1)
	in.CallbackToken = pendingQuestionCallback(t, env, env.session, id, "dismiss_input", "", "")
	accepted, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("dismiss async question: %#v %v", accepted, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Operation != protocol.InputResponse || commands[0].Arguments.Decision != "dismiss" || len(commands[0].Arguments.Answers) != 0 || commands[0].ExpectedTurnID != "" {
		t.Fatalf("dismiss sent an answer: %#v %v", commands, err)
	}
	var selected int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_bindings`).Scan(&selected); err != nil || selected != 0 {
		t.Fatalf("dismiss selected a session: %d %v", selected, err)
	}
	page, err := env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0)
	if err != nil || len(page.Requests) != 0 {
		t.Fatalf("dismissed request remains listed: %#v %v", page, err)
	}
	blocking := insertPendingQuestion(t, env, env.session, "", false, protocol.Question{ID: "q2", Prompt: "A blocking question?"})
	in.UpdateID, in.CallbackToken = 2, pendingQuestionCallback(t, env, env.session, blocking, "dismiss_input", "", "")
	if rejected, err := env.store.AcceptTelegram(ctx, in); err != nil || rejected.ErrorCode != "callback_invalid" {
		t.Fatalf("blocking question dismissed as async: %#v %v", rejected, err)
	}
}
