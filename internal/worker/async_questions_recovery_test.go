package worker

import (
	"encoding/json"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func asyncRecoveryActor(t *testing.T, a *Agent, runtime protocol.Runtime) *sessionActor {
	t.Helper()
	session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "recovery-thread", CWD: runtime.DefaultCWD, State: "idle", Loaded: true})
	if err != nil {
		t.Fatal(err)
	}
	return &sessionActor{agent: a, runtime: runtime, session: session}
}

func asyncRecoveryApproval(actor *sessionActor, itemID string) protocol.Approval {
	return protocol.Approval{Async: true, RequestID: "async:" + itemID, ThreadID: actor.session.ThreadID, TurnID: "old", ItemID: itemID, Type: "user_input", State: "pending", Questions: []protocol.Question{{ID: "q1", Prompt: "Which hardware?"}}}
}

func asyncRecoveryQuestion(itemID string) map[string]any {
	return map[string]any{"type": "agentMessage", "delivery": "async", "id": itemID, "text": "Choose hardware", "questions": []map[string]any{{"title": "Which hardware?"}}}
}

func asyncRecoveryUserInput(text string) map[string]any {
	return map[string]any{"type": "userMessage", "id": "user-input", "content": []map[string]any{{"type": "text", "text": text}}}
}

func TestAsyncQuestionRecoveryDoesNotImportUnknownHistory(t *testing.T) {
	for _, withPending := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_saved_requests", true: "saved_request"}[withPending], func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			actor := asyncRecoveryActor(t, a, runtime)
			items := []map[string]any{asyncRecoveryQuestion("historical-only")}
			if withPending {
				actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "saved-live"))
				items = append(items, asyncRecoveryQuestion("saved-live"))
			}
			if err := server.SetMethodResult("thread/turns/list", map[string]any{"data": []map[string]any{{"id": "old", "items": items}}}); err != nil {
				t.Fatal(err)
			}
			actor.recoverAsyncQuestions()
			actor.asyncRecoveredGeneration = 0
			actor.recoverAsyncQuestions()
			records, err := a.store.asyncQuestionRecords(actor.session.ID)
			want := 0
			if withPending {
				want = 1
			}
			if err != nil || len(records) != want || len(actor.asyncQuestions) != want {
				t.Fatalf("historical question was imported: records=%#v pending=%#v err=%v", records, actor.asyncQuestions, err)
			}
			if withPending && records[0].Approval.RequestID != "async:saved-live" {
				t.Fatalf("wrong request recovered: %#v", records)
			}
			events, _ := a.store.OutboxAfter(0)
			if len(events) != want {
				t.Fatalf("recovery emitted another notification: %#v", events)
			}
			if !withPending && hasCall(server.Calls(), "thread/turns/list") {
				t.Fatal("history was read without a durable request to reconcile")
			}
		})
	}
}

func TestAsyncQuestionRecoverySupersedesOldRequestsBeforeReannouncing(t *testing.T) {
	for _, changedGeneration := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_generation", true: "new_generation"}[changedGeneration], func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			actor := asyncRecoveryActor(t, a, runtime)
			actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "old-import"))
			if changedGeneration {
				client, _, _ := a.manager.Client(runtime.ID)
				actor.runtime.Generation++
				a.manager.install(actor.runtime, client)
			}
			if err := server.SetMethodResult("thread/turns/list", map[string]any{"data": []map[string]any{{"id": "old", "items": []map[string]any{asyncRecoveryQuestion("old-import"), asyncRecoveryUserInput("Authenticated")}}}}); err != nil {
				t.Fatal(err)
			}
			actor.recoverAsyncQuestions()
			records, err := a.store.asyncQuestionRecords(actor.session.ID)
			if err != nil || len(records) != 1 || records[0].State != "superseded" || len(actor.asyncQuestions) != 0 {
				t.Fatalf("ordinary input did not clear the old request: %#v, %v", records, err)
			}
			actor.asyncRecoveredGeneration = 0
			actor.recoverAsyncQuestions()
			events, _ := a.store.OutboxAfter(0)
			wantEvents := 2
			if changedGeneration {
				wantEvents = 1
			}
			if len(events) != wantEvents {
				t.Fatalf("unexpected recovery notifications: %#v", events)
			}
			for _, event := range events[1:] {
				if event.Kind != "approval_resolved" {
					t.Fatalf("stale request was reannounced before clearing: %#v", event)
				}
			}
		})
	}
}

