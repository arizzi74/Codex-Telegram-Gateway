package registry

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const questionSourceBot = "a-source"
const questionOtherBot = "b-destination"

func questionRoutingEnv(t *testing.T, connected bool) eventTestEnv {
	t.Helper()
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{questionSourceBot, questionOtherBot, "disabled", "disallowed", "retired"} {
		bot := telegramBotRecordForTest(id, id != "disabled")
		bot.TelegramID = int64(i + 1)
		if err := env.store.SaveTelegramBotRecord(ctx, bot); err != nil {
			t.Fatal(err)
		}
		if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id) VALUES($1,10,20,0)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id) VALUES($1,10,30,3)`, questionSourceBot); err != nil {
		t.Fatal(err)
	}
	// Independent group selections must never decide which bot is connected.
	for _, id := range []string{questionSourceBot, questionOtherBot} {
		insertQuestionBinding(t, env, IncomingUpdate{BotID: id, UserID: 10, ChatID: 20}, env.session)
	}
	if connected {
		insertQuestionBinding(t, env, IncomingUpdate{BotID: questionSourceBot, UserID: 10, ChatID: 10}, env.session)
	}
	if err := env.store.ApplyTelegramChatAllowlist(ctx, "disallowed", []int64{99}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bot_retirements(bot_id) VALUES('retired')`); err != nil {
		t.Fatal(err)
	}
	return env
}

