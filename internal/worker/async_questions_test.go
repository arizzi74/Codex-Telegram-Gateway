package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func emitTestAsyncQuestion(t *testing.T, a *Agent, runtime protocol.Runtime, session protocol.Session, turn string) protocol.Approval {
	t.Helper()
	a.onEvent(runtime, codexadapter.Event{Kind: "input_requested_async", Async: true, ThreadID: session.ThreadID, TurnID: turn, ItemID: "question-call", Text: "Choose hardware", Questions: []codexadapter.Question{{ID: "q1", Prompt: "Which hardware?", Choices: []codexadapter.Choice{{Label: "Mac"}, {Label: "Windows"}}}}})
	var approval protocol.Approval
	waitFor(t, func() bool {
		records, err := a.store.asyncQuestionRecords(session.ID)
		if err == nil && len(records) == 1 {
			approval = records[0].Approval
			return true
		}
		return false
	})
	return approval
}

func asyncAnswerCommand(runtime protocol.Runtime, session protocol.Session, approval protocol.Approval) protocol.Command {
	c := agentCommand(runtime, session, protocol.InputResponse)
	c.ExpectedTurnID = ""
	c.Arguments = protocol.Arguments{RequestID: approval.RequestID, ApprovalID: approval.ID, Answers: map[string][]string{"q1": {"Mac"}}}
	return c
}

func waitAsyncCommand(t *testing.T, a *Agent, command protocol.Command, state CommandState) {
	t.Helper()
	if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack: %#v %v", ack, err)
	}
	waitFor(t, func() bool {
		r, ok, err := a.store.LoadCommand(command.ID)
		return err == nil && ok && r.State == state
	})
}

func TestAsyncQuestionSurvivesCompletionAndStartsAnswerTurn(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "async-thread", "")
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "original"})
	a.onEvent(runtime, codexadapter.Event{Kind: "agent_message_completed", ThreadID: session.ThreadID, TurnID: "original", Phase: "final_answer", Text: "Actual final answer"})
	approval := emitTestAsyncQuestion(t, a, runtime, session, "original")
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: "original"})
	waitFor(t, func() bool { return sessionTurn(a.store, session.ID) == "" })
	records, _ := a.store.asyncQuestionRecords(session.ID)
	if len(records) != 1 || records[0].State != "pending" {
		t.Fatalf("question lost on completion: %#v", records)
	}
	events, _ := a.store.OutboxAfter(0)
	found := false
	for _, event := range events {
		if event.Kind == "agent_progress_message" && strings.Contains(string(event.Data), "Choose hardware") {
			t.Fatal("async question became ephemeral")
		}
		if event.Kind == "turn_completed" {
			var result protocol.Result
			_ = json.Unmarshal(event.Data, &result)
			found = result.Text == "Actual final answer"
		}
	}
	if !found {
		t.Fatal("async question overwrote the final answer")
	}
	command := asyncAnswerCommand(runtime, session, approval)
	waitAsyncCommand(t, a, command, CommandCompleted)
	if countCall(server.Calls(), "turn/start") != 1 || hasCall(server.Calls(), "turn/steer") {
		t.Fatalf("idle answer transport: %#v", server.Calls())
	}
	records, _ = a.store.asyncQuestionRecords(session.ID)
	if records[0].State != "resolved" {
		t.Fatalf("answer not persisted: %#v", records)
	}
	if !hasTurnEventForCommand(a.store, "turn_started", command.ID, sessionTurn(a.store, session.ID)) {
		t.Fatal("answer turn lost command correlation")
	}
}

func TestAsyncQuestionAnswerSteersActiveTurnAndRejectsReplay(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "async-active", "")
	start := agentCommand(runtime, session, protocol.StartTurn)
	waitAsyncCommand(t, a, start, CommandCompleted)
	turn := sessionTurn(a.store, session.ID)
	approval := emitTestAsyncQuestion(t, a, runtime, session, turn)
	command := asyncAnswerCommand(runtime, session, approval)
	waitAsyncCommand(t, a, command, CommandCompleted)
	if countCall(server.Calls(), "turn/start") != 1 || countCall(server.Calls(), "turn/steer") != 1 || sessionTurn(a.store, session.ID) != turn {
		t.Fatal("answer did not preserve active turn")
	}
	for _, call := range server.Calls() {
		if call.Method != "turn/steer" {
			continue
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Expected string `json:"expectedTurnId"`
			Input    []struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		if err := json.Unmarshal(call.Params, &p); err != nil || p.ThreadID != session.ThreadID || p.Expected != turn || len(p.Input) != 1 || p.Input[0].Text != "> Which hardware?\n\nMac" {
			t.Fatalf("native answer framing: %s %v", call.Params, err)
		}
	}
	replay := asyncAnswerCommand(runtime, session, approval)
	if ack, err := a.HandleCommand(context.Background(), replay); err != nil || ack.Error == nil || ack.Error.Code != protocol.ApprovalNotPending {
		t.Fatalf("replay accepted: %#v %v", ack, err)
	}
}