func TestAsyncQuestionRecoveryPreservesDurablePartialAnswersAndTombstones(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "partial")
	approval.Questions = []protocol.Question{{ID: "q2", Prompt: "Which display?"}}
	actor.rememberAsyncQuestion(approval)
	items := []map[string]any{{"type": "agentMessage", "delivery": "async", "id": "partial", "text": "Choose", "questions": []map[string]any{{"title": "Which hardware?"}, {"title": "Which display?"}}}}
	for _, state := range []string{"resolved", "superseded", "submitting", "outcome_unknown"} {
		itemID := "old-" + state
		actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, itemID))
		if err := a.store.setAsyncQuestionState(runtime, actor.session.ID, "async:"+itemID, state, false); err != nil {
			t.Fatal(err)
		}
		items = append(items, asyncRecoveryQuestion(itemID))
	}
	if err := server.SetMethodResult("thread/turns/list", map[string]any{"data": []map[string]any{{"id": "old", "items": items}}}); err != nil {
		t.Fatal(err)
	}
	before, _ := a.store.OutboxAfter(0)
	actor.recoverAsyncQuestions()
	if len(actor.asyncQuestions) != 1 {
		t.Fatalf("tombstones were reopened: %#v", actor.asyncQuestions)
	}
	remaining := actor.asyncQuestions[approval.RequestID].Questions
	if len(remaining) != 1 || remaining[0].ID != "q2" {
		t.Fatalf("history restored an answered field: %#v", remaining)
	}
	after, _ := a.store.OutboxAfter(0)
	if len(after) != len(before) {
		t.Fatalf("recovery replayed notifications: before=%d after=%d", len(before), len(after))
	}
}

func TestAsyncQuestionRecoveryHistoryFailurePreservesPendingAndRetries(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "saved-live"))
	// Invalid history must not be used as evidence to discard a saved prompt.
	if err := server.SetMethodResult("thread/turns/list", json.RawMessage(`{"data":[{"id":"old","items":null}]}`)); err != nil {
		t.Fatal(err)
	}
	actor.recoverAsyncQuestions()
	if len(actor.asyncQuestions) != 1 || actor.asyncRecoveredGeneration != 0 {
		t.Fatalf("failed history lost pending or prevented retry: %#v, generation=%d", actor.asyncQuestions, actor.asyncRecoveredGeneration)
	}
	if err := server.SetMethodResult("thread/turns/list", map[string]any{"data": []map[string]any{{"id": "old", "items": []map[string]any{asyncRecoveryQuestion("saved-live"), asyncRecoveryUserInput("Proceed")}}}}); err != nil {
		t.Fatal(err)
	}
	actor.recoverAsyncQuestions()
	if len(actor.asyncQuestions) != 0 || actor.asyncRecoveredGeneration != runtime.Generation {
		t.Fatalf("history retry did not reconcile: %#v, generation=%d", actor.asyncQuestions, actor.asyncRecoveredGeneration)
	}
}

