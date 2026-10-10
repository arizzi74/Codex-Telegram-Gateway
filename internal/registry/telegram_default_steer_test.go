package registry

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func defaultSteerCommand(t *testing.T, env eventTestEnv, accepted AcceptResult) protocol.Command {
	t.Helper()
	commands, err := env.store.PendingCommandsForWorker(context.Background(), env.worker, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("pending commands: %#v, %v", commands, err)
	}
	if commands[0].ID != accepted.CommandID {
		t.Fatalf("pending command %q differs from accepted command %q", commands[0].ID, accepted.CommandID)
	}
	return commands[0]
}

func TestTelegramDefaultSteerText(t *testing.T) {
	for _, tc := range []struct {
		name, action, state, turn string
		operation                 protocol.Operation
	}{
		{"idle empty action", "", "idle", "", protocol.StartTurn},
		{"idle text", "text", "idle", "", protocol.StartTurn},
		{"active empty action", "", "running", "original-turn", protocol.Steer},
		{"active text", "text", "running", "original-turn", protocol.Steer},
		{"normalized text action", " TeXT ", "running", "original-turn", protocol.Steer},
		{"waiting input", "text", "waiting_input", "original-turn", protocol.Steer},
		{"waiting approval", "text", "waiting_approval", "original-turn", protocol.Steer},
		{"running without turn", "text", "running", "", protocol.StartTurn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET state=$2,active_turn_id=NULLIF($3,'') WHERE session_id=$1`, env.session, tc.state, tc.turn); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Text = tc.action, "Please include the retry case"
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" || accepted.SessionID != env.session.String() {
				t.Fatalf("accept ordinary text: %#v, %v", accepted, err)
			}
			if duplicate, err := env.store.AcceptTelegram(ctx, in); err != nil || !duplicate.Duplicate {
				t.Fatalf("duplicate ordinary text: %#v, %v", duplicate, err)
			}
			// Later turn and selection changes cannot alter an accepted command.
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='replacement-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			other := uuid.New()
			insertRouteSession(t, env, other, "other-thread", "other-turn")
			selection := telegramUpdate(env, 3)
			selection.Action, selection.Target = "connect", other.String()
			if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
				t.Fatal(err)
			}
			command := defaultSteerCommand(t, env, accepted)
			if command.Operation != tc.operation || command.ExpectedTurnID != tc.turn || command.SessionID != env.session.String() || command.ThreadID != "thread-1" || command.Arguments.Text != in.Text {
				t.Fatalf("ordinary text lost its operation, content, or frozen target: %#v", command)
			}
		})
	}
}

func TestTelegramDefaultSteerUsesRoutedSession(t *testing.T) {
	for _, route := range []string{"topic", "reply", "alias"} {
		for _, active := range []bool{false, true} {
			state := "idle"
			if active {
				state = "active"
			}
			t.Run(route+"/"+state, func(t *testing.T) {
				env := newHistoryTestEnv(t)
				ctx := context.Background()
				turn, selectedTurn := "", "selected-turn"
				operation := protocol.StartTurn
				if active {
					turn, selectedTurn, operation = "addressed-turn", "", protocol.Steer
				}
				if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id=NULLIF($2,'') WHERE session_id=$1`, env.session, selectedTurn); err != nil {
					t.Fatal(err)
				}
				addressed := uuid.New()
				insertRouteSession(t, env, addressed, "addressed-thread", turn)
				in := telegramUpdate(env, 3)
				in.Action, in.Text = "text", "Adjust this conversation"
				switch route {
				case "topic":
					selection := telegramUpdate(env, 2)
					selection.TopicID, selection.Action, selection.Target = 77, "connect", addressed.String()
					if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
						t.Fatal(err)
					}
					in.TopicID = 77
				case "reply":
					if err := env.store.RecordBotMessageRoute(ctx, "bot", 20, 99, addressed, "", uuid.Nil); err != nil {
						t.Fatal(err)
					}
					in.ReplyToMessageID = 99
				case "alias":
					aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
					if err != nil {
						t.Fatal(err)
					}
					for _, alias := range aliases {
						if alias.SessionID == addressed.String() {
							in.Target = alias.Alias
						}
					}
					if in.Target == "" {
						t.Fatal("addressed session has no alias")
					}
					in.Action = "session_alias"
				}
				accepted, err := env.store.AcceptTelegram(ctx, in)
				if err != nil || accepted.CommandID == "" || accepted.SessionID != addressed.String() {
					t.Fatalf("accept addressed text: %#v, %v", accepted, err)
				}
				command := defaultSteerCommand(t, env, accepted)
				if command.Operation != operation || command.ExpectedTurnID != turn || command.SessionID != addressed.String() || command.ThreadID != "addressed-thread" {
					t.Fatalf("ordinary text used the selected session's turn: %#v", command)
				}
				var selected string
				if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=10 AND chat_id=20 AND message_thread_id=0`).Scan(&selected); err != nil || selected != env.session.String() {
					t.Fatalf("addressed prompt changed default selection: %q, %v", selected, err)
				}
			})
		}
	}
}

func TestTelegramDefaultSteerPreservesExplicitOperations(t *testing.T) {
	for _, tc := range []struct {
		action, target, text, expected string
		operation                      protocol.Operation
	}{
		{"start_turn", "", "Start another turn", "", protocol.StartTurn},
		{"steer", "", "Adjust the current turn", "active-turn", protocol.Steer},
		{"interrupt", "", "", "active-turn", protocol.Interrupt},
		{"codex", "status", "", "", protocol.CodexCommand},
		{"history", "", "", "", protocol.ReadHistory},
	} {
		t.Run(tc.action, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET state='running',active_turn_id='active-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Target, in.Text = tc.action, tc.target, tc.text
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" {
				t.Fatalf("accept explicit operation: %#v, %v", accepted, err)
			}
			command := defaultSteerCommand(t, env, accepted)
			if command.Operation != tc.operation || command.ExpectedTurnID != tc.expected {
				t.Fatalf("explicit operation changed: %#v", command)
			}
			if tc.action == "codex" && (command.Arguments.Codex == nil || command.Arguments.Codex.Name != "status" || command.Arguments.Text != "") {
				t.Fatalf("Codex command became a prompt: %#v", command)
			}
			if tc.action == "history" && (command.Arguments.History == nil || command.Arguments.Text != "") {
				t.Fatalf("history request became a prompt: %#v", command)
			}
		})
	}
}

func TestTelegramDefaultSteerPreservesQuestionReplies(t *testing.T) {
	for _, action := range []string{"", "text", "session_alias"} {
		name := action
		if name == "" {
			name = "empty action"
		}
		t.Run(name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET state='waiting_input',active_turn_id='waiting-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			approval := insertPendingQuestion(t, env, env.session, "waiting-turn", false, protocol.Question{ID: "question", Prompt: "Which platform?"})
			in := telegramUpdate(env, 2)
			in.Action, in.Text = action, "Linux"
			if action == "session_alias" {
				aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
				if err != nil || len(aliases) != 1 {
					t.Fatalf("question session aliases: %#v, %v", aliases, err)
				}
				in.Target = aliases[0].Alias
			} else {
				if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 99, env.session, "waiting-turn", approval, "question"); err != nil {
					t.Fatal(err)
				}
				in.ReplyToMessageID = 99
			}
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || accepted.CommandID == "" {
				t.Fatalf("accept question answer: %#v, %v", accepted, err)
			}
			command := defaultSteerCommand(t, env, accepted)
			if command.Operation != protocol.InputResponse || command.Arguments.ApprovalID != approval.String() || len(command.Arguments.Answers["question"]) != 1 || command.Arguments.Answers["question"][0] != "Linux" {
				t.Fatalf("question answer became ordinary steering input: %#v", command)
			}
		})
	}
}

func TestTelegramDefaultSteerImagesFreezeTurnBeforeDownload(t *testing.T) {
	for _, tc := range []struct {
		name, action, caption, initialTurn, downloadedTurn string
		switchSelection                                    bool
		operation                                          protocol.Operation
		errorCode                                          string
	}{
		{"active caption", "text", "Explain this", "original-turn", "original-turn", false, protocol.Steer, ""},
		{"active image only", "", "", "original-turn", "original-turn", false, protocol.Steer, ""},
		{"selection changes", "text", "Explain this", "original-turn", "original-turn", true, protocol.Steer, ""},
		{"turn ends", "text", "Explain this", "original-turn", "", false, "", "stale_turn"},
		{"turn replaced", "", "", "original-turn", "replacement-turn", false, "", "stale_turn"},
		{"idle image only", "", "", "", "", false, protocol.StartTurn, ""},
		{"idle turn starts during download", "text", "Explain this", "", "replacement-turn", false, protocol.StartTurn, ""},
		{"explicit start with active turn", "start_turn", "Explain this", "original-turn", "replacement-turn", false, protocol.StartTurn, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			enableImageInput(t, env)
			ctx := context.Background()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id=NULLIF($2,'') WHERE session_id=$1`, env.session, tc.initialTurn); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Text = tc.action, tc.caption
			prepared, err := env.store.PrepareTelegramImage(ctx, in)
			if err != nil || prepared.imageTarget == nil || prepared.imageTarget.sessionID != env.session || prepared.imageTarget.activeTurnID != tc.initialTurn {
				t.Fatalf("prepare image route: %#v, %v", prepared.imageTarget, err)
			}
			// Simulate a turn event or selection update arriving during download.
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id=NULLIF($2,'') WHERE session_id=$1`, env.session, tc.downloadedTurn); err != nil {
				t.Fatal(err)
			}
			if tc.switchSelection {
				other := uuid.New()
				insertRouteSession(t, env, other, "other-thread", "other-turn")
				selection := telegramUpdate(env, 3)
				selection.Action, selection.Target = "connect", other.String()
				if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
					t.Fatal(err)
				}
			}
			prepared.Images = []protocol.Image{registryTestImage()}
			accepted, err := env.store.AcceptTelegram(ctx, prepared)
			if err != nil || accepted.ErrorCode != tc.errorCode {
				t.Fatalf("accept downloaded image: %#v, %v", accepted, err)
			}
			if tc.errorCode != "" {
				if accepted.View != "error" || accepted.CommandID != "" {
					t.Fatalf("stale image created a command: %#v", accepted)
				}
				commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
				if err != nil || len(commands) != 0 {
					t.Fatalf("stale image remained pending: %#v, %v", commands, err)
				}
				if duplicate, err := env.store.AcceptTelegram(ctx, prepared); err != nil || !duplicate.Duplicate {
					t.Fatalf("stale image deduplication: %#v, %v", duplicate, err)
				}
				return
			}
			if accepted.CommandID == "" || accepted.SessionID != env.session.String() {
				t.Fatalf("downloaded image lost its session: %#v", accepted)
			}
			command := defaultSteerCommand(t, env, accepted)
			expected := ""
			if tc.operation == protocol.Steer {
				expected = tc.initialTurn
			}
			if command.Operation != tc.operation || command.ExpectedTurnID != expected || command.SessionID != env.session.String() || command.ThreadID != "thread-1" || command.Arguments.Text != tc.caption || len(command.Arguments.Images) != 1 || !bytes.Equal(command.Arguments.Images[0].Data, prepared.Images[0].Data) {
				t.Fatalf("downloaded image lost its operation, content, or frozen turn: %#v", command)
			}
		})
	}
}
