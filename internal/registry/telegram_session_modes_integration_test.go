package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func acceptModeUpdate(t *testing.T, env eventTestEnv, id int64, action, target, text string) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, id)
	in.Action, in.Target, in.Text = action, target, text
	result, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil || result.ErrorCode != "" {
		t.Fatalf("accept %s: %+v %v", action, result, err)
	}
	return result
}
func sessionModeDeliveries(t *testing.T, store *Store) []Delivery {
	t.Helper()
	all, err := store.ClaimDeliveries(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var result []Delivery
	for _, d := range all {
		if d.Kind == "ui_response" {
			if err := store.MarkDeliverySent(context.Background(), d.ID, 901, "", "", ""); err != nil {
				t.Fatal(err)
			}
		} else {
			result = append(result, d)
		}
	}
	return result
}
func TestTelegramSessionSwitchRestoresProgressAndSuppressesOldFinalIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := env
	other.session = uuid.New()
	insertRouteSession(t, env, other.session, "thread-b", "")
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, other, 3, "turn_started", "turn-b", "")
	progressEvent(t, env, 4, "agent_progress_message", "turn-a", "")
	progressEvent(t, env, 5, "tool_progress_message", "turn-a", "")
	for i, d := range sessionModeDeliveries(t, env.store) {
		checkpointProgress(t, env.store, d, int64(100+i))
	}
	progressEvent(t, other, 6, "agent_progress_message", "turn-b", "")
	progressEvent(t, other, 7, "tool_progress_message", "turn-b", "")
	if got := sessionModeDeliveries(t, env.store); len(got) != 0 {
		t.Fatalf("hidden progress delivered: %+v", got)
	}
	acceptModeUpdate(t, env, 2, "select", other.session.String(), "")
	restored := sessionModeDeliveries(t, env.store)
	if len(restored) != 2 {
		t.Fatalf("restored %d messages, want2", len(restored))
	}
	for _, d := range restored {
		var event protocol.Event
		if err := json.Unmarshal(d.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.SessionID != other.session.String() {
			t.Fatalf("wrong restored session: %+v", event)
		}
		if suppress, err := env.store.SuppressTelegramDelivery(ctx, d.ID); err != nil || suppress {
			t.Fatalf("restoration suppressed %v %v", suppress, err)
		}
	}
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 2 {
		t.Fatalf("old progress cleanup %v %v", due, err)
	}
	progressEvent(t, env, 8, "turn_completed", "turn-a", "")
	if got := sessionModeDeliveries(t, env.store); len(got) != 0 {
		t.Fatalf("hidden final delivered: %+v", got)
	}
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20})
	acceptModeUpdate(t, env, 3, "disconnect", "", "")
	assertTypingTargets(t, env.store)
}
func TestTelegramQueuedFinalAndInFlightProgressAreFencedOnSwitchIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-b", "")
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	progressEvent(t, env, 2, "agent_progress_message", "turn-a", "")
	live := sessionModeDeliveries(t, env.store)[0]
	if _, err := env.store.PrepareDeliveryChunks(ctx, live.ID, []json.RawMessage{json.RawMessage(`{"text":"inflight"}`)}); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 3, "turn_completed", "turn-a", "")
	final := sessionModeDeliveries(t, env.store)[0]
	acceptModeUpdate(t, env, 2, "select", other.String(), "")
	if suppress, err := env.store.SuppressTelegramDelivery(ctx, final.ID); err != nil || !suppress {
		t.Fatalf("old final escaped: %v %v", suppress, err)
	}
	// A final whose network send raced the switch is also retired.
	if err := env.store.MarkDeliverySent(ctx, final.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// A Telegram send already underway still checkpoints, and is then deleted.
	if err := env.store.MarkDeliveryChunkSent(ctx, live.ID, 0, 99, "", "", ""); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 3, "select", env.session.String(), "")
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 2 {
		t.Fatalf("late progress/final survived reselect %v %v", due, err)
	}
}
func TestTelegramMultiSessionToggleAndStableAliasesIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := env
	other.session = uuid.New()
	insertRouteSession(t, env, other.session, "thread-b", "")
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET name='A shared / title'`); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	result := acceptModeUpdate(t, env, 2, "multisession", "", "on")
	if !result.MultiSession {
		t.Fatal("mode disabled")
	}
	aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
	if err != nil || len(aliases) != 2 || aliases[0].Alias == aliases[1].Alias {
		t.Fatalf("aliases %+v %v", aliases, err)
	}
	alias := ""
	for _, item := range aliases {
		if len(item.Alias) > 32 || !strings.HasPrefix(item.Alias, "_") {
			t.Fatalf("invalid alias %+v", item)
		}
		if item.SessionID == other.session.String() {
			alias = item.Alias
		}
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET name='Renamed session' WHERE session_id=$1`, other.session); err != nil {
		t.Fatal(err)
	}
	again, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range again {
		if item.SessionID == other.session.String() && item.Alias != alias {
			t.Fatal("rename changed alias")
		}
	}
	accepted := acceptModeUpdate(t, env, 3, "session_alias", alias, "targeted prompt")
	if accepted.SessionID != other.session.String() {
		t.Fatalf("alias routed %+v", accepted)
	}
	var binding string
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings`).Scan(&binding); err != nil || binding != env.session.String() {
		t.Fatalf("addressed prompt changed binding: %s %v", binding, err)
	}
	progressEvent(t, other, 2, "turn_started", "turn-b", "")
	progressEvent(t, other, 3, "agent_progress_message", "turn-b", "")
	got := sessionModeDeliveries(t, env.store)
	if len(got) != 1 {
		t.Fatalf("multi progress %+v", got)
	}
	checkpointProgress(t, env.store, got[0], 100)
	presentation, err := env.store.TelegramSessionPresentation(ctx, "bot", 20, 0, other.session.String())
	if err != nil || !presentation.MultiSession || presentation.Name != "Renamed session" || presentation.Marker == "" {
		t.Fatalf("presentation %+v %v", presentation, err)
	}
	acceptModeUpdate(t, env, 4, "multisession", "", "off")
	due, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(due) != 1 {
		t.Fatalf("mode off cleanup %+v %v", due, err)
	}
	assertTypingTargets(t, env.store)
	acceptModeUpdate(t, env, 5, "session_alias", alias, "")
	assertTypingTargets(t, env.store, TelegramTypingTarget{BotID: "bot", ChatID: 20})
}
func TestTelegramCrossSessionQuestionRepliesPreserveSelectionIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-b", "")
	acceptModeUpdate(t, env, 1, "select", other.String(), "")
	progressEvent(t, env, 2, "turn_started", "turn-question", "")
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "ask", ThreadID: "thread-1", TurnID: "turn-question", Type: "user_input", Questions: []protocol.Question{{ID: "q", Prompt: "Which option?"}}}
	data, _ := json.Marshal(approval)
	event := protocol.Event{ID: uuid.NewString(), Seq: 3, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "user_input_requested", OccurredAt: time.Now().UTC(), Data: data}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	got := sessionModeDeliveries(t, env.store)
	if len(got) != 1 || got[0].Kind != "user_input_requested" {
		t.Fatalf("question not delivered: %+v", got)
	}
	if suppress, err := env.store.SuppressTelegramDelivery(ctx, got[0].ID); err != nil || suppress {
		t.Fatalf("question suppressed %v %v", suppress, err)
	}
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 77, env.session, "turn-question", uuid.MustParse(approval.ID), "q"); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 2, "new", env.runtime.String(), "")
	reply := telegramUpdate(env, 3)
	reply.ReplyToMessageID, reply.Text = 77, "use option one"
	accepted, err := env.store.AcceptTelegram(ctx, reply)
	if err != nil || accepted.SessionID != env.session.String() {
		t.Fatalf("question reply %+v %v", accepted, err)
	}
	var binding string
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings`).Scan(&binding); err != nil || binding != other.String() {
		t.Fatalf("question changed binding %s %v", binding, err)
	}
}

