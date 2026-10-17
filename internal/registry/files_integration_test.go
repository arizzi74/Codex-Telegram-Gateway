package registry

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func enableFileInput(t *testing.T, env eventTestEnv) {
	t.Helper()
	if err := env.store.RecordHeartbeat(t.Context(), Heartbeat{WorkerID: env.worker, ConnectionID: env.connection,
		Metadata: json.RawMessage(`{"supports_file_input":true,"supports_image_input":true}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramFilesSaveOnlyAndCaptionCommands(t *testing.T) {
	for _, tc := range []struct {
		name, action, caption, active, state string
		operation                            protocol.Operation
	}{
		{"cold file", "text", "", "", "idle", protocol.StoreFiles},
		{"busy file", "text", "", "active", "running", protocol.StoreFiles},
		{"cold caption", "text", "inspect file", "", "idle", protocol.StartTurn},
		{"active caption", "text", "/model is literal", "active", "running", protocol.Steer},
		{"explicit steer", "steer", "", "active", "running", protocol.Steer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableFileInput(t, env)
			ctx := t.Context()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id=NULLIF($1,''),state=$2,loaded=0 WHERE session_id=$3`, tc.active, tc.state, env.session); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Text = tc.action, tc.caption
			prepared, err := env.store.PrepareTelegramFile(ctx, in)
			if err != nil || prepared.MediaError != "" {
				t.Fatalf("prepare: %#v %v", prepared, err)
			}
			prepared.Files = []protocol.File{{Name: "report.pdf", Data: []byte("%PDF-document\x00bytes")}}
			result, err := env.store.AcceptTelegram(ctx, prepared)
			if err != nil || result.CommandID == "" {
				t.Fatalf("accept: %#v %v", result, err)
			}
			if result, err := env.store.AcceptTelegram(ctx, prepared); err != nil || !result.Duplicate {
				t.Fatalf("dedupe: %#v %v", result, err)
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			if err != nil || len(commands) != 1 {
				t.Fatalf("commands: %d %v", len(commands), err)
			}
			command := commands[0]
			expected := ""
			if tc.operation == protocol.Steer {
				expected = tc.active
			}
			if command.Operation != tc.operation || command.Arguments.Text != tc.caption || command.ExpectedTurnID != expected || command.Arguments.CWD != "/work" || len(command.Arguments.Files) != 1 || !bytes.Equal(command.Arguments.Files[0].Data, prepared.Files[0].Data) {
				t.Fatalf("unexpected durable file command: %#v", command)
			}
			var queuedResponses int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE kind='ui_response' AND json_extract(payload,'$.command_id')=$1`, command.ID).Scan(&queuedResponses); err != nil {
				t.Fatal(err)
			}
			wantQueued := 0
			if tc.operation == protocol.Steer {
				wantQueued = 1
			}
			if queuedResponses != wantQueued {
				t.Fatalf("queued confirmations=%d, want %d for %s", queuedResponses, wantQueued, tc.operation)
			}
		})
	}
}

func TestTelegramFileDownloadKeepsDestinationAndCWD(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableFileInput(t, env)
	in := telegramUpdate(env, 2)
	in.Action = "text"
	prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
	if err != nil || prepared.fileTarget == nil {
		t.Fatalf("prepare: %v", err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	change := telegramUpdate(env, 3)
	change.Action, change.Target = "connect", other.String()
	if _, err := env.store.AcceptTelegram(t.Context(), change); err != nil {
		t.Fatal(err)
	}
	// A later turn cannot retarget a file-only upload.
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET active_turn_id='later' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	prepared.Files = []protocol.File{{Name: "data.csv", Data: []byte("a,b")}}
	result, err := env.store.AcceptTelegram(t.Context(), prepared)
	if err != nil || result.SessionID != env.session.String() || result.CommandID == "" {
		t.Fatalf("accept: %#v %v", result, err)
	}
	commands, err := env.store.PendingCommandsForWorker(t.Context(), env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Arguments.CWD != "/work" || commands[0].Operation != protocol.StoreFiles || commands[0].ThreadID != "thread-1" {
		t.Fatalf("frozen command: %#v %v", commands, err)
	}
}

func TestTelegramFileRejectsChangedWorkspaceAfterDownload(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableFileInput(t, env)
	in := telegramUpdate(env, 2)
	in.Action = "text"
	prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
	if err != nil || prepared.fileTarget == nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET cwd='/changed' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	prepared.Files = []protocol.File{{Name: "data.csv", Data: []byte("a,b")}}
	result, err := env.store.AcceptTelegram(t.Context(), prepared)
	if err != nil || result.ErrorCode != "target_unavailable" || result.CommandID != "" {
		t.Fatalf("changed workspace accepted: %#v %v", result, err)
	}
}

func TestTelegramFileCaptionRejectsChangedActiveTurn(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableFileInput(t, env)
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET active_turn_id='original' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Text = "text", "inspect this"
	prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET active_turn_id='replacement' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	prepared.Files = []protocol.File{{Name: "data", Data: []byte("x")}}
	result, err := env.store.AcceptTelegram(t.Context(), prepared)
	if err != nil || result.ErrorCode != "stale_turn" || result.CommandID != "" {
		t.Fatalf("changed turn accepted: %#v %v", result, err)
	}
}

func TestTelegramDocumentCapabilityClassifiesBeforeQueue(t *testing.T) {
	for _, image := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic file", true: "image document"}[image], func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			in := telegramUpdate(env, 2)
			in.Action = "text"
			prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
			if err != nil || prepared.MediaError != "" {
				t.Fatalf("image-capable preflight: %#v %v", prepared, err)
			}
			if image {
				prepared.Images = []protocol.Image{registryTestImage()}
			} else {
				prepared.Files = []protocol.File{{Name: "doc", Data: []byte("contents")}}
			}
			result, err := env.store.AcceptTelegram(t.Context(), prepared)
			if err != nil {
				t.Fatal(err)
			}
			if image && result.CommandID == "" {
				t.Fatalf("image document stopped working: %#v", result)
			}
			if !image && (result.ErrorCode != "file_worker_upgrade" || result.CommandID != "") {
				t.Fatalf("file reached old worker: %#v", result)
			}
		})
	}
	env := newHistoryTestEnv(t)
	in := telegramUpdate(env, 2)
	prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
	if err != nil || prepared.MediaError != "file_worker_upgrade" {
		t.Fatalf("old worker preflight: %#v %v", prepared, err)
	}
}

func TestTelegramFileBypassesQuestionAndWizardText(t *testing.T) {
	env := newHistoryTestEnv(t)
	enableFileInput(t, env)
	approvalID := uuid.New()
	payload, _ := json.Marshal(protocol.Approval{ID: approvalID.String(), RequestID: "request", ThreadID: "thread-1", Type: "input", Questions: []protocol.Question{{ID: "q", Prompt: "Answer"}}})
	if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO approvals (approval_id,worker_id,runtime_id,runtime_generation,session_id,codex_request_id,codex_thread_id,approval_type,request_payload,state,requested_at) VALUES($1,$2,$3,1,$4,'request','thread-1','input',$5,'pending',`+sqliteNow+`)`, approvalID, env.worker, env.runtime, env.session, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, 901, env.session, "", approvalID, "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(t.Context(), `INSERT INTO telegram_session_wizards(wizard_id,bot_id,user_id,chat_id,revision,kind,phase,expires_at) VALUES($1,'bot',10,20,1,'new','name',$2)`, uuid.New(), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.ReplyToMessageID = "text", "inspect the attachment", 901
	prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
	if err != nil || prepared.MediaError != "" {
		t.Fatalf("prepare: %#v %v", prepared, err)
	}
	prepared.Files = []protocol.File{{Name: "data.txt", Data: []byte("file")}}
	result, err := env.store.AcceptTelegram(t.Context(), prepared)
	if err != nil || result.CommandID == "" {
		t.Fatalf("payload consumed: %#v %v", result, err)
	}
	var answer, response *string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT input_answers,response_command_id FROM approvals WHERE approval_id=$1`, approvalID).Scan(&answer, &response); err != nil || response != nil || (answer != nil && *answer != "{}") {
		t.Fatalf("file answered question: %v", err)
	}
	var name string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT session_name FROM telegram_session_wizards`).Scan(&name); err != nil || name != "" {
		t.Fatalf("caption answered wizard: %q %v", name, err)
	}
}

func TestTelegramFileAuthorizationIsCheckedBeforeAndAfterDownload(t *testing.T) {
	for _, change := range []string{"owner", "chat", "disabled"} {
		t.Run(change, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableFileInput(t, env)
			bot := telegramBotRecordForTest("bot", true)
			if err := env.store.SaveTelegramBotRecord(t.Context(), bot); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action = "text"
			prepared, err := env.store.PrepareTelegramFile(t.Context(), in)
			if err != nil || prepared.MediaError != "" {
				t.Fatalf("prepare: %#v %v", prepared, err)
			}
			switch change {
			case "owner":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_bots SET owner_id=99 WHERE bot_id='bot'`)
			case "chat":
				err = env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{99})
			case "disabled":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_bots SET enabled=0 WHERE bot_id='bot'`)
			}
			if err != nil {
				t.Fatal(err)
			}
			preflight, err := env.store.PrepareTelegramFile(t.Context(), in)
			if err != nil || preflight.MediaError != "file_unauthorized" {
				t.Fatalf("unauthorized preflight: %#v %v", preflight, err)
			}
			prepared.Files = []protocol.File{{Name: "file", Data: []byte("x")}}
			result, err := env.store.AcceptTelegram(t.Context(), prepared)
			if change == "disabled" {
				if err != ErrTelegramBotDisabled {
					t.Fatalf("disabled result: %#v %v", result, err)
				}
			} else if err != nil || result.ErrorCode != "file_unauthorized" || result.CommandID != "" {
				t.Fatalf("authorization race: %#v %v", result, err)
			}
		})
	}
}
