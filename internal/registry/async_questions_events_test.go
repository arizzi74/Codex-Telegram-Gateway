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

func asyncQuestionEvent(t *testing.T, env eventTestEnv, seq, generation uint64, kind string, approval protocol.Approval) protocol.Event {
	t.Helper()
	raw, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: generation, SessionID: env.session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return event
}

func assertAsyncSessionState(t *testing.T, env eventTestEnv, state, turn string) {
	t.Helper()
	var gotState, gotTurn string
	if err := env.store.pool.QueryRow(context.Background(), `SELECT state,COALESCE(active_turn_id,'') FROM sessions WHERE session_id=$1`, env.session).Scan(&gotState, &gotTurn); err != nil {
		t.Fatal(err)
	}
	if gotState != state || gotTurn != turn {
		t.Fatalf("session state/turn = %q/%q, want %q/%q", gotState, gotTurn, state, turn)
	}
}

func TestAsyncQuestionEventsReachAllKnownContextsWithoutPausingTurn(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	// An idle selected session and a disconnected topic must still receive a
	// question from another session, independently of multisession mode.
	for i, action := range []string{"select", "disconnect"} {
		in := telegramUpdate(env, int64(2+i))
		in.ChatID, in.TopicID = int64(30+i), int64(4+i)
		in.Action, in.Target = action, other.String()
		if accepted, err := env.store.AcceptTelegram(ctx, in); err != nil || accepted.ErrorCode != "" {
			t.Fatalf("register context: %#v %v", accepted, err)
		}
	}
	progressEvent(t, env, 2, "turn_started", "turn-one", "")
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async-question", ThreadID: "thread-1", TurnID: "turn-one", Type: "input", Async: true,
		Questions: []protocol.Question{{ID: "choice", Prompt: "Which option?", Options: []string{"One", "Two"}}}}
	asyncQuestionEvent(t, env, 3, 1, "user_input_requested", approval)
	assertAsyncSessionState(t, env, "running", "turn-one")
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20})
	deliveries := sessionModeDeliveries(t, env.store)
	want := map[TelegramTypingTarget]bool{{BotID: "bot", ChatID: 20}: true, {BotID: "bot", ChatID: 30, TopicID: 4}: true, {BotID: "bot", ChatID: 31, TopicID: 5}: true}
	if len(deliveries) != len(want) {
		t.Fatalf("question deliveries = %d, want %d", len(deliveries), len(want))
	}
	for _, delivery := range deliveries {
		target := TelegramTypingTarget{BotID: delivery.BotID, ChatID: delivery.ChatID, TopicID: delivery.TopicID}
		if delivery.Kind != "user_input_requested" || !want[target] {
			t.Fatalf("unexpected question destination: %#v", delivery)
		}
		delete(want, target)
		if suppressed, err := env.store.SuppressTelegramDelivery(ctx, delivery.ID); err != nil || suppressed {
			t.Fatalf("cross-session question suppressed: %v %v", suppressed, err)
		}
	}
	progressEvent(t, env, 4, "turn_completed", "turn-one", "")
	assertAsyncSessionState(t, env, "idle", "")
	assertTypingTargets(t, env.store)
	if pending, err := env.store.PendingApproval(ctx, uuid.MustParse(approval.ID)); err != nil || !pending.Async {
		t.Fatalf("turn completion lost async question: %#v %v", pending, err)
	}
	// Answering a previous turn's async question cannot stop a subsequent turn.
	progressEvent(t, env, 5, "turn_started", "turn-two", "")
	approval.State = "approved"
	asyncQuestionEvent(t, env, 6, 1, "approval_resolved", approval)
	assertAsyncSessionState(t, env, "running", "turn-two")
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20})
	if _, err := env.store.PendingApproval(ctx, uuid.MustParse(approval.ID)); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("resolved async question remained pending: %v", err)
	}
}

