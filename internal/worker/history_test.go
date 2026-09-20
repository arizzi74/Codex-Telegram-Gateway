package worker

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestHistoryCommandPreservesIdleColdAndRunningSessions(t *testing.T) {
	for _, state := range []string{"idle", "not_loaded", "running"} {
		t.Run(state, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "thread-history", CWD: runtime.DefaultCWD, State: state, Loaded: state != "not_loaded"})
			if err != nil {
				t.Fatal(err)
			}
			actor := &sessionActor{agent: a, runtime: runtime, session: session, finalText: "keep final", legacyFinalText: "keep legacy", hasFinalAnswer: true}
			if state == "running" {
				actor.session.ActiveTurnID = "active-turn"
				actor.activeCommand = &protocol.Command{ID: "original-command"}
				actor.awaitingTurnStart = true
				actor.queue = []protocol.Command{{ID: "queued-command"}}
			}
			before := actor.session
			active := actor.activeCommand
			server.SetThreads([]map[string]any{historyThread(session.ThreadID, "cli prompt")}, nil)
			command := agentCommand(runtime, session, protocol.ReadHistory)
			command.Arguments.History = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 10}
			result := runHistoryCommand(t, actor, command)
			if result.State != CommandCompleted || result.Result == nil || result.Result.History == nil || len(result.Result.History.Messages) != 1 || result.Result.History.Messages[0].Text != "cli prompt" {
				t.Fatalf("history result = %#v", result)
			}
			if result.Result.TurnID != "" || result.Result.Session != nil || result.Result.Text != "" {
				t.Fatalf("history response changed turn/session correlation: %#v", result.Result)
			}
			if actor.session != before || actor.activeCommand != active || actor.finalText != "keep final" || actor.legacyFinalText != "keep legacy" || !actor.hasFinalAnswer {
				t.Fatal("history changed session actor state")
			}
			if state == "running" && (!actor.awaitingTurnStart || len(actor.queue) != 1) {
				t.Fatal("history changed active turn or queued prompts")
			}
			sessions, err := a.store.ListSessions(runtime.ID)
			if err != nil || len(sessions) != 1 || sessions[0] != session {
				t.Fatalf("history rewrote saved session: %#v, %v", sessions, err)
			}
			calls := historyOperationCalls(server.Calls())
			if len(calls) != 1 || calls[0].Method != "thread/turns/list" {
				t.Fatalf("history made non-read RPCs: %#v", calls)
			}
			var params map[string]any
			if err := json.Unmarshal(calls[0].Params, &params); err != nil || params["threadId"] != session.ThreadID || params["itemsView"] != "full" || params["limit"] != float64(1) || params["sortDirection"] != "desc" {
				t.Fatalf("history RPC = %#v, %v", params, err)
			}
		})
	}
}

func TestHistoryCommandFailuresPreserveSessionAndHideRawErrors(t *testing.T) {
	for _, scenario := range []string{"unavailable", "unsupported", "rpc", "workspace", "stale", "wrong_session", "wrong_thread", "expired", "cursor"} {
		t.Run(scenario, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "thread-history", CWD: runtime.DefaultCWD, State: "running", Loaded: true, ActiveTurnID: "live-turn"}
			actor := &sessionActor{agent: a, runtime: runtime, session: session, activeCommand: &protocol.Command{ID: "live-command"}, finalText: "keep final"}
			command := agentCommand(runtime, session, protocol.ReadHistory)
			command.Arguments.History = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 10}
			server.SetThreads([]map[string]any{historyThread(session.ThreadID, "saved")}, nil)
			wantCode, wantReads := protocol.CodexUnavailable, 1
			switch scenario {
			case "unavailable":
				server.SetThreads([]map[string]any{{"id": session.ThreadID}}, nil)
			case "unsupported":
				server.SetMethodUnavailable("thread/turns/list", true)
				wantCode = protocol.CodexMethodUnsupported
			case "rpc":
				server.SetRPCError("thread/turns/list", -32000, "secret saved prompt at /private/history")
			case "workspace":
				actor.session.CWD = t.TempDir()
				wantCode, wantReads = protocol.InvalidWorkspace, 0
			case "stale":
				command.RuntimeGeneration++
				wantCode, wantReads = protocol.StaleRuntime, 0
			case "wrong_session":
				command.SessionID = uuid.NewString()
				wantCode, wantReads = protocol.UnknownSession, 0
			case "wrong_thread":
				command.ThreadID = "another-thread"
				wantCode, wantReads = protocol.UnknownSession, 0
			case "expired":
				command.CreatedAt, command.ExpiresAt = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
				wantCode, wantReads = protocol.CommandExpired, 0
			case "cursor":
				command.Arguments.History.Before = &protocol.HistoryCursor{TurnID: "missing", ItemID: "missing"}
				wantCode = protocol.CodexCommandInvalid
			}
			before, active := actor.session, actor.activeCommand
			record := runHistoryCommand(t, actor, command)
			if record.State != CommandFailed || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != wantCode {
				t.Fatalf("failure result = %#v, want %s", record, wantCode)
			}
			if strings.Contains(record.Result.Error.Message, "secret") || strings.Contains(record.Result.Error.Message, "/private") {
				t.Fatal("raw history read error was exposed")
			}
			if actor.session != before || actor.activeCommand != active || actor.finalText != "keep final" {
				t.Fatal("failed history read changed an active session")
			}
			if calls := historyOperationCalls(server.Calls()); len(calls) != wantReads || countCall(calls, "thread/turns/list") != wantReads {
				t.Fatalf("unexpected history RPCs: %#v", calls)
			}
		})
	}
}