func TestAsyncQuestionLiveInputSupersession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		text        string
		wantPending int
		wantState   string
	}{
		{name: "ordinary_reply", text: "Authenticated", wantState: "superseded"},
		{name: "new_task", text: "Proceed with the correction", wantState: "superseded"},
		{name: "environment", text: "<environment_context>\n<cwd>/example/project</cwd>\n</environment_context>", wantPending: 2, wantState: "pending"},
		{name: "native_answer", text: codexadapter.FormatAsyncQuestionAnswer("Which hardware?", "Mac"), wantPending: 1, wantState: "resolved"},
		{name: "other_native_answer", text: codexadapter.FormatAsyncQuestionAnswer("Unrelated question?", "Yes"), wantPending: 2, wantState: "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			actor := asyncRecoveryActor(t, a, runtime)
			actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "hardware"))
			other := asyncRecoveryApproval(actor, "display")
			other.Questions[0].Prompt = "Which display?"
			actor.rememberAsyncQuestion(other)
			actor.observeAsyncAnswer(tc.text)
			if len(actor.asyncQuestions) != tc.wantPending {
				t.Fatalf("pending requests=%#v, want count %d", actor.asyncQuestions, tc.wantPending)
			}
			records, _ := a.store.asyncQuestionRecords(actor.session.ID)
			for _, record := range records {
				if record.Approval.ItemID == "hardware" && record.State != tc.wantState {
					t.Fatalf("hardware state=%q want %q", record.State, tc.wantState)
				}
			}
			if hasCall(server.Calls(), "turn/start") || hasCall(server.Calls(), "turn/steer") || hasCall(server.Calls(), "thread/resume") {
				t.Fatal("input observation changed the native session")
			}
		})
	}
}

func TestAsyncQuestionIgnoresDuplicateLateAndMalformedUserEvents(t *testing.T) {
	for _, answerKind := range []string{"ordinary", "quoted"} {
		for _, invalid := range []string{"duplicate", "old_turn", "missing_turn", "missing_item", "empty_text", "completed_turn"} {
			t.Run(answerKind+"/"+invalid, func(t *testing.T) {
				a, runtime, _, cleanup := testAgent(t)
				defer cleanup()
				actor := asyncRecoveryActor(t, a, runtime)
				actor.event(codexadapter.Event{Kind: "turn_started", TurnID: "current"})
				text := "Proceed"
				if answerKind == "quoted" {
					text = codexadapter.FormatAsyncQuestionAnswer("Which hardware?", "Mac")
				}
				// The original input precedes this question. Receiving it again
				// must never answer or supersede the newer pending request.
				initial := codexadapter.Event{Kind: "user_message_completed", TurnID: "current", ItemID: "initial", Text: text}
				actor.event(initial)
				actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "new-question"))
				event := initial
				event.ItemID = "late-input"
				switch invalid {
				case "duplicate":
					event.ItemID = initial.ItemID
				case "old_turn":
					event.TurnID = "previous"
				case "missing_turn":
					event.TurnID = ""
				case "missing_item":
					event.ItemID = ""
				case "empty_text":
					event.Text = " \n\t"
				case "completed_turn":
					actor.event(codexadapter.Event{Kind: "turn_completed", TurnID: "current"})
				}
				actor.event(event)
				records, err := a.store.asyncQuestionRecords(actor.session.ID)
				if err != nil || len(records) != 1 || records[0].State != "pending" || len(actor.asyncQuestions) != 1 {
					t.Fatalf("invalid input cleared a newer question: records=%#v pending=%#v err=%v", records, actor.asyncQuestions, err)
				}
				events, _ := a.store.OutboxAfter(0)
				for _, emitted := range events {
					if emitted.Kind == "approval_resolved" {
						t.Fatalf("invalid input emitted a question resolution: %#v", emitted)
					}
				}
				// A fresh item with identical text remains a valid input.
				actor.event(codexadapter.Event{Kind: "turn_started", TurnID: "current"})
				actor.event(codexadapter.Event{Kind: "user_message_completed", TurnID: "current", ItemID: "fresh-input", Text: text})
				records, err = a.store.asyncQuestionRecords(actor.session.ID)
				wantState := "superseded"
				if answerKind == "quoted" {
					wantState = "resolved"
				}
				if err != nil || len(records) != 1 || records[0].State != wantState || len(actor.asyncQuestions) != 0 {
					t.Fatalf("fresh valid input was ignored: records=%#v pending=%#v err=%v", records, actor.asyncQuestions, err)
				}
			})
		}
	}
}
