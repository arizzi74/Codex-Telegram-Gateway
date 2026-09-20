package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func answerProgressEnv(t *testing.T) eventTestEnv {
	t.Helper()
	env := newHistoryTestEnv(t)
	for _, row := range claimProgress(t, env.store, 1) {
		if err := env.store.MarkDeliverySent(t.Context(), row.ID, 90, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	progressEvent(t, env, 4, "tool_progress_message", "turn-a", "")
	for i, row := range claimProgress(t, env.store, 2) {
		checkpointProgress(t, env.store, row, 100+int64(i))
	}
	return env
}

func answerProgressQuestion(t *testing.T, env eventTestEnv, count int) uuid.UUID {
	t.Helper()
	questions := []protocol.Question{{ID: "first", Prompt: "Which computer?", Options: []string{"Mac", "Linux"}}}
	if count == 2 {
		questions = append(questions, protocol.Question{ID: "second", Prompt: "What result?"})
	}
	id := insertPendingQuestion(t, env, env.session, "turn-a", false, questions...)
	for i, question := range questions {
		if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, 900+int64(i), env.session, "turn-a", id, question.ID); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func answerProgressUI(t *testing.T, store *Store) Delivery {
	t.Helper()
	row := claimProgress(t, store, 1)[0]
	var result AcceptResult
	if row.Kind != "ui_response" || json.Unmarshal(row.Payload, &result) != nil || !result.ProgressReposition {
		t.Fatalf("answer acknowledgement did not precede progress: %+v %+v", row, result)
	}
	return row
}

func deleteAnswerProgress(t *testing.T, store *Store, count int) []TelegramDeletion {
	t.Helper()
	rows, err := store.ClaimTelegramDeletions(t.Context(), 100)
	if err != nil || len(rows) != count {
		t.Fatalf("answer cleanup: got %d, want %d: %v", len(rows), count, err)
	}
	for _, row := range rows {
		if err := store.MarkTelegramDeletionDone(t.Context(), row.ID); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

func TestAnswerProgressMovesAfterTypedAndButtonAnswers(t *testing.T) {
	for _, button := range []bool{false, true} {
		name := "typed"
		if button {
			name = "button"
		}
		t.Run(name, func(t *testing.T) {
			env := answerProgressEnv(t)
			ctx := t.Context()
			approval := answerProgressQuestion(t, env, 2)
			answer := telegramUpdate(env, 2)
			answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Mac"
			if button {
				answer.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "input", "first", "Mac")
			}
			accepted, err := env.store.AcceptTelegram(ctx, answer)
			if err != nil || !accepted.ProgressReposition || accepted.View != "input_pending" || accepted.QuestionID != "second" {
				t.Fatalf("partial answer: %+v %v", accepted, err)
			}
			if duplicate, err := env.store.AcceptTelegram(ctx, answer); err != nil || !duplicate.Duplicate || duplicate.ProgressReposition {
				t.Fatalf("duplicate answer moved progress again: %+v %v", duplicate, err)
			}
			ack := answerProgressUI(t, env.store)
			deleted := deleteAnswerProgress(t, env.store, 2)
			for _, row := range deleted {
				if row.MessageID != 100 && row.MessageID != 101 {
					t.Fatalf("answer deleted a question or acknowledgement: %+v", row)
				}
			}
			claimProgress(t, env.store, 0)
			if err := env.store.RetryDelivery(ctx, ack.ID, time.Hour, "Telegram retry"); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			claimProgress(t, reopened, 0)
			if _, err := reopened.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=$2 WHERE delivery_id=$1`, ack.ID, time.Now().UTC().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			ack = answerProgressUI(t, reopened)
			if _, err := reopened.PrepareDeliveryChunks(ctx, ack.ID, []json.RawMessage{json.RawMessage(`{"text":"Answer saved"}`), json.RawMessage(`{"text":"Next question"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := reopened.MarkDeliveryChunkSent(ctx, ack.ID, 0, 910, "", "", ""); err != nil {
				t.Fatal(err)
			}
			claimProgress(t, reopened, 0)
			if err := reopened.MarkDeliveryChunkSent(ctx, ack.ID, 1, 911, "", "", ""); err != nil {
				t.Fatal(err)
			}
			for i, row := range claimProgress(t, reopened, 2) {
				assertProgressTarget(t, reopened, row, 0)
				checkpointProgress(t, reopened, row, 200+int64(i))
			}
			answer.UpdateID, answer.CallbackToken, answer.ReplyToMessageID, answer.Text = 3, "", 901, "Connected"
			complete, err := env.store.AcceptTelegram(ctx, answer)
			if err != nil || !complete.ProgressReposition || complete.CommandID == "" || complete.SessionID != env.session.String() {
				t.Fatalf("completed answer: %+v %v", complete, err)
			}
			ack = answerProgressUI(t, env.store)
			if err := env.store.MarkDeliverySent(ctx, ack.ID, 912, "", "", ""); err != nil {
				t.Fatal(err)
			}
			// Even a sent acknowledgement cannot replay before deletion completes.
			claimProgress(t, env.store, 0)
			deleteAnswerProgress(t, env.store, 2)
			for _, row := range claimProgress(t, env.store, 2) {
				assertProgressTarget(t, env.store, row, 0)
				var event protocol.Event
				if err := json.Unmarshal(row.Payload, &event); err != nil || (event.Seq != 3 && event.Seq != 4) {
					t.Fatalf("answer replayed old progress: %+v %v", event, err)
				}
			}
			var routes int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM bot_message_routes WHERE message_id IN (900,901)`).Scan(&routes); err != nil || routes != 2 {
				t.Fatalf("question reply routes were removed: %d %v", routes, err)
			}
		})
	}
}

func TestAnswerProgressWaitsForLateRevokedSendCheckpoint(t *testing.T) {
	env := answerProgressEnv(t)
	ctx := t.Context()
	answerProgressQuestion(t, env, 1)
	progressEvent(t, env, 5, "agent_progress_message", "turn-a", "")
	late := claimProgress(t, env.store, 1)[0]
	if _, err := env.store.PrepareDeliveryChunks(ctx, late.ID, []json.RawMessage{json.RawMessage(`{"text":"In flight"}`)}); err != nil {
		t.Fatal(err)
	}
	answer := telegramUpdate(env, 2)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Mac"
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || !result.ProgressReposition {
		t.Fatalf("answer: %+v %v", result, err)
	}
	ack := answerProgressUI(t, env.store)
	if err := env.store.MarkDeliverySent(ctx, ack.ID, 910, "", "", ""); err != nil {
		t.Fatal(err)
	}
	deleteAnswerProgress(t, env.store, 2)
	claimProgress(t, env.store, 0)
	// A send that was already in Telegram completes after the answer transaction.
	if err := env.store.MarkDeliveryChunkSent(ctx, late.ID, 0, 303, "", "", ""); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, env.store, 0)
	if rows := deleteAnswerProgress(t, env.store, 1); rows[0].MessageID != 303 {
		t.Fatalf("late message was not retired: %+v", rows)
	}
	for _, row := range claimProgress(t, env.store, 2) {
		assertProgressTarget(t, env.store, row, 0)
	}
}

func TestAnswerProgressDeletionFailuresDoNotFreezeChat(t *testing.T) {
	env := answerProgressEnv(t)
	ctx := t.Context()
	answerProgressQuestion(t, env, 1)
	answer := telegramUpdate(env, 2)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Mac"
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || !result.ProgressReposition {
		t.Fatalf("answer: %+v %v", result, err)
	}
	ack := answerProgressUI(t, env.store)
	if err := env.store.MarkDeliverySent(ctx, ack.ID, 910, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		deletions, err := env.store.ClaimTelegramDeletions(ctx, 100)
		if err != nil || len(deletions) != 2 {
			t.Fatalf("cleanup attempt %d: %+v %v", attempt, deletions, err)
		}
		claimProgress(t, env.store, 0)
		for _, deletion := range deletions {
			if deletion.Attempt != attempt {
				t.Fatalf("wrong attempt: %+v", deletion)
			}
			if err := env.store.RetryTelegramDeletion(ctx, deletion.ID, time.Hour, "Telegram refused deletion"); err != nil {
				t.Fatal(err)
			}
		}
		if attempt < 3 {
			claimProgress(t, env.store, 0)
			if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_progress_messages SET next_attempt_at=$1 WHERE status='pending'`, time.Now().UTC().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, row := range claimProgress(t, env.store, 2) {
		assertProgressTarget(t, env.store, row, 0)
		checkpointProgress(t, env.store, row, 200+int64(i))
	}
	// Old copies still have durable cleanup work; successful retries remove
	// only those old IDs, never the fresh messages at the bottom.
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_progress_messages SET next_attempt_at=$1 WHERE retire_requested=1`, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, deletion := range deleteAnswerProgress(t, env.store, 2) {
		if deletion.MessageID != 100 && deletion.MessageID != 101 {
			t.Fatalf("cleanup removed fresh progress: %+v", deletion)
		}
	}
	deleteAnswerProgress(t, env.store, 0)
}

func TestAnswerProgressIgnoresOpenInvalidAndRepeatedQuestionButtons(t *testing.T) {
	env := answerProgressEnv(t)
	ctx := t.Context()
	approval := answerProgressQuestion(t, env, 1)
	for i, question := range []string{"first", "unknown"} {
		in := telegramUpdate(env, 2+int64(i))
		in.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "input_prompt", question, "")
		result, err := env.store.AcceptTelegram(ctx, in)
		if err != nil || result.ProgressReposition || (i == 0 && !result.TextReply) || (i == 1 && result.ErrorCode == "") {
			t.Fatalf("open/invalid button: %+v %v", result, err)
		}
		deleteAnswerProgress(t, env.store, 0)
		row := claimProgress(t, env.store, 1)[0]
		if err := env.store.MarkDeliverySent(ctx, row.ID, 910+int64(i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	answer := telegramUpdate(env, 4)
	answer.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "input", "first", "Mac")
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || !result.ProgressReposition {
		t.Fatalf("answer: %+v %v", result, err)
	}
	ack := answerProgressUI(t, env.store)
	if err := env.store.MarkDeliverySent(ctx, ack.ID, 915, "", "", ""); err != nil {
		t.Fatal(err)
	}
	deleteAnswerProgress(t, env.store, 2)
	for i, row := range claimProgress(t, env.store, 2) {
		checkpointProgress(t, env.store, row, 200+int64(i))
	}
	answer.UpdateID = 5
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || result.ProgressReposition || result.ErrorCode != "callback_invalid" {
		t.Fatalf("used answer moved progress: %+v %v", result, err)
	}
	deleteAnswerProgress(t, env.store, 0)
}

func TestAnswerProgressPreservesOtherDestinations(t *testing.T) {
	env := answerProgressEnv(t)
	ctx := t.Context()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings (bot_id,user_id,chat_id,message_thread_id,session_id) VALUES ('bot',10,20,4,$1),('other-bot',10,20,0,$1),('bot',10,21,0,$1)`, env.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 5, "agent_progress_message", "turn-a", "")
	for i, row := range claimProgress(t, env.store, 4) {
		messageID := 300 + int64(i)
		if row.BotID == "bot" && row.ChatID == 20 && row.TopicID == 0 {
			messageID, _ = env.store.TelegramProgressTarget(ctx, row.ID)
		}
		checkpointProgress(t, env.store, row, messageID)
	}
	answerProgressQuestion(t, env, 1)
	answer := telegramUpdate(env, 2)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Mac"
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || !result.ProgressReposition {
		t.Fatalf("answer: %+v %v", result, err)
	}
	ack := answerProgressUI(t, env.store)
	if err := env.store.MarkDeliverySent(ctx, ack.ID, 910, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, row := range deleteAnswerProgress(t, env.store, 2) {
		if row.BotID != "bot" || row.ChatID != 20 || row.TopicID != 0 {
			t.Fatalf("answer retired another destination: %+v", row)
		}
	}
	for _, row := range claimProgress(t, env.store, 2) {
		if row.BotID != "bot" || row.ChatID != 20 || row.TopicID != 0 {
			t.Fatalf("answer replayed another destination: %+v", row)
		}
	}
	var unaffected int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_progress_messages WHERE (bot_id<>'bot' OR chat_id<>20 OR message_thread_id<>0) AND status='pending' AND retire_requested=0`).Scan(&unaffected); err != nil || unaffected != 3 {
		t.Fatalf("other destinations changed: %d %v", unaffected, err)
	}
}

func TestAnswerProgressKeepsSelectionAndVisibleMultiSessionTurns(t *testing.T) {
	for _, multi := range []bool{false, true} {
		name := "selected_only"
		if multi {
			name = "multisession"
		}
		t.Run(name, func(t *testing.T) {
			env := answerProgressEnv(t)
			ctx := t.Context()
			other := env
			other.session = uuid.New()
			insertRouteSession(t, env, other.session, "other-thread", "")
			if multi {
				acceptModeUpdate(t, env, 2, "multisession", "", "on")
				row := claimProgress(t, env.store, 1)[0]
				if err := env.store.MarkDeliverySent(ctx, row.ID, 91, "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			progressEvent(t, other, 5, "turn_started", "turn-b", "")
			progressEvent(t, other, 6, "agent_progress_message", "turn-b", "")
			progressEvent(t, other, 7, "tool_progress_message", "turn-b", "")
			count := 2
			if multi {
				count = 4
				for i, row := range claimProgress(t, env.store, 2) {
					checkpointProgress(t, env.store, row, 200+int64(i))
				}
			} else {
				claimProgress(t, env.store, 0)
			}
			approval := insertPendingQuestion(t, env, other.session, "turn-b", false, protocol.Question{ID: "q", Prompt: "Continue?"})
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 900, other.session, "turn-b", approval, "q"); err != nil {
				t.Fatal(err)
			}
			answer := telegramUpdate(env, 3)
			answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Continue"
			if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || !result.ProgressReposition || result.SessionID != other.session.String() {
				t.Fatalf("other-session answer: %+v %v", result, err)
			}
			ack := answerProgressUI(t, env.store)
			if err := env.store.MarkDeliverySent(ctx, ack.ID, 910, "", "", ""); err != nil {
				t.Fatal(err)
			}
			deleteAnswerProgress(t, env.store, count)
			seen := make(map[string]int)
			for _, row := range claimProgress(t, env.store, count) {
				var event protocol.Event
				if err := json.Unmarshal(row.Payload, &event); err != nil {
					t.Fatal(err)
				}
				seen[event.SessionID]++
				assertProgressTarget(t, env.store, row, 0)
			}
			if seen[env.session.String()] != 2 || (multi && seen[other.session.String()] != 2) || (!multi && len(seen) != 1) {
				t.Fatalf("wrong visible turns replayed: %+v", seen)
			}
			var selected string
			if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=0`).Scan(&selected); err != nil || selected != env.session.String() {
				t.Fatalf("answer changed selection: %s %v", selected, err)
			}
		})
	}
}

func TestAnswerProgressDoesNotReplayCompletedTurn(t *testing.T) {
	env := answerProgressEnv(t)
	ctx := t.Context()
	approval := insertPendingQuestion(t, env, env.session, "turn-a", true, protocol.Question{ID: "q", Prompt: "Continue?"})
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 900, env.session, "turn-a", approval, "q"); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 5, "turn_completed", "turn-a", "")
	final := claimProgress(t, env.store, 1)[0]
	if err := env.store.MarkDeliverySent(ctx, final.ID, 500, "", "", ""); err != nil {
		t.Fatal(err)
	}
	answer := telegramUpdate(env, 2)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 900, "Continue"
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || !result.ProgressReposition {
		t.Fatalf("async answer: %+v %v", result, err)
	}
	ack := answerProgressUI(t, env.store)
	if err := env.store.MarkDeliverySent(ctx, ack.ID, 910, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, row := range deleteAnswerProgress(t, env.store, 2) {
		if row.MessageID == 500 {
			t.Fatal("final response retired")
		}
	}
	claimProgress(t, env.store, 0)
}
