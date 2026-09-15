package registry

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func insertRouteSession(t *testing.T, env eventTestEnv, id uuid.UUID, thread, active string) {
	t.Helper()
	_, err := env.store.pool.Exec(context.Background(), `INSERT INTO sessions
        (session_id, worker_id, runtime_id, codex_thread_id, state, active_turn_id, loaded)
        VALUES ($1,$2,$3,$4,'idle',NULLIF($5,''),TRUE)`, id, env.worker, env.runtime, thread, active)
	if err != nil {
		t.Fatal(err)
	}
}

func telegramUpdate(env eventTestEnv, updateID int64) IncomingUpdate {
	return IncomingUpdate{BotID: "bot", UpdateID: updateID, UserID: 10, ChatID: 20, Raw: json.RawMessage(`{"update_id":1}`)}
}

func TestAcceptTelegramRoutingPriorityAndFrozenCommandIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	topicSession, replySession := uuid.New(), uuid.New()
	insertRouteSession(t, env, topicSession, "thread-topic", "turn-topic")
	insertRouteSession(t, env, replySession, "thread-reply", "")
	base := telegramUpdate(env, 1)
	base.Action, base.Target = "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, base); err != nil {
		t.Fatal(err)
	}
	topic := telegramUpdate(env, 2)
	topic.TopicID, topic.Action, topic.Target = 77, "select", topicSession.String()
	if _, err := env.store.AcceptTelegram(ctx, topic); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordBotMessageRoute(ctx, "bot", 20, 99, replySession, "", uuid.Nil); err != nil {
		t.Fatal(err)
	}

	// Topic binding wins over the default selection and freezes its exact target.
	message := telegramUpdate(env, 3)
	message.TopicID, message.Text = 77, "topic text"
	accepted, err := env.store.AcceptTelegram(ctx, message)
	if err != nil || accepted.SessionID != topicSession.String() {
		t.Fatalf("topic route = %#v, %v", accepted, err)
	}
	commandID := uuid.MustParse(accepted.CommandID)
	// Changing selection afterwards must never affect the persisted command.
	changed := telegramUpdate(env, 4)
	changed.Action, changed.Target = "select", replySession.String()
	if _, err := env.store.AcceptTelegram(ctx, changed); err != nil {
		t.Fatal(err)
	}
	pending, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != commandID.String() || pending[0].SessionID != topicSession.String() || pending[0].ThreadID != "thread-topic" {
		t.Fatalf("frozen pending command = %#v, %v", pending, err)
	}
	// A reply mapping is more specific than the now-changed selection.
	reply := telegramUpdate(env, 5)
	reply.ReplyToMessageID, reply.Text = 99, "reply text"
	accepted, err = env.store.AcceptTelegram(ctx, reply)
	if err != nil || accepted.SessionID != replySession.String() {
		t.Fatalf("reply route = %#v, %v", accepted, err)
	}
	// Duplicate updates do not create commands or responses a second time.
	dup, err := env.store.AcceptTelegram(ctx, reply)
	if err != nil || !dup.Duplicate {
		t.Fatalf("duplicate = %#v, %v", dup, err)
	}
}