func insertQuestionBinding(t *testing.T, env eventTestEnv, in IncomingUpdate, session uuid.UUID) {
	t.Helper()
	// Direct SQL permits constructing old duplicate connections without the
	// current selector's one-bot-per-session repair.
	if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO telegram_bindings(bot_id,user_id,chat_id,message_thread_id,session_id)
        VALUES($1,$2,$3,$4,$5)`, in.BotID, in.UserID, in.ChatID, in.TopicID, session); err != nil {
		t.Fatal(err)
	}
}

func questionRoutingApproval() protocol.Approval {
	id := uuid.NewString()
	return protocol.Approval{ID: id, RequestID: "async:" + id, ThreadID: "thread-1", Type: "input", Async: true,
		Questions: []protocol.Question{{ID: "first", Prompt: "First?"}, {ID: "second", Prompt: "Second?"}}}
}

func assertQuestionRoutingTargets(t *testing.T, env eventTestEnv, event protocol.Event, want ...string) {
	t.Helper()
	rows, err := env.store.pool.Query(t.Context(), `SELECT bot_id,chat_id,message_thread_id FROM telegram_deliveries
        WHERE event_id=$1 AND status IN ('pending','sending','failed') AND visibility_revoked=0`, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var bot string
		var chat, topic int64
		if err := rows.Scan(&bot, &chat, &topic); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s/%d/%d", bot, chat, topic))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("question destinations = %v, want %v", got, want)
	}
}

func TestTelegramQuestionSourceBotControlsAutomaticBroadcast(t *testing.T) {
	for _, kind := range []string{"user_input_requested", "approval_requested"} {
		for _, tc := range []struct {
			name                             string
			connected, sourceFlag, otherFlag bool
			other                            bool
		}{
			{name: "connected default off", connected: true},
			{name: "recipient opt in cannot broaden", connected: true, otherFlag: true},
			{name: "source opt in includes recipient default off", connected: true, sourceFlag: true, other: true},
			{name: "no connection keeps all bot fallback", other: true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				env := questionRoutingEnv(t, tc.connected)
				for id, flag := range map[string]bool{questionSourceBot: tc.sourceFlag, questionOtherBot: tc.otherFlag} {
					if flag {
						if err := env.store.SetTelegramBotBroadcastQuestions(t.Context(), id, true); err != nil {
							t.Fatal(err)
						}
					}
				}
				approval := questionRoutingApproval()
				if kind == "approval_requested" {
					approval.Type, approval.Questions = "command", nil
				}
				event := asyncQuestionEvent(t, env, 2, 1, kind, approval)
				want := []string{"a-source/20/0", "a-source/30/3"}
				if tc.connected {
					want = append(want, "a-source/10/0")
				}
				if tc.other {
					want = append(want, "b-destination/20/0")
				}
				assertQuestionRoutingTargets(t, env, event, want...)
				claimed, err := env.store.ClaimDeliveries(t.Context(), 100)
				if err != nil || len(claimed) != len(want) {
					t.Fatalf("question claims=%+v err=%v, want %d", claimed, err, len(want))
				}
				for _, delivery := range claimed {
					if suppressed, err := env.store.SuppressTelegramDelivery(t.Context(), delivery.ID); err != nil || suppressed {
						t.Fatalf("valid automatic question suppressed: %v %v", suppressed, err)
					}
				}
			})
		}
	}
}

func TestTelegramQuestionUnusablePrivateConnectionKeepsFallback(t *testing.T) {
	for _, reason := range []string{"missing", "disabled", "retired", "owner chat disallowed", "different user", "topic", "archived selection", "revoked selection worker"} {
		t.Run(reason, func(t *testing.T) {
			env := questionRoutingEnv(t, true)
			ctx := t.Context()
			want := []string{"a-source/20/0", "a-source/30/3", "b-destination/20/0"}
			query := ""
			switch reason {
			case "missing":
				query = `DELETE FROM telegram_bindings WHERE bot_id='a-source' AND chat_id=10`
			case "disabled":
				query = `UPDATE telegram_bots SET enabled=0 WHERE bot_id='a-source'`
				want = []string{"b-destination/20/0"}
			case "retired":
				query = `INSERT INTO telegram_bot_retirements(bot_id) VALUES('a-source')`
				want = []string{"b-destination/20/0"}
			case "owner chat disallowed":
				if err := env.store.ApplyTelegramChatAllowlist(ctx, questionSourceBot, []int64{20, 30}); err != nil {
					t.Fatal(err)
				}
			case "different user":
				query = `UPDATE telegram_bindings SET user_id=11 WHERE bot_id='a-source' AND chat_id=10`
				want = append(want, "a-source/10/0")
			case "topic":
				query = `UPDATE telegram_bindings SET message_thread_id=1 WHERE bot_id='a-source' AND chat_id=10`
				want = append(want, "a-source/10/1")
			case "archived selection", "revoked selection worker":
				other := env
				if reason == "revoked selection worker" {
					other = inventoryWorker(t, env.store, "revoked source selection")
					if err := env.store.IngestEvent(ctx, other.worker, other.connection, other.discovery(t)); err != nil {
						t.Fatal(err)
					}
					if err := env.store.RevokeWorker(ctx, other.worker); err != nil {
						t.Fatal(err)
					}
				} else {
					other.session = uuid.New()
					insertRouteSession(t, env, other.session, "hidden-source-thread", "")
					if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=1 WHERE session_id=$1`, other.session); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_bindings SET session_id=$1 WHERE bot_id=$2 AND chat_id=10`, other.session, questionSourceBot); err != nil {
					t.Fatal(err)
				}
			}
			if query != "" {
				if _, err := env.store.pool.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			event := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", questionRoutingApproval())
			assertQuestionRoutingTargets(t, env, event, want...)
		})
	}
}

func TestTelegramQuestionLegacyDuplicatePrivateConnectionsChooseFirstBot(t *testing.T) {
	env := questionRoutingEnv(t, true)
	insertQuestionBinding(t, env, IncomingUpdate{BotID: questionOtherBot, UserID: 10, ChatID: 10}, env.session)
	if err := env.store.SetTelegramBotBroadcastQuestions(t.Context(), questionOtherBot, true); err != nil {
		t.Fatal(err)
	}
	event := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", questionRoutingApproval())
	assertQuestionRoutingTargets(t, env, event, "a-source/10/0", "a-source/20/0", "a-source/30/3")
	if err := env.store.SetTelegramBotBroadcastQuestions(t.Context(), questionSourceBot, true); err != nil {
		t.Fatal(err)
	}
	event = asyncQuestionEvent(t, env, 3, 1, "user_input_requested", questionRoutingApproval())
	assertQuestionRoutingTargets(t, env, event, "a-source/10/0", "a-source/20/0", "a-source/30/3", "b-destination/10/0", "b-destination/20/0")
}

func TestTelegramAutomaticQuestionQueueRevalidatesPolicy(t *testing.T) {
	for _, change := range []string{"broadcast off", "different connected bot", "worker revoked", "session archived"} {
		t.Run(change, func(t *testing.T) {
			env := questionRoutingEnv(t, true)
			ctx := t.Context()
			if err := env.store.SetTelegramBotBroadcastQuestions(ctx, questionSourceBot, true); err != nil {
				t.Fatal(err)
			}
			event := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", questionRoutingApproval())
			claimed, err := env.store.ClaimDeliveries(ctx, 100)
			if err != nil || len(claimed) != 4 {
				t.Fatalf("initial broadcast claims = %+v %v", claimed, err)
			}
			var disallowedID string
			for _, delivery := range claimed {
				if delivery.BotID == questionOtherBot {
					disallowedID = delivery.ID
				}
			}
			switch change {
			case "broadcast off":
				if err := env.store.SetTelegramBotBroadcastQuestions(ctx, questionSourceBot, false); err != nil {
					t.Fatal(err)
				}
			case "different connected bot":
				if _, err := env.store.SetWebUITelegramBot(ctx, env.session, questionOtherBot); err != nil {
					t.Fatal(err)
				}
				for _, delivery := range claimed {
					if delivery.BotID == questionSourceBot {
						disallowedID = delivery.ID
					}
				}
			case "worker revoked":
				if err := env.store.RevokeWorker(ctx, env.worker); err != nil {
					t.Fatal(err)
				}
			case "session archived":
				if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=1 WHERE session_id=$1`, env.session); err != nil {
					t.Fatal(err)
				}
			}
			if suppressed, err := env.store.SuppressTelegramDelivery(ctx, disallowedID); err != nil || !suppressed {
				t.Fatalf("leased question escaped changed policy: %v %v", suppressed, err)
			}
			// Pending and failed/backoff rows must be retired before another claim.
			if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET status='failed',next_attempt_at='2099-01-01T00:00:00.000000000Z' WHERE event_id=$1`, event.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.ClaimDeliveries(ctx, 100); err != nil {
				t.Fatal(err)
			}
			var status string
			var revoked bool
			if err := env.store.pool.QueryRow(ctx, `SELECT status,visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1`, disallowedID).Scan(&status, &revoked); err != nil || status != "cancelled" || !revoked {
				t.Fatalf("backoff question retained: status=%s revoked=%v err=%v", status, revoked, err)
			}
		})
	}
}

func TestTelegramAutomaticQuestionRevalidationUsesDurableEventSession(t *testing.T) {
	env := questionRoutingEnv(t, true)
	event := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", questionRoutingApproval())
	// Old delivery envelopes may lack session metadata; the durable event
	// relation remains the authority for queued notification visibility.
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET payload=json_remove(payload,'$.session_id') WHERE event_id=$1`, event.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := env.store.ClaimDeliveries(t.Context(), 100)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("durable event session lost: %+v %v", claimed, err)
	}
	for _, delivery := range claimed {
		if suppressed, err := env.store.SuppressTelegramDelivery(t.Context(), delivery.ID); err != nil || suppressed {
			t.Fatalf("durable event question suppressed: %v %v", suppressed, err)
		}
	}
}