func TestAsyncQuestionDismissAndNativeAnswerNeedNoModelTurn(t *testing.T) {
	for _, action := range []string{"dismiss", "native"} {
		t.Run(action, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "async-dismiss", "")
			approval := emitTestAsyncQuestion(t, a, runtime, session, "old-turn")
			if action == "dismiss" {
				command := asyncAnswerCommand(runtime, session, approval)
				command.Arguments.Answers, command.Arguments.Decision = nil, "dismiss"
				waitAsyncCommand(t, a, command, CommandCompleted)
			} else {
				a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "later"})
				a.onEvent(runtime, codexadapter.Event{Kind: "user_message_completed", ThreadID: session.ThreadID, TurnID: "later", ItemID: "native-answer", Text: "> Which hardware?\n\nMac"})
			}
			waitFor(t, func() bool {
				records, _ := a.store.asyncQuestionRecords(session.ID)
				return len(records) == 1 && records[0].State == "resolved"
			})
			if hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/steer") || hasCall(server.Calls(), "thread/resume") {
				t.Fatal("dismiss/native notification submitted input")
			}
			resolved := false
			events, _ := a.store.OutboxAfter(0)
			for _, event := range events {
				if event.Kind == "approval_resolved" {
					resolved = true
				}
			}
			if !resolved {
				t.Fatal("gateway pending request was not cleared")
			}
		})
	}
}

func TestAsyncQuestionColdLoadFailureKeepsPendingAndNeverSubmits(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installColdSession(a, runtime, "cold-async")
	approval := emitTestAsyncQuestion(t, a, runtime, session, "finished-turn")
	server.SetRPCError("thread/resume", -32603, "temporary load failure")
	command := asyncAnswerCommand(runtime, session, approval)
	waitAsyncCommand(t, a, command, CommandFailed)
	records, _ := a.store.asyncQuestionRecords(session.ID)
	if len(records) != 1 || records[0].State != "pending" {
		t.Fatalf("unsubmitted answer was lost: %#v", records)
	}
	if hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/steer") {
		t.Fatal("load failure submitted an answer")
	}
	events, _ := a.store.OutboxAfter(0)
	for _, event := range events {
		if event.Kind == "approval_resolved" {
			t.Fatal("load failure cleared the pending question")
		}
	}
}

func TestAsyncQuestionRecoveryClearsPreviouslyPublishedNativeAnswer(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "history-async", CWD: runtime.DefaultCWD, State: "idle", Loaded: true})
	if err != nil {
		t.Fatal(err)
	}
	approval := protocol.Approval{Async: true, RequestID: "async:question-call", ThreadID: session.ThreadID, TurnID: "old", ItemID: "question-call", Type: "user_input", State: "pending", Questions: []protocol.Question{{ID: "q1", Prompt: "Which hardware?"}}}
	if _, _, err := a.store.announceAsyncQuestion(runtime, session.ID, approval); err != nil {
		t.Fatal(err)
	}
	if err := server.SetMethodResult("thread/turns/list", json.RawMessage(`{"data":[{"id":"old","items":[{"type":"agentMessage","delivery":"async","id":"question-call","text":"Choose","questions":[{"title":"Which hardware?"}]},{"type":"userMessage","id":"answer","content":[{"type":"text","text":"> Which hardware?\n\nMac"}]}]}]}`)); err != nil {
		t.Fatal(err)
	}
	a.onSession(runtime, session)
	waitFor(t, func() bool {
		records, _ := a.store.asyncQuestionRecords(session.ID)
		return len(records) == 1 && records[0].State == "resolved"
	})
	events, _ := a.store.OutboxAfter(0)
	resolved := false
	for _, event := range events {
		if event.Kind == "approval_resolved" {
			resolved = true
		}
	}
	if !resolved {
		t.Fatal("history recovery left a ghost question in the gateway")
	}
	if hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/steer") || hasCall(server.Calls(), "thread/resume") {
		t.Fatal("history recovery changed a native session")
	}
}