func TestTelegramQuestionsReachDisconnectedKnownChatIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	acceptModeUpdate(t, env, 2, "disconnect", "", "")
	progressEvent(t, env, 2, "turn_started", "turn-question", "")
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "ask", ThreadID: "thread-1", TurnID: "turn-question", Type: "user_input", Questions: []protocol.Question{{ID: "q", Prompt: "Which option?"}}}
	data, _ := json.Marshal(approval)
	event := protocol.Event{ID: uuid.NewString(), Seq: 3, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "user_input_requested", OccurredAt: time.Now().UTC(), Data: data}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	got := sessionModeDeliveries(t, env.store)
	if len(got) != 1 || got[0].Kind != "user_input_requested" {
		t.Fatalf("question not delivered: %+v", got)
	}
	if suppress, err := env.store.SuppressTelegramDelivery(ctx, got[0].ID); err != nil || suppress {
		t.Fatalf("disconnected question suppressed %v %v", suppress, err)
	}
}

func TestTelegramNativeUserMessagesOnlyReachMultiSessionChatsIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	progressEvent(t, env, 2, "user_message", "turn-user", "")
	if got := sessionModeDeliveries(t, env.store); len(got) != 0 {
		t.Fatalf("single mode echoed prompt: %+v", got)
	}
	acceptModeUpdate(t, env, 2, "multisession", "", "on")
	progressEvent(t, env, 3, "user_message", "turn-user", "")
	got := sessionModeDeliveries(t, env.store)
	if len(got) != 1 || got[0].Kind != "user_message" {
		t.Fatalf("multi mode missed prompt: %+v", got)
	}
	acceptModeUpdate(t, env, 3, "multisession", "", "off")
	if suppress, err := env.store.SuppressTelegramDelivery(ctx, got[0].ID); err != nil || !suppress {
		t.Fatalf("prompt queued before toggle escaped %v %v", suppress, err)
	}
}

