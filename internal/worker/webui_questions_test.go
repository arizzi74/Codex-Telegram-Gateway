package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestWebUIQuestionRPCsAreScopedAndAnswersCannotReplay(t *testing.T) {
	r := testWebUIRelay()
	for _, request := range []string{`{"id":1,"method":"gateway/questions","params":{}}`, `{"id":1,"method":"gateway/answer","params":{"approvalId":"approval","result":{"decision":"accept"}}}`} {
		if _, err := r.clientMessage([]byte(request)); err == nil {
			t.Fatal("question access before resume")
		}
	}
	r.resumed = true
	for _, request := range []string{
		`{"id":1,"method":"gateway/questions","params":{"threadId":"other"}}`,
		`{"id":1,"method":"gateway/answer","params":{"approvalId":"approval","result":null}}`,
		`{"id":1,"method":"gateway/answer","params":{"approvalId":"approval","result":{},"threadId":"other"}}`,
	} {
		if _, err := r.clientMessage([]byte(request)); err == nil {
			t.Fatalf("unsafe question RPC accepted: %s", request)
		}
	}
	answer := []byte(`{"id":1,"method":"gateway/answer","params":{"approvalId":"approval","result":{"decision":"accept"}}}`)
	if _, err := r.clientMessage(answer); err != nil {
		t.Fatal(err)
	}
	delete(r.pending, "n:1")
	if _, err := r.clientMessage(answer); err == nil {
		t.Fatal("completed answer ID was replayable")
	}
}