func TestHistoryDefaultReadsNewestPromptsWithoutTraversingOldTurns(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "large-thread", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	turns := make([]map[string]any, 100)
	for i := range turns {
		turnID := fmt.Sprintf("turn-%03d", i)
		turns[i] = map[string]any{"id": turnID, "startedAt": int64(1800441000 + i), "items": []map[string]any{{"id": "user", "type": "userMessage", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("prompt %d", i)}}}}}
	}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "turns": turns}}, nil)
	command := agentCommand(runtime, session, protocol.ReadHistory)
	command.Arguments.History = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: protocol.DefaultHistoryLimit}
	record := runHistoryCommand(t, actor, command)
	page := record.Result.History
	if page == nil || page.Limit != 2 || len(page.Messages) != 2 || page.Messages[0].Text != "prompt 99" || page.Messages[1].Text != "prompt 98" || page.Next == nil || page.Next.TurnID != "turn-098" {
		t.Fatalf("default history did not start at latest prompt: %#v", page)
	}
	if got := countCall(historyOperationCalls(server.Calls()), "thread/turns/list"); got != 3 {
		t.Fatalf("reading two prompts traversed old conversation history: got %d requests, want 3 including older-page check", got)
	}
	if page.Messages[0].Timestamp == nil || page.Messages[0].Timestamp.Unix() != 1800441099 {
		t.Fatalf("history did not preserve saved timestamp: %#v", page.Messages[0])
	}
	// New input after the first page must not move the exclusive older cursor.
	newTurn := map[string]any{"id": "turn-100", "items": []map[string]any{{"id": "user", "type": "userMessage", "content": []map[string]any{{"type": "text", "text": "prompt 100"}}}}}
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "turns": append(turns, newTurn)}}, nil)
	command = agentCommand(runtime, session, protocol.ReadHistory)
	command.Arguments.History = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 2, Before: page.Next}
	record = runHistoryCommand(t, actor, command)
	page = record.Result.History
	if page == nil || len(page.Messages) != 2 || page.Messages[0].Text != "prompt 97" || page.Messages[1].Text != "prompt 96" {
		t.Fatalf("older page skipped or repeated prompts after append: %#v", page)
	}
}

