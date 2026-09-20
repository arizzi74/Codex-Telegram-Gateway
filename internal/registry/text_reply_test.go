package registry

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestExplicitTextReplyKeepsQuestionRouteAndSelection(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "blocking"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='waiting-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			id := insertPendingQuestion(t, env, env.session, "waiting-turn", async,
				protocol.Question{ID: "first", Prompt: "Which computer?"},
				protocol.Question{ID: "second", Prompt: "What output?"})
			other := uuid.New()
			insertRouteSession(t, env, other, "other-thread", "")
			selection := telegramUpdate(env, 2)
			selection.Action, selection.Target = "select", other.String()
			if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
				t.Fatal(err)
			}
			assertSelection := func() {
				t.Helper()
				var selected string
				if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=0`).Scan(&selected); err != nil || selected != other.String() {
					t.Fatalf("question changed active selection: %q %v", selected, err)
				}
			}
			assertNoCommand := func() {
				t.Helper()
				commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
				if err != nil || len(commands) != 0 {
					t.Fatalf("question queued a premature command: %#v %v", commands, err)
				}
			}
			staleButton := pendingQuestionCallback(t, env, env.session, id, "input_prompt", "first", "")
			press := telegramUpdate(env, 3)
			press.CallbackToken = pendingQuestionCallback(t, env, env.session, id, "input_prompt", "first", "")
			prompt, err := env.store.AcceptTelegram(ctx, press)
			if err != nil || !prompt.TextReply || prompt.View != "input_prompt" || prompt.QuestionID != "first" || prompt.ApprovalID != id.String() || prompt.SessionID != env.session.String() || prompt.RuntimeID != env.runtime.String() || prompt.UserID != 10 || prompt.CommandID != "" {
				t.Fatalf("explicit text reply lost its request: %#v %v", prompt, err)
			}
			assertSelection()
			assertNoCommand()
			if duplicate, err := env.store.AcceptTelegram(ctx, press); err != nil || !duplicate.Duplicate || duplicate.TextReply {
				t.Fatalf("callback redelivery reopened composer: %#v %v", duplicate, err)
			}
			press.UpdateID = 4
			if rejected, err := env.store.AcceptTelegram(ctx, press); err != nil || rejected.ErrorCode != "callback_invalid" || rejected.TextReply {
				t.Fatalf("used text button remained valid: %#v %v", rejected, err)
			}
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 300, env.session, "waiting-turn", id, prompt.QuestionID); err != nil {
				t.Fatal(err)
			}
			answer := telegramUpdate(env, 5)
			answer.Action, answer.ReplyToMessageID, answer.Text = "text", 300, "The same Mac"
			partial, err := env.store.AcceptTelegram(ctx, answer)
			if err != nil || partial.View != "input_pending" || partial.TextReply || partial.QuestionID != "second" || partial.SessionID != env.session.String() {
				t.Fatalf("reply did not answer the displayed question: %#v %v", partial, err)
			}
			if duplicate, err := env.store.AcceptTelegram(ctx, answer); err != nil || !duplicate.Duplicate {
				t.Fatalf("answer update was not deduplicated: %#v %v", duplicate, err)
			}
			var raw []byte
			var answers map[string][]string
			if err := env.store.pool.QueryRow(ctx, `SELECT input_answers FROM approvals WHERE approval_id=$1`, id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &answers); err != nil || !reflect.DeepEqual(answers, map[string][]string{"first": {"The same Mac"}}) {
				t.Fatalf("partial answer consumed another question: %#v %v", answers, err)
			}
			assertNoCommand()
			assertSelection()
			press.UpdateID, press.CallbackToken = 6, staleButton
			if rejected, err := env.store.AcceptTelegram(ctx, press); err != nil || rejected.ErrorCode != "callback_invalid" || rejected.TextReply {
				t.Fatalf("answered question reopened its composer: %#v %v", rejected, err)
			}
			press.UpdateID, press.CallbackToken = 7, pendingQuestionCallback(t, env, env.session, id, "question_open", "", "")
			opened, err := env.store.AcceptTelegram(ctx, press)
			if err != nil || opened.View != "input_prompt" || opened.QuestionID != "second" || opened.TextReply {
				t.Fatalf("opening a pending question forced a text reply: %#v %v", opened, err)
			}
			press.UpdateID, press.CallbackToken = 8, pendingQuestionCallback(t, env, env.session, id, "input_prompt", "second", "")
			second, err := env.store.AcceptTelegram(ctx, press)
			if err != nil || !second.TextReply || second.QuestionID != "second" {
				t.Fatalf("second composer: %#v %v", second, err)
			}
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 301, env.session, "waiting-turn", id, second.QuestionID); err != nil {
				t.Fatal(err)
			}
			answer.UpdateID, answer.ReplyToMessageID, answer.Text = 9, 301, "Connection refused"
			complete, err := env.store.AcceptTelegram(ctx, answer)
			if err != nil || complete.CommandID == "" || complete.SessionID != env.session.String() || complete.TextReply {
				t.Fatalf("completed response lost its origin: %#v %v", complete, err)
			}
			if duplicate, err := env.store.AcceptTelegram(ctx, answer); err != nil || !duplicate.Duplicate {
				t.Fatalf("completed answer redelivery was not deduplicated: %#v %v", duplicate, err)
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			wantAnswers := map[string][]string{"first": {"The same Mac"}, "second": {"Connection refused"}}
			if err != nil || len(commands) != 1 || commands[0].ID != complete.CommandID || commands[0].SessionID != env.session.String() || commands[0].Operation != protocol.InputResponse || commands[0].Arguments.ApprovalID != id.String() || !reflect.DeepEqual(commands[0].Arguments.Answers, wantAnswers) {
				t.Fatalf("answer command was duplicated or misrouted: %#v %v", commands, err)
			}
			assertSelection()
			press.UpdateID, press.CallbackToken = 10, pendingQuestionCallback(t, env, env.session, id, "input_prompt", "second", "")
			if rejected, err := env.store.AcceptTelegram(ctx, press); err != nil || rejected.ErrorCode != "callback_invalid" || rejected.TextReply {
				t.Fatalf("completed request reopened composer: %#v %v", rejected, err)
			}
		})
	}
}

func TestTextReplyRejectsStaleQuestionContext(t *testing.T) {
	for _, scenario := range []string{"unknown_question", "runtime_restarted", "turn_changed"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='waiting-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			id := insertPendingQuestion(t, env, env.session, "waiting-turn", false, protocol.Question{ID: "q1", Prompt: "Which computer?"})
			question := "q1"
			switch scenario {
			case "unknown_question":
				question = "missing"
			case "runtime_restarted":
				if _, err := env.store.pool.Exec(ctx, `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime); err != nil {
					t.Fatal(err)
				}
			case "turn_changed":
				if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='new-turn' WHERE session_id=$1`, env.session); err != nil {
					t.Fatal(err)
				}
			}
			press := telegramUpdate(env, 2)
			press.CallbackToken = pendingQuestionCallback(t, env, env.session, id, "input_prompt", question, "")
			if result, err := env.store.AcceptTelegram(ctx, press); err != nil || result.ErrorCode != "callback_invalid" || result.TextReply || result.CommandID != "" {
				t.Fatalf("stale text request accepted: %#v %v", result, err)
			}
		})
	}
}