func TestWebUIPendingSnapshotAndObserverAnswerStayCurrentAndScoped(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t, `private-[ab]`)
	defer cleanup()
	session := installSession(a, runtime, "web-question", "turn-question")
	other := installSession(a, runtime, "other-thread", "other-turn")
	client := mustClient(t, a, runtime)
	if err := server.Request("item/tool/requestUserInput", 71, map[string]any{
		"threadId": session.ThreadID, "turnId": "turn-question", "itemId": "input-old", "privateFutureField": "do not forward",
		"questions": []map[string]any{{"id": "q", "question": "Choose hardware", "options": []map[string]string{{"label": "private-a"}, {"label": "private-b"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	var request codexadapter.Request
	select {
	case request = <-client.Requests():
	case <-time.After(time.Second):
		t.Fatal("native question missing")
	}
	a.onRequest(runtime, request)
	waitFor(t, func() bool {
		questions, err := a.webUIQuestions(t.Context(), runtime, session, nil)
		return err == nil && len(questions) == 1
	})
	questions, err := a.webUIQuestions(t.Context(), runtime, session, nil)
	if err != nil || len(questions) != 1 || questions[0].Async || questions[0].Method != "item/tool/requestUserInput" {
		t.Fatalf("snapshot=%#v %v", questions, err)
	}
	raw, _ := json.Marshal(questions)
	visible, err := redactWebUIJSON(a.redactor, raw)
	if err != nil || strings.Contains(string(visible), "private-a") || strings.Contains(string(visible), "privateFutureField") || !strings.Contains(string(visible), "[REDACTED] (option 2)") {
		t.Fatalf("snapshot leaked or lost option identity: %s %v", visible, err)
	}
	answer := &webUIQuestionAnswer{ApprovalID: questions[0].ApprovalID, Result: json.RawMessage(`{"answers":{"q":{"answers":["[REDACTED] (option 2)"]}}}`)}
	if _, err := a.webUIQuestions(t.Context(), runtime, other, answer); err == nil {
		t.Fatal("another session answered this question")
	}
	stale := runtime
	stale.Generation++
	if _, err := a.webUIQuestions(t.Context(), stale, session, answer); err == nil {
		t.Fatal("stale runtime answered question")
	}
	if _, err := a.webUIQuestions(t.Context(), runtime, session, answer); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return responseID(server.Responses(), "71") })
	if !strings.Contains(string(server.Responses()[0].Result), "private-b") {
		t.Fatalf("observer did not restore original selected label: %s", server.Responses()[0].Result)
	}
	if questions, err := a.webUIQuestions(t.Context(), runtime, session, nil); err != nil || len(questions) != 0 {
		t.Fatalf("answered question remained pending: %#v %v", questions, err)
	}
	if _, err := a.webUIQuestions(t.Context(), runtime, session, answer); err == nil {
		t.Fatal("duplicate answer accepted")
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	answered := false
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "command_") {
			t.Fatalf("Web UI answer entered Telegram command queue: %#v", event)
		}
		if event.Kind == "user_input_answered" {
			answered = true
			if strings.Contains(string(event.Data), "private-b") {
				t.Fatal("answer event leaked configured secret")
			}
		}
	}
	if !answered {
		t.Fatal("Telegram question transcript cannot reconcile confirmed browser answer")
	}
}

func TestWebUIAsyncSnapshotDoesNotDependOnTranscriptWindow(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-async", "")
	a.onEvent(runtime, codexadapter.Event{Kind: "input_requested_async", ThreadID: session.ThreadID, TurnID: "old-turn", ItemID: "old-question", Text: "Choose", Questions: []codexadapter.Question{{ID: "q", Prompt: "Which machine?", Choices: []codexadapter.Choice{{Label: "Mac"}}}}})
	var questions []webUIQuestion
	waitFor(t, func() bool {
		var err error
		questions, err = a.webUIQuestions(t.Context(), runtime, session, nil)
		return err == nil && len(questions) == 1
	})
	if !questions[0].Async || string(questions[0].Params["itemId"]) != `"old-question"` {
		t.Fatalf("async question not independently restored: %#v", questions)
	}
	if _, err := a.webUIQuestions(t.Context(), runtime, session, &webUIQuestionAnswer{ApprovalID: questions[0].ApprovalID, Result: json.RawMessage(`{"answers":{"q":{"answers":["Mac"]}}}`)}); err == nil {
		t.Fatal("async question injected into native blocking reply path")
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "later-turn"})
	a.onEvent(runtime, codexadapter.Event{Kind: "user_message_completed", ThreadID: session.ThreadID, TurnID: "later-turn", ItemID: "answer", Text: "> Which machine?\n\nMac"})
	waitFor(t, func() bool {
		questions, err := a.webUIQuestions(t.Context(), runtime, session, nil)
		return err == nil && len(questions) == 0
	})
}

func TestWebUIPendingSnapshotOmitsExternallyResolvedRequest(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-resolved", "turn")
	client := mustClient(t, a, runtime)
	if err := server.Request("item/commandExecution/requestApproval", 90, map[string]any{"threadId": session.ThreadID, "turnId": "turn", "command": "make test", "availableDecisions": []string{"decline"}}); err != nil {
		t.Fatal(err)
	}
	var request codexadapter.Request
	select {
	case request = <-client.Requests():
	case <-time.After(time.Second):
		t.Fatal("native request missing")
	}
	a.onRequest(runtime, request)
	waitFor(t, func() bool { return len(approvalIDs(t, a.store)) == 1 })
	questions, err := a.webUIQuestions(t.Context(), runtime, session, nil)
	if err != nil || len(questions) != 1 {
		t.Fatalf("snapshot=%#v %v", questions, err)
	}
	if _, err := a.webUIQuestions(t.Context(), runtime, session, &webUIQuestionAnswer{ApprovalID: questions[0].ApprovalID, Result: json.RawMessage(`{"decision":"accept"}`)}); err == nil {
		t.Fatal("unoffered approval decision was accepted")
	}
	if err := server.Emit("serverRequest/resolved", map[string]any{"threadId": session.ThreadID, "requestId": 90}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !client.RequestPending(request.RequestID) })
	if questions, err := a.webUIQuestions(t.Context(), runtime, session, nil); err != nil || len(questions) != 0 {
		t.Fatalf("externally resolved request resurrected: %#v %v", questions, err)
	}
}
