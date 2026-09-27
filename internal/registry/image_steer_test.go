package registry

import (
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestImageSteerPreparationPreservesOriginalTurn(t *testing.T) {
	for _, change := range []string{"selection", "turn-ended", "turn-replaced"} {
		t.Run(change, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			ctx := t.Context()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='original-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action = "steer"
			prepared, err := env.store.PrepareTelegramImage(ctx, in)
			if err != nil || prepared.MediaError != "" || prepared.imageTarget == nil {
				t.Fatalf("prepare image steer: %+v %v", prepared, err)
			}
			// Selection and turn events may arrive while the image downloads.
			switch change {
			case "selection":
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "other-turn")
				selection := telegramUpdate(env, 3)
				selection.Action, selection.Target = "connect", other.String()
				if result, err := env.store.AcceptTelegram(ctx, selection); err != nil || result.ErrorCode != "" {
					t.Fatalf("change selection: %+v %v", result, err)
				}
			case "turn-ended":
				_, err = env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id=NULL WHERE session_id=$1`, env.session)
			case "turn-replaced":
				_, err = env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='replacement-turn' WHERE session_id=$1`, env.session)
			}
			if err != nil {
				t.Fatal(err)
			}
			prepared.Images = []protocol.Image{registryTestImage()}
			result, err := env.store.AcceptTelegram(ctx, prepared)
			if err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			if err != nil {
				t.Fatal(err)
			}
			if change == "selection" {
				if result.CommandID == "" || len(commands) != 1 {
					t.Fatalf("image-only steer not queued: %+v, commands=%d", result, len(commands))
				}
				command := commands[0]
				if command.Operation != protocol.Steer || command.SessionID != env.session.String() || command.ExpectedTurnID != "original-turn" || command.Arguments.Text != "" || len(command.Arguments.Images) != 1 {
					t.Fatalf("image steer lost its frozen session, turn, or attachment: %+v", command)
				}
			} else if result.ErrorCode != "stale_turn" || result.CommandID != "" || len(commands) != 0 {
				t.Fatalf("image steered a different or completed turn: %+v, commands=%d", result, len(commands))
			}
			if duplicate, err := env.store.AcceptTelegram(ctx, prepared); err != nil || !duplicate.Duplicate {
				t.Fatalf("image steer retry was not deduplicated: %+v %v", duplicate, err)
			}
		})
	}
}

func TestImageSteerQuestionReplyIsRejectedBeforeDownload(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableImageInput(t, env)
	approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "What do you see?"})
	if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, 901, env.session, "", approval, "q"); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.ReplyToMessageID = "steer", "Use this screenshot", 901
	prepared, err := env.store.PrepareTelegramImage(t.Context(), in)
	if err != nil || prepared.Action != "media_error" || prepared.MediaError != "image_input_reply" || prepared.imageTarget != nil {
		t.Fatalf("question reply became image steer: %+v %v", prepared, err)
	}
	result, err := env.store.AcceptTelegram(t.Context(), prepared)
	if err != nil || result.ErrorCode != "image_input_reply" || result.CommandID != "" {
		t.Fatalf("question reply failed to report unsupported image answer: %+v %v", result, err)
	}
	var answers string
	var response *string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT input_answers,response_command_id FROM approvals WHERE approval_id=$1`, approval).Scan(&answers, &response); err != nil || answers != "{}" || response != nil {
		t.Fatalf("image steer changed the question response: answers=%s response=%v err=%v", answers, response, err)
	}
}