func TestTelegramAliasAnswersOnlyUnambiguousQuestionsIntegration(t *testing.T) {
	for _, questions := range []int{1, 2} {
		t.Run(string(rune('0'+questions)), func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			other := uuid.New()
			insertRouteSession(t, env, other, "thread-b", "")
			acceptModeUpdate(t, env, 1, "select", other.String(), "")
			progressEvent(t, env, 2, "turn_started", "turn-question", "")
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "ask", ThreadID: "thread-1", TurnID: "turn-question", Type: "user_input"}
			for i := 0; i < questions; i++ {
				approval.Questions = append(approval.Questions, protocol.Question{ID: string(rune('a' + i)), Prompt: "Choose a response"})
			}
			data, _ := json.Marshal(approval)
			event := protocol.Event{ID: uuid.NewString(), Seq: 3, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "user_input_requested", OccurredAt: time.Now().UTC(), Data: data}
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
				t.Fatal(err)
			}
			aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
			if err != nil {
				t.Fatal(err)
			}
			alias := ""
			for _, item := range aliases {
				if item.SessionID == env.session.String() {
					alias = item.Alias
				}
			}
			in := telegramUpdate(env, 2)
			in.Action, in.Target, in.Text = "session_alias", alias, "my answer"
			accepted, err := env.store.AcceptTelegram(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			commands, err := env.store.PendingCommands(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			if questions == 1 {
				if accepted.ErrorCode != "" || len(commands) != 1 || commands[0].Operation != protocol.InputResponse || commands[0].SessionID != env.session.String() {
					t.Fatalf("alias answer failed: %+v %+v", accepted, commands)
				}
			} else if accepted.ErrorCode != "session_answer_ambiguous" || len(commands) != 0 {
				t.Fatalf("ambiguous answer queued turn: %+v %+v", accepted, commands)
			}
			var binding string
			if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings`).Scan(&binding); err != nil || binding != other.String() {
				t.Fatalf("alias answer changed binding %s %v", binding, err)
			}
		})
	}
}

func TestTelegramSessionLabelsMatchSessionPickerFallbackIntegration(t *testing.T) {
	cases := []struct{ name, preview, cwd, want string }{
		{" Named session ", "preview", "/work/project", "Named session"},
		{"   ", " A complete preview that remains readable without truncation ", "/work/project", "A complete preview that remains readable without truncation"},
		{"", "  ", "/work/Folder Name", "Folder Name"},
		{"", "", "/", "Session"},
	}
	for _, test := range cases {
		t.Run(test.want, func(t *testing.T) {
			env := newEventTestEnv(t)
			ctx := context.Background()
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET name=$1,preview=$2,cwd=$3 WHERE session_id=$4`, test.name, test.preview, test.cwd, env.session); err != nil {
				t.Fatal(err)
			}
			acceptModeUpdate(t, env, 1, "multisession", "", "on")
			aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
			if err != nil || len(aliases) != 1 || aliases[0].Name != test.want || aliases[0].Alias != "_"+sessionAliasBase(test.want) {
				t.Fatalf("picker/alias name mismatch %+v %v", aliases, err)
			}
			presentation, err := env.store.TelegramSessionPresentation(ctx, "bot", 20, 0, env.session.String())
			if err != nil || presentation.Name != test.want {
				t.Fatalf("picker/presentation mismatch %+v %v", presentation, err)
			}
		})
	}
}

