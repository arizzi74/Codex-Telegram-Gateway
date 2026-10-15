package registry

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestRecoveredQuestionCopiesWaitForFreshAnswersAcrossBots(t *testing.T) {
	for _, source := range []string{"telegram", "terminal"} {
		t.Run(source, func(t *testing.T) { recoveredQuestionCopies(t, source) })
	}
}

func recoveredQuestionCopies(t *testing.T, source string) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	for i, bot := range []string{"bot", "telegram:333"} {
		record := telegramBotRecordForTest(bot, true)
		record.TelegramID = int64(222 + i*111)
		if err := env.store.SaveTelegramBotRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-bot-thread", "")
	acceptBotSessionUpdate(t, env, "telegram:333", 1, "select", other.String(), "")
	request := insertPendingQuestion(t, env, env.session, "", true,
		protocol.Question{ID: "first", Prompt: "First question?"},
		protocol.Question{ID: "second", Prompt: "Second question?"})
	registerCopies := func(first, second int64) {
		t.Helper()
		for _, bot := range []string{"bot", "telegram:333"} {
			for question, message := range map[string]int64{"first": first, "second": second} {
				if err := env.store.RecordBotInputRoute(ctx, bot, 20, message, env.session, "", request, question); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	answer := func(bot string, update, message int64, text string) AcceptResult {
		t.Helper()
		in := telegramUpdate(env, update)
		in.BotID, in.Action, in.ReplyToMessageID, in.Text = bot, "text", message, text
		result, err := env.store.AcceptTelegram(ctx, in)
		if err != nil || result.ErrorCode != "" {
			t.Fatalf("answer from %s: %+v %v", bot, result, err)
		}
		return result
	}
	registerCopies(101, 102)
	answer("bot", 10, 101, "Original first")
	submitted := answer("telegram:333", 10, 102, "Original second")
	if submitted.CommandID == "" {
		t.Fatal("complete answers did not submit a response")
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

	// A fresh ordinary question message, or a late copy of a reopened prompt,
	// must not immediately acquire the failed submission's retained summary.
	registerCopies(201, 202)
	assertFreshEdits := func(want map[int64]string) {
		t.Helper()
		rows, err := env.store.pool.Query(ctx, `SELECT bot_id,payload FROM telegram_deliveries
            WHERE kind='question_answered' AND visibility_revoked=0
              AND json_extract(payload,'$.message_id') IN (201,202)`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := make(map[string]map[int64]string)
		count := 0
		for rows.Next() {
			var bot string
			var raw []byte
			if err := rows.Scan(&bot, &raw); err != nil {
				t.Fatal(err)
			}
			var edit QuestionAnswerEdit
			if err := json.Unmarshal(raw, &edit); err != nil {
				t.Fatal(err)
			}
			if seen[bot] == nil {
				seen[bot] = make(map[int64]string)
			}
			seen[bot][edit.MessageID] = edit.Answer
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if count != 2*len(want) {
			t.Fatalf("fresh copies received stale or duplicate edits: %v, want %v per bot", seen, want)
		}
		for _, bot := range []string{"bot", "telegram:333"} {
			for message, text := range want {
				if seen[bot][message] != text {
					t.Fatalf("%s message %d: got %q want %q", bot, message, seen[bot][message], text)
				}
			}
		}
	}
	assertFreshEdits(nil)
	var native protocol.Approval
	if source == "terminal" {
		var raw []byte
		if err := env.store.pool.QueryRow(ctx, `SELECT request_payload FROM approvals WHERE approval_id=$1`, request).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &native); err != nil {
			t.Fatal(err)
		}
		native.Answers = map[string][]string{"first": {"Fresh first"}}
		asyncQuestionEvent(t, env, 2, 1, "user_input_answered", native)
	} else {
		if result := answer("telegram:333", 11, 201, "Fresh first"); result.View != "input_pending" {
			t.Fatalf("retry did not advance: %+v", result)
		}
	}
	// Retained second-answer summaries must not rewrite copies still awaiting
	// the second field when the first field is answered again.
	assertFreshEdits(map[int64]string{201: "Fresh first"})
	if source == "terminal" {
		native.Answers = map[string][]string{"second": {"Fresh second"}}
		asyncQuestionEvent(t, env, 3, 1, "user_input_answered", native)
	} else {
		if result := answer("bot", 11, 202, "Fresh second"); result.CommandID == "" {
			t.Fatalf("retry did not submit: %+v", result)
		}
	}
	assertFreshEdits(map[int64]string{201: "Fresh first", 202: "Fresh second"})
	if got := testSelectedSession(t, env.store, IncomingUpdate{BotID: "telegram:333", UserID: 10, ChatID: 20}); got != other {
		t.Fatal("retry changed the answering bot's selected session")
	}
}
