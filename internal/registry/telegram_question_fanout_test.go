package registry

import (
	"encoding/json"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestTelegramPartialQuestionAnswerFanoutRespectsBotDestinations(t *testing.T) {
	for _, tc := range []struct {
		name, answer, next string
		fanout             bool
	}{{"advance current question", "first", "second", true}, {"answer later question", "third", "first", false}} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			approval := insertPendingQuestion(t, env, env.session, "", true,
				protocol.Question{ID: "first", Prompt: "First?"},
				protocol.Question{ID: "second", Prompt: "Second?"},
				protocol.Question{ID: "third", Prompt: "Third?"})
			for i, bot := range []string{"bot", "allowed", "disabled", "disallowed"} {
				record := telegramBotRecordForTest(bot, bot != "disabled")
				record.TelegramID = int64(100 + i)
				if err := env.store.SaveTelegramBotRecord(ctx, record); err != nil {
					t.Fatal(err)
				}
				if bot == "bot" {
					continue
				}
				// Duplicate remembered users and a binding for the same bot/chat
				// must still produce only one followup at that destination.
				for _, user := range []int64{10, 11} {
					if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id) VALUES($1,$2,20,0)`, bot, user); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings(bot_id,user_id,chat_id,message_thread_id,session_id) VALUES($1,10,20,0,$2)`, bot, env.session); err != nil {
					t.Fatal(err)
				}
			}
			if err := env.store.ApplyTelegramChatAllowlist(ctx, "disallowed", []int64{30}); err != nil {
				t.Fatal(err)
			}
			answer := telegramUpdate(env, 2)
			answer.Action, answer.Target, answer.QuestionID, answer.Text = "input", approval.String(), tc.answer, "Answer"
			result, err := env.store.AcceptTelegram(ctx, answer)
			if err != nil || result.View != "input_pending" || result.QuestionID != tc.next {
				t.Fatalf("partial answer: %+v %v", result, err)
			}
			rows, err := env.store.pool.Query(ctx, `SELECT bot_id,payload FROM telegram_deliveries WHERE kind='ui_response' AND json_extract(payload,'$.view')='input_pending'`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			counts := make(map[string]int)
			for rows.Next() {
				var bot string
				var raw []byte
				if err := rows.Scan(&bot, &raw); err != nil {
					t.Fatal(err)
				}
				var response AcceptResult
				if err := json.Unmarshal(raw, &response); err != nil {
					t.Fatal(err)
				}
				if response.ApprovalID != approval.String() || response.QuestionID != tc.next {
					t.Fatalf("wrong followup for %s: %+v", bot, response)
				}
				if (bot == "bot" && response.UserID != 10) || (bot != "bot" && response.UserID != 0) {
					t.Fatalf("wrong followup actor for %s: %+v", bot, response)
				}
				counts[bot]++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			wantAllowed := 0
			if tc.fanout {
				wantAllowed = 1
			}
			if counts["bot"] != 1 || counts["allowed"] != wantAllowed || counts["disabled"] != 0 || counts["disallowed"] != 0 || len(counts) != 1+wantAllowed {
				t.Fatalf("unexpected followup destinations: %+v", counts)
			}
		})
	}
}