func TestHistoryFiltersRecordedGatewayInputByTurnAndCount(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), ThreadID: "thread-history"}
	add := func(op protocol.Operation, text, expected, actual string, state CommandState, thread string) {
		t.Helper()
		command := agentCommand(runtime, session, op)
		command.ThreadID, command.Arguments.Text, command.ExpectedTurnID = thread, text, expected
		if op == protocol.CodexCommand {
			command.Arguments.Codex = &protocol.CodexCommandPayload{Name: "init"}
		}
		if _, err := a.store.Receive(command); err != nil {
			t.Fatal(err)
		}
		if err := a.store.SetCommandState(command.ID, state, &protocol.Result{TurnID: actual}); err != nil {
			t.Fatal(err)
		}
	}
	add(protocol.StartTurn, "same", "", "turn-1", CommandCompleted, session.ThreadID)
	add(protocol.Steer, "same", "turn-1", "", CommandCompleted, session.ThreadID)
	add(protocol.StartTurn, "cli-only", "", "turn-1", CommandCompleted, "other-thread")
	add(protocol.Steer, "failed submission", "turn-1", "", CommandFailed, session.ThreadID)
	add(protocol.StartTurn, "failed turn", "", "turn-2", CommandFailed, session.ThreadID)
	add(protocol.CodexCommand, "", "", "turn-init", CommandCompleted, session.ThreadID)
	prompts := []codexadapter.UserPrompt{
		{TurnID: "turn-1", ItemID: "gateway-start", Text: "same"},
		{TurnID: "turn-1", ItemID: "gateway-steer", Text: "same"},
		{TurnID: "turn-1", ItemID: "cli-repeat", Text: "same"},
		{TurnID: "turn-1", ItemID: "cli-only", Text: "cli-only"},
		{TurnID: "turn-1", ItemID: "cli-failed-match", Text: "failed submission"},
		{TurnID: "turn-2", ItemID: "cli-another-turn", Text: "same"},
		{TurnID: "turn-2", ItemID: "gateway-failed-turn", Text: "failed turn"},
		{TurnID: "turn-init", ItemID: "gateway-init", Text: initPrompt},
		{TurnID: "cli-init", ItemID: "cli-init", Text: initPrompt},
	}
	got, err := a.store.externalHistoryPrompts(runtime.ID, session.ThreadID, prompts)
	want := []codexadapter.UserPrompt{prompts[2], prompts[3], prompts[4], prompts[5], prompts[8]}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered history = %#v, err=%v", got, err)
	}
	// Re-reading is deterministic: the durable ledger is never consumed.
	again, err := a.store.externalHistoryPrompts(runtime.ID, session.ThreadID, prompts)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("second read differs: %#v, err=%v", again, err)
	}
}

func TestHistoryIncludesRecordedTelegramInputBeforePagingAndRedacting(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "thread-history", CWD: runtime.DefaultCWD, State: "idle", Loaded: true}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	var err error
	a.redactor, err = auth.NewRedactor([]string{`secret-[12]`}, "[hidden]")
	if err != nil {
		t.Fatal(err)
	}
	server.SetThreads([]map[string]any{historyThread(session.ThreadID, "old CLI", "secret-1", "new CLI", "secret-2")}, nil)
	gateway := agentCommand(runtime, session, protocol.Steer)
	gateway.Arguments.Text, gateway.ExpectedTurnID = "secret-2", "turn"
	if _, err := a.store.Receive(gateway); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetCommandState(gateway.ID, CommandCompleted, &protocol.Result{TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.ReadHistory)
	command.Arguments.History = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 2}
	record := runHistoryCommand(t, actor, command)
	page := record.Result.History
	if page == nil || len(page.Messages) != 2 || page.Messages[0].Text != "[hidden]" || page.Messages[1].Text != "new CLI" || page.Next == nil || page.Next.ItemID != "item-2" {
		t.Fatalf("wrong order of filtering, paging, redaction: %#v", page)
	}
}

func runHistoryCommand(t *testing.T, actor *sessionActor, command protocol.Command) CommandRecord {
	t.Helper()
	if _, err := actor.agent.store.Receive(command); err != nil {
		t.Fatal(err)
	}
	reply := make(chan commandReply, 1)
	actor.command(actorCommand{command: command, reply: reply})
	if r := <-reply; r.err != nil {
		t.Fatal(r.err)
	}
	record, found, err := actor.agent.store.LoadCommand(command.ID)
	if err != nil || !found {
		t.Fatalf("history record not saved: found=%v, err=%v", found, err)
	}
	select {
	case err := <-actor.agent.fatal:
		t.Fatal(err)
	default:
	}
	return record
}

func historyThread(threadID string, texts ...string) map[string]any {
	items := make([]map[string]any, 0, len(texts))
	for i, text := range texts {
		items = append(items, map[string]any{"id": fmt.Sprintf("item-%d", i), "type": "userMessage", "content": []map[string]any{{"type": "text", "text": text}}})
	}
	return map[string]any{"id": threadID, "turns": []map[string]any{{"id": "turn", "items": items}}}
}

func historyOperationCalls(calls []codextest.Call) []codextest.Call {
	var operations []codextest.Call
	for _, call := range calls {
		if call.Method != "initialize" && call.Method != "initialized" {
			operations = append(operations, call)
		}
	}
	return operations
}