func TestAcceptTelegramCommandsCallbacksAndDispatchIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	selectUpdate := telegramUpdate(env, 1)
	selectUpdate.Action, selectUpdate.Target = "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selectUpdate); err != nil {
		t.Fatal(err)
	}

	// Steer requires an active turn and captures it as expected_turn_id.
	steer := telegramUpdate(env, 2)
	steer.Action, steer.Text = "steer", "continue"
	if rejected, err := env.store.AcceptTelegram(ctx, steer); err != nil || rejected.View != "error" || rejected.ErrorCode != "stale_turn" {
		t.Fatalf("steer without turn = %#v, %v", rejected, err)
	}
	if duplicate, err := env.store.AcceptTelegram(ctx, steer); err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate deterministic error = %#v, %v", duplicate, err)
	}
	var errorResponses int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries
        WHERE kind='ui_response' AND payload->>'error_code'='stale_turn'`).Scan(&errorResponses); err != nil || errorResponses != 1 {
		t.Fatalf("deterministic stale-turn response count = %d, %v", errorResponses, err)
	}
	if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET active_turn_id='turn-1', state='running' WHERE session_id=$1", env.session); err != nil {
		t.Fatal(err)
	}
	steer.UpdateID = 3
	accepted, err := env.store.AcceptTelegram(ctx, steer)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(pending) != 1 || pending[0].Operation != protocol.Steer || pending[0].ExpectedTurnID != "turn-1" {
		t.Fatalf("steer command = %#v, %v", pending, err)
	}
	if err := env.store.MarkDispatched(ctx, env.worker, uuid.MustParse(accepted.CommandID)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: accepted.CommandID, Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	newConnection := uuid.New()
	if err := env.store.BindConnection(ctx, env.worker, newConnection); err != nil {
		t.Fatal(err)
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: accepted.CommandID, Status: "accepted"}); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("stale command acknowledgement = %v, want ErrConnectionFenced", err)
	}
	if err := env.store.BindConnection(ctx, env.worker, env.connection); err != nil {
		t.Fatal(err)
	}

	approvalID := uuid.New()
	approvalPayload, _ := json.Marshal(protocol.Approval{ID: approvalID.String(), RequestID: "request-1", ThreadID: "thread-1", Type: "permissions", Decisions: []string{"grant", "decline"}})
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO approvals
        (approval_id, worker_id, runtime_id, runtime_generation, session_id, codex_request_id,
         codex_thread_id, approval_type, request_payload, state, requested_at)
	        VALUES ($1,$2,$3,1,$4,'request-1','thread-1','permissions',$5,'pending',(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))`, approvalID, env.worker, env.runtime, env.session, json.RawMessage(approvalPayload)); err != nil {
		t.Fatal(err)
	}
	token, err := env.store.CreateCallback(ctx, Callback{Action: "approval", BotID: "bot", UserID: 10, ChatID: 20, SessionID: env.session, RuntimeID: env.runtime, Generation: 1, ApprovalID: approvalID, Decision: "grant", ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	secondToken, err := env.store.CreateCallback(ctx, Callback{Action: "approval", BotID: "bot", UserID: 10, ChatID: 20, SessionID: env.session, RuntimeID: env.runtime, Generation: 1, ApprovalID: approvalID, Decision: "decline", ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	type callbackOutcome struct {
		result AcceptResult
		err    error
	}
	results := make(chan callbackOutcome, 2)
	var callbacks sync.WaitGroup
	for _, callback := range []IncomingUpdate{
		func() IncomingUpdate { update := telegramUpdate(env, 4); update.CallbackToken = token; return update }(),
		func() IncomingUpdate {
			update := telegramUpdate(env, 7)
			update.CallbackToken = secondToken
			return update
		}(),
	} {
		callbacks.Add(1)
		go func(callback IncomingUpdate) {
			defer callbacks.Done()
			result, err := env.store.AcceptTelegram(ctx, callback)
			results <- callbackOutcome{result: result, err: err}
		}(callback)
	}
	callbacks.Wait()
	close(results)
	queued, rejectedCallbacks := 0, 0
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("racing callback error: %v", outcome.err)
		}
		if outcome.result.CommandID != "" {
			queued++
		} else if outcome.result.View == "error" && outcome.result.ErrorCode == "callback_invalid" {
			rejectedCallbacks++
		}
	}
	if queued != 1 || rejectedCallbacks != 1 {
		t.Fatalf("racing callback outcomes queued=%d rejected=%d", queued, rejectedCallbacks)
	}
	callback := telegramUpdate(env, 5)
	callback.CallbackToken = token
	if rejected, err := env.store.AcceptTelegram(ctx, callback); err != nil || rejected.View != "error" || rejected.ErrorCode != "callback_invalid" {
		t.Fatalf("callback replay = %#v, %v", rejected, err)
	}
	var approvalCommands int
	if err := env.store.pool.QueryRow(ctx, "SELECT count(*) FROM commands WHERE telegram_update_id IN (4,7)").Scan(&approvalCommands); err != nil || approvalCommands != 1 {
		t.Fatalf("competing approval commands = %d, %v", approvalCommands, err)
	}

	// Input is addressed to one exact pending request before ordinary routing.
	inputApprovalID := uuid.New()
	inputPayload, _ := json.Marshal(protocol.Approval{ID: inputApprovalID.String(), RequestID: "input-request", ThreadID: "thread-1", Type: "input", Questions: []protocol.Question{{ID: "question-1", Prompt: "Answer"}}})
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO approvals
        (approval_id, worker_id, runtime_id, runtime_generation, session_id, codex_request_id,
         codex_thread_id, approval_type, request_payload, state, requested_at)
        VALUES ($1,$2,$3,1,$4,'input-request','thread-1','input',$5,'pending',(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))`, inputApprovalID, env.worker, env.runtime, env.session, json.RawMessage(inputPayload)); err != nil {
		t.Fatal(err)
	}
	input := telegramUpdate(env, 6)
	input.Action, input.Target, input.Text = "input", inputApprovalID.String(), "answer"
	accepted, err = env.store.AcceptTelegram(ctx, input)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("input response = %#v, %v", accepted, err)
	}
	pending, err = env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range pending {
		if command.ID == accepted.CommandID && (len(command.Arguments.Answers) != 1 || command.Arguments.Answers["question-1"][0] != "answer") {
			t.Fatalf("input answers = %#v", command.Arguments.Answers)
		}
	}
}

func TestAcceptTelegramNewAndRoutingWritesIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	newRequest := telegramUpdate(env, 1)
	newRequest.Action, newRequest.Target = "new", "main"
	_, err := env.store.AcceptTelegram(ctx, newRequest)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(pending) != 1 || pending[0].Operation != protocol.NewSession || pending[0].SessionID != "" {
		t.Fatalf("new session command = %#v, %v", pending, err)
	}

	// Disconnect and status are routing/UI writes only; neither emits commands.
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	selectUpdate := telegramUpdate(env, 2)
	selectUpdate.Action, selectUpdate.Target = "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selectUpdate); err != nil {
		t.Fatal(err)
	}
	disconnect := telegramUpdate(env, 3)
	disconnect.Action = "disconnect"
	if _, err := env.store.AcceptTelegram(ctx, disconnect); err != nil {
		t.Fatal(err)
	}
	status := telegramUpdate(env, 4)
	status.Action = "status"
	if result, err := env.store.AcceptTelegram(ctx, status); err != nil || result.View != "error" || result.ErrorCode != "target_unavailable" {
		t.Fatalf("status = %#v, %v", result, err)
	}
	pending, err = env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("routing write created command: %#v, %v", pending, err)
	}
}

func TestAcceptTelegramMultiQuestionReplyRoutingIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET active_turn_id='turn-input', state='waiting_input' WHERE session_id=$1", env.session); err != nil {
		t.Fatal(err)
	}
	approvalID := uuid.New()
	payload, _ := json.Marshal(protocol.Approval{
		ID: approvalID.String(), RequestID: "multi-input", ThreadID: "thread-1", TurnID: "turn-input", Type: "input",
		Questions: []protocol.Question{{ID: "first", Prompt: "First"}, {ID: "second", Prompt: "Second"}},
	})
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO approvals
        (approval_id, worker_id, runtime_id, runtime_generation, session_id, codex_request_id,
         codex_thread_id, codex_turn_id, approval_type, request_payload, state, requested_at)
        VALUES ($1,$2,$3,1,$4,'multi-input','thread-1','turn-input','input',$5,'pending',(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))`, approvalID, env.worker, env.runtime, env.session, json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 901, env.session, "turn-input", approvalID, "first"); err != nil {
		t.Fatal(err)
	}
	first := telegramUpdate(env, 1)
	first.Action, first.ReplyToMessageID, first.Text = "text", 901, "one"
	partial, err := env.store.AcceptTelegram(ctx, first)
	if err != nil || partial.View != "input_pending" || partial.ApprovalID != approvalID.String() || partial.QuestionID != "second" {
		t.Fatalf("first input reply = %#v, %v", partial, err)
	}
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 902, env.session, "turn-input", approvalID, "second"); err != nil {
		t.Fatal(err)
	}
	second := telegramUpdate(env, 2)
	second.Action, second.ReplyToMessageID, second.Text = "text", 902, "two"
	complete, err := env.store.AcceptTelegram(ctx, second)
	if err != nil || complete.View != "queued" || complete.CommandID == "" {
		t.Fatalf("second input reply = %#v, %v", complete, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range commands {
		if command.ID == complete.CommandID {
			if command.ExpectedTurnID != "turn-input" || command.Arguments.Answers["first"][0] != "one" || command.Arguments.Answers["second"][0] != "two" {
				t.Fatalf("frozen multi-input command = %#v", command)
			}
			return
		}
	}
	t.Fatalf("missing multi-input command %s", complete.CommandID)
}

func TestCommandDispatchRecoveryAndExpiryIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	selectUpdate := telegramUpdate(env, 1)
	selectUpdate.Action, selectUpdate.Target = "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selectUpdate); err != nil {
		t.Fatal(err)
	}
	start := telegramUpdate(env, 2)
	start.Text = "retry me"
	accepted, err := env.store.AcceptTelegram(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	commandID := uuid.MustParse(accepted.CommandID)
	if err := env.store.MarkDispatched(ctx, env.worker, commandID); err != nil {
		t.Fatal(err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 0 {
		t.Fatalf("fresh dispatched command should wait for retry: %#v, %v", commands, err)
	}
	if _, err := env.store.pool.Exec(ctx, "UPDATE commands SET dispatched_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-6 seconds') || '000000Z') WHERE command_id=$1", commandID); err != nil {
		t.Fatal(err)
	}
	commands, err = env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != commandID.String() {
		t.Fatalf("dispatched retry = %#v, %v", commands, err)
	}
	if _, err := env.store.pool.Exec(ctx, "UPDATE commands SET status='completed', completed_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z') WHERE command_id=$1", commandID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkDispatched(ctx, env.worker, commandID); err != nil {
		t.Fatalf("terminal dispatch race = %v", err)
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: commandID.String(), Status: "accepted"}); err != nil {
		t.Fatalf("terminal acknowledgement race = %v", err)
	}

	expired := telegramUpdate(env, 3)
	expired.Text, expired.CommandTTL = "expire me", time.Minute
	queued, err := env.store.AcceptTelegram(ctx, expired)
	if err != nil {
		t.Fatal(err)
	}
	expiredID := uuid.MustParse(queued.CommandID)
	if _, err := env.store.pool.Exec(ctx, "UPDATE commands SET expires_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-1 second') || '000000Z') WHERE command_id=$1", expiredID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ExpireCommands(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := env.store.pool.QueryRow(ctx, "SELECT status FROM commands WHERE command_id=$1", expiredID).Scan(&status); err != nil || status != "expired" {
		t.Fatalf("expired command status=%q err=%v", status, err)
	}
	var expiryResponses int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries
        WHERE kind='ui_response' AND payload->>'command_id'=$1 AND payload->>'error_code'='command_expired'`, expiredID.String()).Scan(&expiryResponses); err != nil || expiryResponses != 1 {
		t.Fatalf("expiry delivery count=%d err=%v", expiryResponses, err)
	}
}
