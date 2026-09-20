package registry

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestReplyBeforeQuestionCheckpointRetriesWithoutChangingSelectedSession(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	for _, row := range claimProgress(t, env.store, 1) {
		if err := env.store.MarkDeliverySent(ctx, row.ID, 70, env.session.String(), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-b", "")
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:reply-race", ThreadID: "thread-b", TurnID: "old-turn", Type: "input", Async: true, Questions: []protocol.Question{{ID: "q", Prompt: "Which platform?"}}}
	data, _ := json.Marshal(approval)
	event := protocol.Event{ID: uuid.NewString(), Seq: 2, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: other.String(), Kind: "user_input_requested", Data: data, OccurredAt: time.Now().UTC()}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	question := claimProgress(t, env.store, 1)[0]
	if _, err := env.store.PrepareDeliveryChunks(ctx, question.ID, []json.RawMessage{json.RawMessage(`{"text":"Which platform?"}`)}); err != nil {
		t.Fatal(err)
	}
	reply := telegramUpdate(env, 2)
	reply.Action, reply.Text, reply.ReplyToMessageID = "text", "Linux", 777
	accepted, err := env.store.AcceptTelegram(ctx, reply)
	if !errors.Is(err, ErrTelegramReplyPending) || accepted.CommandID != "" {
		t.Fatalf("reply before checkpoint accepted: %+v %v", accepted, err)
	}
	var updates, commands int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_updates WHERE bot_id='bot' AND update_id=2`).Scan(&updates); err != nil || updates != 0 {
		t.Fatalf("retryable reply was deduplicated: %d %v", updates, err)
	}
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM commands`).Scan(&commands); err != nil || commands != 0 {
		t.Fatalf("reply created wrong-session command: %d %v", commands, err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, question.ID, 0, 777, other.String(), "old-turn", approval.ID, "q"); err != nil {
		t.Fatal(err)
	}
	accepted, err = env.store.AcceptTelegram(ctx, reply)
	if err != nil || accepted.SessionID != other.String() || accepted.ApprovalID != approval.ID || accepted.CommandID == "" {
		t.Fatalf("checkpointed question retry: %+v %v", accepted, err)
	}
	pending, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(pending) != 1 || pending[0].Operation != protocol.InputResponse || pending[0].SessionID != other.String() || pending[0].Arguments.Answers["q"][0] != "Linux" {
		t.Fatalf("reply not frozen to question: %+v %v", pending, err)
	}
	var selected string
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=0`).Scan(&selected); err != nil || selected != env.session.String() {
		t.Fatalf("answer changed selected session: %s %v", selected, err)
	}
}

func TestReplyCheckpointGuardScopesAmbiguityAndPreservesFallback(t *testing.T) {
	for _, tc := range []struct {
		name, bot, status string
		chat              int64
		attempt           int
		known, routable   bool
		pending           bool
	}{
		{"sending", "bot", "sending", 20, 1, false, true, true},
		{"failed checkpoint retry", "bot", "failed", 20, 1, false, true, true},
		{"previously attempted pending", "bot", "pending", 20, 1, false, true, true},
		{"not yet attempted", "bot", "pending", 20, 0, false, true, false},
		{"already sent", "bot", "sent", 20, 1, false, true, false},
		{"cancelled", "bot", "cancelled", 20, 1, false, true, false},
		{"other bot", "other", "sending", 20, 1, false, true, false},
		{"other chat", "bot", "sending", 21, 1, false, true, false},
		{"unroutable UI", "bot", "sending", 20, 1, false, false, false},
		{"known reply", "bot", "sending", 20, 1, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			ctx := t.Context()
			payload := json.RawMessage(`{"view":"help"}`)
			if tc.routable {
				payload, _ = json.Marshal(AcceptResult{View: "input_prompt", SessionID: env.session.String()})
			}
			// Expired leases remain ambiguous: Telegram may have accepted the
			// message before a crash, leaving its route to a recovering sender.
			if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload,status,attempt_count,next_attempt_at) VALUES($1,$2,$3,'ui_response',$4,$5,$6,$7)`, uuid.New(), tc.bot, tc.chat, string(payload), tc.status, tc.attempt, time.Now().UTC().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if tc.known {
				if err := env.store.RecordBotMessageRoute(ctx, "bot", 20, 777, env.session, "", uuid.Nil); err != nil {
					t.Fatal(err)
				}
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Text, in.ReplyToMessageID = "text", "Reply", 777
			prepared, err := env.store.PrepareTelegramImage(ctx, in)
			if tc.pending {
				if !errors.Is(err, ErrTelegramReplyPending) || prepared.imageTarget != nil {
					t.Fatalf("image froze wrong target before checkpoint: %+v %v", prepared, err)
				}
			} else if err != nil || prepared.imageTarget == nil || prepared.imageTarget.sessionID != env.session {
				t.Fatalf("unambiguous image fallback changed: %+v %v", prepared, err)
			}
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if tc.pending {
				if !errors.Is(err, ErrTelegramReplyPending) || accepted.CommandID != "" {
					t.Fatalf("ambiguous reply accepted: %+v %v", accepted, err)
				}
			} else if err != nil || accepted.SessionID != env.session.String() || accepted.CommandID == "" {
				t.Fatalf("unambiguous fallback changed: %+v %v", accepted, err)
			}
		})
	}
}

func TestUnknownReplyCannotBypassCheckpointGuardThroughTopicBinding(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableImageInput(t, env)
	ctx := t.Context()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings(bot_id,user_id,chat_id,message_thread_id,session_id) VALUES('bot',10,20,9,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.Event{Data: json.RawMessage(`{"session":{"session_id":"` + env.session.String() + `"}}`)})
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,message_thread_id,kind,payload,status,attempt_count) VALUES($1,'bot',20,9,'command_completed',$2,'sending',1)`, uuid.New(), string(payload)); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Target, in.TopicID, in.ReplyToMessageID = "codex", "status", 9, 777
	if accepted, err := env.store.AcceptTelegram(ctx, in); !errors.Is(err, ErrTelegramReplyPending) || accepted.CommandID != "" {
		t.Fatalf("topic-bound command bypassed reply checkpoint: %+v %v", accepted, err)
	}
	in.Action, in.Target = "text", ""
	if prepared, err := env.store.PrepareTelegramImage(ctx, in); !errors.Is(err, ErrTelegramReplyPending) || prepared.imageTarget != nil {
		t.Fatalf("topic image bypassed reply checkpoint: %+v %v", prepared, err)
	}
}