func TestAsyncQuestionDeliverySurvivesProgressReplacementAndFinal(t *testing.T) {
	for _, sent := range []bool{false, true} {
		name := "queued"
		if sent {
			name = "sent"
		}
		t.Run(name, func(t *testing.T) {
			env := progressEnv(t)
			ctx := context.Background()
			progressEvent(t, env, 2, "turn_started", "turn-one", "")
			progressEvent(t, env, 3, "agent_progress_message", "turn-one", "")
			checkpointProgress(t, env.store, claimProgress(t, env.store, 1)[0], 101)
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async-durable", ThreadID: "thread-1", TurnID: "turn-one", Type: "input", Async: true,
				Questions: []protocol.Question{{ID: "question", Prompt: "Your preference?"}}}
			asyncQuestionEvent(t, env, 4, 1, "user_input_requested", approval)
			question := claimProgress(t, env.store, 1)[0]
			if sent {
				if _, err := env.store.PrepareDeliveryChunks(ctx, question.ID, []json.RawMessage{json.RawMessage(`{"text":"Your preference?"}`)}); err != nil {
					t.Fatal(err)
				}
				if err := env.store.MarkDeliveryChunkSent(ctx, question.ID, 0, 701, env.session.String(), "turn-one", approval.ID, "question"); err != nil {
					t.Fatal(err)
				}
			}
			progressEvent(t, env, 5, "agent_progress_message", "turn-one", "")
			replacement := claimProgress(t, env.store, 1)[0]
			assertProgressTarget(t, env.store, replacement, 101)
			checkpointProgress(t, env.store, replacement, 101)
			progressEvent(t, env, 6, "turn_completed", "turn-one", "")
			checkpointProgress(t, env.store, claimProgress(t, env.store, 1)[0], 201)
			reopened, err := Open(ctx, env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !sent {
				if suppressed, err := reopened.SuppressTelegramDelivery(ctx, question.ID); err != nil || suppressed {
					t.Fatalf("final suppressed queued question: %v %v", suppressed, err)
				}
			}
			deletions, err := reopened.ClaimTelegramDeletions(ctx, 100)
			if err != nil || len(deletions) != 1 || deletions[0].MessageID != 101 {
				t.Fatalf("cleanup must remove only replaced progress: %#v %v", deletions, err)
			}
			if pending, err := reopened.PendingApproval(ctx, uuid.MustParse(approval.ID)); err != nil || pending.Questions[0].Prompt != "Your preference?" {
				t.Fatalf("restart lost unanswered question: %#v %v", pending, err)
			}
			if sent {
				var routed string
				if err := reopened.pool.QueryRow(ctx, `SELECT approval_id FROM bot_message_routes WHERE bot_id='bot' AND chat_id=20 AND message_id=701`).Scan(&routed); err != nil || routed != approval.ID {
					t.Fatalf("question answer route lost after final: %q %v", routed, err)
				}
			}
		})
	}
}

func TestAsyncQuestionGenerationRestartInvalidatesOldAndAcceptsRecoveredRequest(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "surviving-async-request", ThreadID: "thread-1", TurnID: "past-turn", Type: "input", Async: true,
		Questions: []protocol.Question{{ID: "answer", Prompt: "Continue?"}}}
	original := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", approval)
	assertAsyncSessionState(t, env, "idle", "")
	assertTypingTargets(t, env.store)
	token, err := env.store.CreateCallback(ctx, Callback{Action: "input", BotID: "bot", UserID: 10, ChatID: 20,
		SessionID: env.session, RuntimeID: env.runtime, Generation: 1, ApprovalID: uuid.MustParse(approval.ID), QuestionID: "answer", Answer: "Yes", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	progressEventAtGeneration(t, env, 3, 2, "runtime_started", "", "")
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, original); err != nil {
		t.Fatalf("durable replay rejected: %v", err)
	}
	asyncQuestionEvent(t, env, 4, 1, "user_input_requested", approval)
	var state string
	if err := env.store.pool.QueryRow(ctx, `SELECT state FROM approvals WHERE approval_id=$1`, approval.ID).Scan(&state); err != nil || state != "cleared" {
		t.Fatalf("stale replay revived old request: %q %v", state, err)
	}
	if _, err := env.store.PendingApproval(ctx, uuid.MustParse(approval.ID)); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("old generation question still answerable: %v", err)
	}
	in := telegramUpdate(env, 2)
	in.CallbackToken = token
	if rejected, err := env.store.AcceptTelegram(ctx, in); err != nil || rejected.ErrorCode != "callback_invalid" {
		t.Fatalf("old generation answer accepted: %#v %v", rejected, err)
	}
	// A resumed app-server request keeps its Codex request ID but receives a new
	// approval identity tied to the current runtime generation.
	approval.ID = uuid.NewString()
	asyncQuestionEvent(t, env, 5, 2, "user_input_requested", approval)
	progressEventAtGeneration(t, env, 6, 2, "turn_started", "new-turn", "")
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 702, env.session, "past-turn", uuid.MustParse(approval.ID), "answer"); err != nil {
		t.Fatal(err)
	}
	in = telegramUpdate(env, 3)
	in.ReplyToMessageID, in.Text = 702, "Yes"
	accepted, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("recovered question answer rejected: %#v %v", accepted, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].RuntimeGeneration != 2 || commands[0].ExpectedTurnID != "" || commands[0].Arguments.ApprovalID != approval.ID {
		t.Fatalf("recovered question routed to stale runtime/turn: %#v %v", commands, err)
	}
	assertAsyncSessionState(t, env, "running", "new-turn")
}