func TestTelegramQuestionPolicyPreservesManualAndUnrelatedUIResponses(t *testing.T) {
	env := questionRoutingEnv(t, true)
	for _, tc := range []struct {
		name     string
		response AcceptResult
		suppress bool
	}{
		{name: "unrelated empty descriptor"},
		{name: "manual partial response", response: AcceptResult{View: "input_pending", SessionID: env.session.String(), UserID: 10}},
		{name: "automatic partial response", response: AcceptResult{View: "input_pending", SessionID: env.session.String()}, suppress: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.response)
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.NewString()
			if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload)
                VALUES($1,$2,20,'ui_response',$3)`, id, questionOtherBot, string(raw)); err != nil {
				t.Fatal(err)
			}
			if suppressed, err := env.store.SuppressTelegramDelivery(t.Context(), id); err != nil || suppressed != tc.suppress {
				t.Fatalf("UI response suppression=%v err=%v, want %v", suppressed, err, tc.suppress)
			}
		})
	}
}

func TestTelegramQuestionCommandOriginCannotBypassConnectedBot(t *testing.T) {
	env := questionRoutingEnv(t, true)
	ctx := t.Context()
	in := IncomingUpdate{BotID: questionOtherBot, UpdateID: 1, UserID: 10, ChatID: 20, Action: "text", Text: "Ask a question"}
	result, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || result.CommandID == "" {
		t.Fatalf("start origin command: %+v %v", result, err)
	}
	command, session := uuid.MustParse(result.CommandID), env.session
	event := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", questionRoutingApproval())
	tx, err := env.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM telegram_deliveries WHERE event_id=$1`, event.ID); err != nil {
		t.Fatal(err)
	}
	// Enqueue directly with the frozen initiating command to exercise every
	// target union even when current workers omit command IDs for questions.
	if err := enqueueEventDeliveries(ctx, tx, uuid.MustParse(event.ID), event, &session, &env.runtime, &command); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertQuestionRoutingTargets(t, env, event, "a-source/10/0", "a-source/20/0", "a-source/30/3")
}

