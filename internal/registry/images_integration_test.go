package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func registryTestImage() protocol.Image {
	return protocol.Image{MIMEType: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}
}

func enableImageInput(t *testing.T, env eventTestEnv) {
	t.Helper()
	if err := env.store.RecordHeartbeat(context.Background(), Heartbeat{WorkerID: env.worker, ConnectionID: env.connection,
		Metadata: json.RawMessage(`{"supports_image_input":true}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestImageCommandsAreDurableAndKeepFrozenTarget(t *testing.T) {
	for _, action := range []string{"text", "steer"} {
		t.Run(action, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, "UPDATE sessions SET active_turn_id='active-turn' WHERE session_id=$1", env.session); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Images = action, []protocol.Image{registryTestImage()}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || result.CommandID == "" {
				t.Fatalf("accept image-only input: %#v %v", result, err)
			}
			duplicate, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || !duplicate.Duplicate {
				t.Fatalf("duplicate image: %#v %v", duplicate, err)
			}
			other := uuid.New()
			insertRouteSession(t, env, other, "other-thread", "")
			change := telegramUpdate(env, 3)
			change.Action, change.Target = "connect", other.String()
			if _, err := env.store.AcceptTelegram(ctx, change); err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			if err != nil || len(commands) != 1 {
				t.Fatalf("pending image commands=%d: %v", len(commands), err)
			}
			command := commands[0]
			if command.ID != result.CommandID || command.SessionID != env.session.String() || command.ThreadID != "thread-1" || command.Arguments.Text != "" || len(command.Arguments.Images) != 1 || !bytes.Equal(command.Arguments.Images[0].Data, in.Images[0].Data) {
				t.Fatal("persisted image command lost content or its immutable target")
			}
			if action == "steer" && (command.Operation != protocol.Steer || command.ExpectedTurnID != "active-turn") {
				t.Fatal("image steer lost active-turn correlation")
			}
		})
	}
}

func TestImageRequiresWorkerCapabilityBeforeCaptionCanRun(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.Images = "text", "explain this", []protocol.Image{registryTestImage()}
	prepared, err := env.store.PrepareTelegramImage(ctx, in)
	if err != nil || prepared.Action != "media_error" || prepared.MediaError != "image_worker_upgrade" || len(prepared.Images) != 0 {
		t.Fatalf("legacy image preparation: action=%s code=%s err=%v", prepared.Action, prepared.MediaError, err)
	}
	result, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || result.ErrorCode != "image_worker_upgrade" || result.CommandID != "" {
		t.Fatalf("legacy worker image result: %#v %v", result, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 0 {
		t.Fatal("legacy worker received caption-only command")
	}
	enableImageInput(t, env)
	in.UpdateID++
	result, err = env.store.AcceptTelegram(ctx, in)
	if err != nil || result.CommandID == "" {
		t.Fatalf("updated worker image result: %#v %v", result, err)
	}
}

func TestMediaErrorsAreDurableAllowlistedResponses(t *testing.T) {
	for _, code := range []string{"image_too_large", "image_download_failed", "image_unsupported", "image_worker_upgrade", "https://example.test/private-token"} {
		t.Run(code, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			in := telegramUpdate(env, 1)
			in.Action, in.MediaError = "media_error", code
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || result.View != "error" || result.CommandID != "" {
				t.Fatalf("media error result: %#v %v", result, err)
			}
			want := code
			if strings.HasPrefix(code, "https:") {
				want = "image_unsupported"
			}
			if result.ErrorCode != want {
				t.Fatalf("media error=%s, want %s", result.ErrorCode, want)
			}
			if duplicate, err := env.store.AcceptTelegram(ctx, in); err != nil || !duplicate.Duplicate {
				t.Fatalf("media error deduplication: %#v %v", duplicate, err)
			}
			var payload string
			if err := env.store.pool.QueryRow(ctx, "SELECT payload FROM telegram_deliveries").Scan(&payload); err != nil || strings.Contains(payload, "private-token") {
				t.Fatalf("unsafe media error persistence: %v", err)
			}
		})
	}
}

func TestImageCannotSilentlyBecomeAnApprovalAnswer(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableImageInput(t, env)
	ctx := context.Background()
	approvalID := uuid.New()
	payload, err := json.Marshal(protocol.Approval{ID: approvalID.String(), RequestID: "input-request", ThreadID: "thread-1", Type: "input", Questions: []protocol.Question{{ID: "question", Prompt: "Answer"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO approvals
        (approval_id,worker_id,runtime_id,runtime_generation,session_id,codex_request_id,
         codex_thread_id,approval_type,request_payload,state,requested_at)
        VALUES ($1,$2,$3,1,$4,'input-request','thread-1','input',$5,'pending',(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z'))`,
		approvalID, env.worker, env.runtime, env.session, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 901, env.session, "", approvalID, "question"); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.ReplyToMessageID, in.Images = "text", "caption is not an answer", 901, []protocol.Image{registryTestImage()}
	prepared, err := env.store.PrepareTelegramImage(ctx, in)
	if err != nil || prepared.Action != "media_error" || prepared.MediaError != "image_input_reply" || len(prepared.Images) != 0 {
		t.Fatalf("approval reply should not download: action=%s code=%s err=%v", prepared.Action, prepared.MediaError, err)
	}
	result, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || result.ErrorCode != "image_input_reply" || result.CommandID != "" {
		t.Fatalf("image approval reply: %#v %v", result, err)
	}
	var answer, response *string
	if err := env.store.pool.QueryRow(ctx, "SELECT input_answers,response_command_id FROM approvals WHERE approval_id=$1", approvalID).Scan(&answer, &response); err != nil || response != nil || (answer != nil && *answer != "{}") {
		t.Fatalf("image reply consumed approval: %v", err)
	}
}

func TestImagePreparationFreezesSelectionBeforeDownload(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableImageInput(t, env)
	ctx := context.Background()
	in := telegramUpdate(env, 2)
	in.Action, in.Text = "text", "explain this"
	prepared, err := env.store.PrepareTelegramImage(ctx, in)
	if err != nil || prepared.imageTarget == nil {
		t.Fatalf("image target preparation: %v", err)
	}
	// A Telegram selection change arrives while the file is being downloaded.
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	change := telegramUpdate(env, 3)
	change.Action, change.Target = "connect", other.String()
	if _, err := env.store.AcceptTelegram(ctx, change); err != nil {
		t.Fatal(err)
	}
	prepared.Images = []protocol.Image{registryTestImage()}
	result, err := env.store.AcceptTelegram(ctx, prepared)
	if err != nil || result.SessionID != env.session.String() || result.CommandID == "" {
		t.Fatalf("image rerouted after download: %#v %v", result, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].ThreadID != "thread-1" || len(commands[0].Arguments.Images) != 1 {
		t.Fatalf("prepared image target not preserved: %v", err)
	}
}

func TestImagePreparationRejectsChangedRuntimeAndExpiredSession(t *testing.T) {
	for _, change := range []string{"generation", "archived", "thread"} {
		t.Run(change, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			ctx := context.Background()
			in := telegramUpdate(env, 2)
			in.Action = "text"
			prepared, err := env.store.PrepareTelegramImage(ctx, in)
			if err != nil || prepared.imageTarget == nil {
				t.Fatalf("prepare image: %v", err)
			}
			switch change {
			case "generation":
				_, err = env.store.pool.Exec(ctx, "UPDATE runtimes SET generation=2 WHERE runtime_id=$1", env.runtime)
			case "archived":
				_, err = env.store.pool.Exec(ctx, "UPDATE sessions SET archived=TRUE WHERE session_id=$1", env.session)
			case "thread":
				_, err = env.store.pool.Exec(ctx, "UPDATE sessions SET codex_thread_id='replacement' WHERE session_id=$1", env.session)
			}
			if err != nil {
				t.Fatal(err)
			}
			prepared.Images = []protocol.Image{registryTestImage()}
			result, err := env.store.AcceptTelegram(ctx, prepared)
			if err != nil || result.ErrorCode != "target_unavailable" || result.CommandID != "" {
				t.Fatalf("changed image target accepted: %#v %v", result, err)
			}
		})
	}
}

func TestImagePreparationWithoutSelectionReturnsDurableError(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	in := telegramUpdate(env, 1)
	in.Action = "text"
	prepared, err := env.store.PrepareTelegramImage(ctx, in)
	if err != nil || prepared.Action != "media_error" || prepared.MediaError != "target_unavailable" {
		t.Fatalf("missing image destination: action=%s code=%s err=%v", prepared.Action, prepared.MediaError, err)
	}
	result, err := env.store.AcceptTelegram(ctx, prepared)
	if err != nil || result.ErrorCode != "target_unavailable" || result.CommandID != "" {
		t.Fatalf("missing destination not reported: %#v %v", result, err)
	}
}

func TestImageRejectedForUnsupportedActionOrInvalidContent(t *testing.T) {
	for _, tc := range []struct {
		name, action, code string
		images             []protocol.Image
	}{
		{"slash command", "codex", "image_unsupported", []protocol.Image{registryTestImage()}},
		{"malformed", "text", "image_unsupported", []protocol.Image{{MIMEType: "image/png", Data: []byte("bad")}}},
		{"oversized", "text", "image_too_large", []protocol.Image{{MIMEType: "image/png", Data: make([]byte, protocol.MaxImageBytes+1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			in := telegramUpdate(env, 2)
			in.Action, in.Text, in.Images = tc.action, "caption", tc.images
			result, err := env.store.AcceptTelegram(context.Background(), in)
			if err != nil || result.ErrorCode != tc.code || result.CommandID != "" {
				t.Fatalf("unsupported image result: %#v %v", result, err)
			}
		})
	}
}