// Reproduce a long-lived turn with old CLI input and a recent Telegram prompt:
// a history request must include that prompt and the latest final answer.
func TestHistoryDefaultIncludesLatestTelegramPromptAndCodexReply(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "long-lived", CWD: runtime.DefaultCWD, State: "idle"}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	thread := historyThread(session.ThreadID, "morning CLI prompt", "recent Telegram prompt")
	turn := thread["turns"].([]map[string]any)[0]
	turn["items"] = append(turn["items"].([]map[string]any), map[string]any{"id": "answer", "type": "agentMessage", "phase": "final_answer", "text": "latest Codex reply"})
	server.SetThreads([]map[string]any{thread}, nil)
	prompt := agentCommand(runtime, session, protocol.Steer)
	prompt.Arguments.Text, prompt.ExpectedTurnID = "recent Telegram prompt", "turn"
	if _, err := a.store.Receive(prompt); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetCommandState(prompt.ID, CommandCompleted, &protocol.Result{TurnID: "turn"}); err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, session, protocol.ReadHistory)
	command.Arguments.History = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: protocol.DefaultHistoryLimit}
	page := runHistoryCommand(t, actor, command).Result.History
	if page == nil || !page.Conversation || !page.NewestFirst || len(page.Messages) != 2 || page.Messages[0].Text != "latest Codex reply" || page.Messages[1].Text != "recent Telegram prompt" || page.Next == nil || page.Next.ItemID != "item-1" {
		t.Fatalf("history skipped latest conversation: %#v", page)
	}
	if got := countCall(historyOperationCalls(server.Calls()), "thread/turns/list"); got != 1 {
		t.Fatalf("history made %d reads for latest two messages in one turn", got)
	}
}

func TestHistoryBoundsAndRedactsWithoutSkippingOlderMessages(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "long-messages", CWD: runtime.DefaultCWD, State: "idle"}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	var err error
	a.redactor, err = auth.NewRedactor([]string{"secret-value"}, "[hidden]")
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 7)
	for i := range texts {
		texts[i] = "secret-value" + strings.Repeat("界", historyPromptRunes)
	}
	server.SetThreads([]map[string]any{historyThread(session.ThreadID, texts...)}, nil)
	request := &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 50}
	var visited []string
	for pages := 0; pages < 5; pages++ {
		command := agentCommand(runtime, session, protocol.ReadHistory)
		command.Arguments.History = request
		page := runHistoryCommand(t, actor, command).Result.History
		if page == nil || len(page.Messages) == 0 {
			t.Fatal("history made no progress")
		}
		total := 0
		for _, message := range page.Messages {
			if !message.Truncated || !utf8.ValidString(message.Text) || !strings.HasPrefix(message.Text, "[hidden]") || strings.Contains(message.Text, "secret-value") {
				t.Fatalf("invalid message %q", message.ItemID)
			}
			total += utf8.RuneCountInString(message.Text)
			visited = append(visited, message.ItemID)
		}
		if total > historyPageRunes {
			t.Fatalf("page exceeds budget: %d", total)
		}
		if page.Next == nil {
			break
		}
		request = &protocol.HistoryRequest{Messages: true, NewestFirst: true, Limit: 50, Before: page.Next}
	}
	want := []string{"item-6", "item-5", "item-4", "item-3", "item-2", "item-1", "item-0"}
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("history skipped/repeated messages: %v", visited)
	}
}

func TestHistoryLegacyGatewayRequestStillReceivesPromptPage(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := protocol.Session{ID: uuid.NewString(), RuntimeID: runtime.ID, ThreadID: "legacy-history", CWD: runtime.DefaultCWD, State: "idle"}
	actor := &sessionActor{agent: a, runtime: runtime, session: session}
	server.SetThreads([]map[string]any{historyThread(session.ThreadID, "old", "new")}, nil)
	command := agentCommand(runtime, session, protocol.ReadHistory)
	command.Arguments.History = &protocol.HistoryRequest{Limit: 1}
	page := runHistoryCommand(t, actor, command).Result.History
	if page == nil || page.Conversation || len(page.Messages) != 0 || len(page.Prompts) != 1 || page.Prompts[0].Text != "new" || page.Next == nil || page.Next.ItemID != "item-1" {
		t.Fatalf("old gateway request got incompatible history: %#v", page)
	}
	command = agentCommand(runtime, session, protocol.ReadHistory)
	command.Arguments.History = &protocol.HistoryRequest{Limit: 1, Before: page.Next}
	page = runHistoryCommand(t, actor, command).Result.History
	if page == nil || page.Conversation || len(page.Prompts) != 1 || page.Prompts[0].Text != "old" || page.Next != nil {
		t.Fatalf("old gateway cursor got incompatible history: %#v", page)
	}
}