func TestTelegramQuestionPartialAndRecoveredRequestsUseSourcePolicy(t *testing.T) {
	for _, broadcast := range []bool{false, true} {
		t.Run(fmt.Sprintf("broadcast=%v", broadcast), func(t *testing.T) {
			env := questionRoutingEnv(t, true)
			ctx := t.Context()
			if err := env.store.SetTelegramBotBroadcastQuestions(ctx, questionSourceBot, broadcast); err != nil {
				t.Fatal(err)
			}
			// A recipient's preference never changes this request's policy.
			if err := env.store.SetTelegramBotBroadcastQuestions(ctx, questionOtherBot, true); err != nil {
				t.Fatal(err)
			}
			approval := questionRoutingApproval()
			approval.ID = insertPendingQuestion(t, env, env.session, "", true, approval.Questions...).String()
			for i, bot := range []string{questionSourceBot, questionOtherBot} {
				if err := env.store.RecordBotInputRoute(ctx, bot, 20, int64(101+i), env.session, "", uuid.MustParse(approval.ID), "first"); err != nil {
					t.Fatal(err)
				}
			}
			in := IncomingUpdate{BotID: questionSourceBot, UpdateID: 1, UserID: 10, ChatID: 20, Action: "input", Target: approval.ID, QuestionID: "first", Text: "First answer"}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || result.View != "input_pending" || result.QuestionID != "second" {
				t.Fatalf("partial answer: %+v %v", result, err)
			}
			rows, err := env.store.pool.Query(ctx, `SELECT bot_id,chat_id,message_thread_id,payload FROM telegram_deliveries
                WHERE kind='ui_response' AND json_extract(payload,'$.view')='input_pending'`)
			if err != nil {
				t.Fatal(err)
			}
			got := []string{}
			var automaticOther string
			for rows.Next() {
				var bot string
				var chat, topic int64
				var raw []byte
				if err := rows.Scan(&bot, &chat, &topic, &raw); err != nil {
					t.Fatal(err)
				}
				var response AcceptResult
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if response.QuestionID != "second" || response.ApprovalID != approval.ID || (response.UserID != 0 && (bot != questionSourceBot || chat != 20)) {
					t.Fatalf("invalid partial followup: %s/%d %+v", bot, chat, response)
				}
				got = append(got, fmt.Sprintf("%s/%d/%d", bot, chat, topic))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			want := []string{"a-source/10/0", "a-source/20/0", "a-source/30/3"}
			if broadcast {
				want = append(want, "b-destination/20/0")
				if err := env.store.pool.QueryRow(ctx, `SELECT delivery_id FROM telegram_deliveries WHERE bot_id=$1
                    AND kind='ui_response' AND json_extract(payload,'$.view')='input_pending'`, questionOtherBot).Scan(&automaticOther); err != nil {
					t.Fatal(err)
				}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("partial followup destinations=%v, want %v", got, want)
			}
			// Routing restrictions never prevent a delivered copy from settling.
			if edits := questionAnswerEdits(t, env.store); len(edits) != 2 {
				t.Fatalf("old copies did not both receive answer edits: %+v", edits)
			}
			if broadcast {
				if err := env.store.SetTelegramBotBroadcastQuestions(ctx, questionSourceBot, false); err != nil {
					t.Fatal(err)
				}
				if suppressed, err := env.store.SuppressTelegramDelivery(ctx, automaticOther); err != nil || !suppressed {
					t.Fatalf("queued partial broadcast escaped flag off: %v %v", suppressed, err)
				}
			}
			// Answering through another bot remains authorized even after the
			// source stops broadcasting. A definite submission failure reopens it.
			in.BotID, in.QuestionID, in.Text = questionOtherBot, "second", "Second answer"
			submitted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || submitted.CommandID == "" {
				t.Fatalf("manual answer from other bot: %+v %v", submitted, err)
			}
			tx, err := env.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `UPDATE commands SET status='failed',error_code='invalid_argument' WHERE command_id=$1`, submitted.CommandID); err != nil {
				t.Fatal(err)
			}
			if err := recoverAsyncQuestionResponse(ctx, tx, uuid.MustParse(submitted.CommandID)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			// Worker recovery/reduced native request notifications consult the
			// current source flag, regardless of the response command's origin.
			var request []byte
			if err := env.store.pool.QueryRow(ctx, `SELECT request_payload FROM approvals WHERE approval_id=$1`, approval.ID).Scan(&request); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(request, &approval); err != nil {
				t.Fatal(err)
			}
			if err := env.store.SetTelegramBotBroadcastQuestions(ctx, questionSourceBot, broadcast); err != nil {
				t.Fatal(err)
			}
			event := asyncQuestionEvent(t, env, 2, 1, "user_input_requested", approval)
			assertQuestionRoutingTargets(t, env, event, want...)
			page, err := env.store.ListPendingQuestions(ctx, questionOtherBot, 10, 20, 0, 0)
			if err != nil || len(page.Requests) != 1 || page.Requests[0].Approval.ID != approval.ID {
				t.Fatalf("manual pending menu lost recovered question: %+v %v", page, err)
			}
		})
	}
}