func TestAsyncQuestionResponseFailureRecovery(t *testing.T) {
	for _, test := range []struct {
		name, outcome, code        string
		dispatched, retry, cleared bool
	}{
		{name: "rejected active turn", outcome: "command_failed", code: protocol.StaleTurn, dispatched: true, retry: true},
		{name: "rejected acknowledgement", outcome: "ack", code: protocol.StaleTurn, dispatched: true, retry: true},
		{name: "expired before dispatch", outcome: "expired", retry: true},
		{name: "expired after dispatch", outcome: "expired", dispatched: true},
		{name: "unknown result", outcome: "command_result_unknown", code: protocol.OutcomeUnknown, dispatched: true},
		{name: "unknown acknowledgement", outcome: "ack", code: protocol.OutcomeUnknown, dispatched: true},
		{name: "request no longer pending", outcome: "command_failed", code: protocol.ApprovalNotPending, dispatched: true, cleared: true},
		{name: "request rejected acknowledgement", outcome: "ack", code: protocol.ApprovalNotPending, dispatched: true, cleared: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:retryable", ThreadID: "thread-1", TurnID: "past-turn", Type: "input", Async: true,
				Questions: []protocol.Question{{ID: "answer", Prompt: "Continue?"}}}
			asyncQuestionEvent(t, env, 2, 1, "user_input_requested", approval)
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 703, env.session, "past-turn", uuid.MustParse(approval.ID), "answer"); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.ReplyToMessageID, in.Text = 703, "Yes"
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" {
				t.Fatalf("initial answer: %#v %v", accepted, err)
			}
			id := uuid.MustParse(accepted.CommandID)
			if test.dispatched {
				if err := env.store.MarkDispatched(ctx, env.worker, id); err != nil {
					t.Fatal(err)
				}
			}
			seq := uint64(3)
			switch test.outcome {
			case "ack":
				if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: id.String(), Status: "rejected_invalid_state", Error: &protocol.Error{Code: test.code}}); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := env.store.pool.Exec(ctx, `UPDATE commands SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE command_id=$1`, id); err != nil {
					t.Fatal(err)
				}
				if err := env.store.ExpireCommands(ctx); err != nil {
					t.Fatal(err)
				}
			default:
				raw, _ := json.Marshal(protocol.Result{CommandID: id.String(), Error: &protocol.Error{Code: test.code}})
				event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
					SessionID: env.session.String(), Kind: test.outcome, OccurredAt: time.Now().UTC(), Data: raw}
				if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
					t.Fatal(err)
				}
				seq++
			}
			var state, draft string
			var claimed *string
			if err := env.store.pool.QueryRow(ctx, `SELECT state,input_answers,response_command_id FROM approvals WHERE approval_id=$1`, approval.ID).Scan(&state, &draft, &claimed); err != nil {
				t.Fatal(err)
			}
			wantState := "pending"
			if test.cleared {
				wantState = "cleared"
			}
			if state != wantState || (claimed == nil) != test.retry || (draft == "{}") != test.retry {
				t.Fatalf("recovery state=%s draft=%s claim=%v, retry=%v cleared=%v", state, draft, claimed, test.retry, test.cleared)
			}
			page, err := env.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0)
			if err != nil || (len(page.Requests) == 1) != test.retry {
				t.Fatalf("pending menu after outcome: %#v %v", page, err)
			}
			if !test.retry {
				return
			}
			var recorded string
			if err := env.store.pool.QueryRow(ctx, `SELECT json_extract(payload,'$.arguments.answers.answer[0]') FROM commands WHERE command_id=$1`, id).Scan(&recorded); err != nil || recorded != "Yes" {
				t.Fatalf("failed command lost submitted answer: %q %v", recorded, err)
			}
			in.UpdateID = 3
			retried, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || retried.CommandID == "" || retried.CommandID == id.String() {
				t.Fatalf("retry failed: %#v %v", retried, err)
			}
			// A delayed failure event for the first command must not release the
			// replacement command's claim and admit duplicate answers.
			raw, _ := json.Marshal(protocol.Result{CommandID: id.String(), Error: &protocol.Error{Code: protocol.StaleTurn}})
			event := protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1,
				SessionID: env.session.String(), Kind: "command_failed", OccurredAt: time.Now().UTC(), Data: raw}
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
				t.Fatal(err)
			}
			if err := env.store.pool.QueryRow(ctx, `SELECT response_command_id FROM approvals WHERE approval_id=$1`, approval.ID).Scan(&claimed); err != nil || claimed == nil || *claimed != retried.CommandID {
				t.Fatalf("old failure released new claim: %v %v", claimed, err)
			}
		})
	}
}