func TestTelegramArchivedHelpersStayHiddenAndRetireQueuedProgressIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	helper := env
	helper.session = uuid.New()
	insertRouteSession(t, env, helper.session, "hidden-helper", "")
	// Worker reconciliation archives old subagent identities; newly discovered
	// subagents are filtered before they ever enter the registry inventory.
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=TRUE WHERE session_id=$1`, helper.session); err != nil {
		t.Fatal(err)
	}
	acceptModeUpdate(t, env, 1, "select", env.session.String(), "")
	acceptModeUpdate(t, env, 2, "multisession", "", "on")
	aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
	if err != nil || len(aliases) != 1 || aliases[0].SessionID != env.session.String() {
		t.Fatalf("archived helper got alias %+v %v", aliases, err)
	}
	// Even a legacy stale binding cannot make an archived helper visible.
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_bindings(bot_id,user_id,chat_id,message_thread_id,session_id) VALUES('bot',99,20,0,$1)`, helper.session); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, helper, 2, "agent_progress_message", "turn-helper", "")
	progressEvent(t, helper, 3, "user_message", "turn-helper", "")
	progressEvent(t, helper, 4, "turn_completed", "turn-helper", "")
	if got := sessionModeDeliveries(t, env.store); len(got) != 0 {
		t.Fatalf("archived helper leaked deliveries %+v", got)
	}
	progressEvent(t, env, 5, "agent_progress_message", "turn-a", "")
	live := sessionModeDeliveries(t, env.store)[0]
	checkpointProgress(t, env.store, live, 100)
	progressEvent(t, env, 6, "tool_progress_message", "turn-a", "")
	queued := sessionModeDeliveries(t, env.store)[0]
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET archived=TRUE WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	if suppress, err := env.store.SuppressTelegramDelivery(ctx, queued.ID); err != nil || !suppress {
		t.Fatalf("archived progress escaped send guard %v %v", suppress, err)
	}
	deletions, err := env.store.ClaimTelegramDeletions(ctx, 100)
	if err != nil || len(deletions) != 1 {
		t.Fatalf("archived visible progress survived %+v %v", deletions, err)
	}
	if aliases, err := env.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0); err != nil || len(aliases) != 0 {
		t.Fatalf("archived aliases remained listed %+v %v", aliases, err)
	}
}
