package worker

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func answeredQuestionEvents(t *testing.T, store *Store) ([]protocol.Event, []protocol.Approval) {
	t.Helper()
	events, err := store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	var selected []protocol.Event
	var approvals []protocol.Approval
	for _, event := range events {
		if event.Kind != "user_input_answered" {
			continue
		}
		var approval protocol.Approval
		if err := json.Unmarshal(event.Data, &approval); err != nil {
			t.Fatal(err)
		}
		selected, approvals = append(selected, event), append(approvals, approval)
	}
	return selected, approvals
}

func TestAsyncQuestionPartialAnswersEmitBeforeShrinkAndResolution(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "partial")
	approval.Questions = append(approval.Questions, protocol.Question{ID: "q2", Prompt: "Display?"})
	actor.rememberAsyncQuestion(approval)
	original := actor.asyncQuestions[approval.RequestID]
	actor.observeAsyncAnswer(codexadapter.FormatAsyncQuestionAnswer("Which hardware?", "Mac Studio\nApple Silicon"))
	actor.observeAsyncAnswer(codexadapter.FormatAsyncQuestionAnswer("Which hardware?", "Mac Studio\nApple Silicon"))
	actor.observeAsyncAnswer(codexadapter.FormatAsyncQuestionAnswer("Display?", "Two monitors"))
	actor.observeAsyncAnswer(codexadapter.FormatAsyncQuestionAnswer("Display?", "Two monitors"))
	events, answers := answeredQuestionEvents(t, a.store)
	if len(answers) != 2 {
		t.Fatalf("answer events = %#v", answers)
	}
	for index, event := range events {
		if event.RuntimeID != runtime.ID || event.RuntimeGeneration != runtime.Generation || event.SessionID != actor.session.ID || answers[index].ID != original.ID || answers[index].RequestID != original.RequestID || answers[index].ThreadID != original.ThreadID || answers[index].State != "answered" {
			t.Fatalf("answer event lost question identity: %#v / %#v", event, answers[index])
		}
	}
	if !reflect.DeepEqual(answers[0].Answers, map[string][]string{"q1": {"Mac Studio\nApple Silicon"}}) || len(answers[0].Questions) != 1 || answers[0].Questions[0].Prompt != "Which hardware?" || !reflect.DeepEqual(answers[1].Answers, map[string][]string{"q2": {"Two monitors"}}) {
		t.Fatalf("wrong answer projection: %#v", answers)
	}
	all, _ := a.store.OutboxAfter(0)
	var kinds []string
	for _, event := range all {
		kinds = append(kinds, event.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"user_input_requested", "user_input_answered", "user_input_requested", "user_input_answered", "approval_resolved"}) {
		t.Fatalf("answer/shrink/resolve ordering = %#v", kinds)
	}
}

func TestAsyncQuestionAnswersRedactSecretsAndNeverInventAnswers(t *testing.T) {
	for _, text := range []string{"Proceed", "> Unrelated?\n\nMac", "> Which hardware?\n\n   ", "> Which hardware?\n\nMy private answer"} {
		a, runtime, _, cleanup := testAgent(t)
		actor := asyncRecoveryActor(t, a, runtime)
		approval := asyncRecoveryApproval(actor, "secret")
		approval.Questions[0].Secret = true
		actor.rememberAsyncQuestion(approval)
		actor.observeAsyncAnswer(text)
		_, answered := answeredQuestionEvents(t, a.store)
		if strings.Contains(text, "My private answer") {
			if len(answered) != 1 || !reflect.DeepEqual(answered[0].Answers, map[string][]string{"q1": {"[REDACTED]"}}) {
				t.Fatalf("secret answer was exposed: %#v", answered)
			}
		} else if len(answered) != 0 {
			t.Fatalf("unconfirmed answer was invented: %#v", answered)
		}
		cleanup()
	}
}

func TestAsyncQuestionBundlePreservesQuotedAnswerParagraphs(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "bundle")
	approval.Questions = append(approval.Questions, protocol.Question{ID: "q2", Prompt: "Display?"})
	actor.rememberAsyncQuestion(approval)
	first := "Mac\n\n> quoted diagnostic\n\ncontinued output"
	actor.observeAsyncAnswer(codexadapter.FormatAsyncQuestionAnswer("Which hardware?", first) + "\n\n" + codexadapter.FormatAsyncQuestionAnswer("Display?", "Two monitors"))
	_, answered := answeredQuestionEvents(t, a.store)
	if len(answered) != 1 || !reflect.DeepEqual(answered[0].Answers, map[string][]string{"q1": {first}, "q2": {"Two monitors"}}) {
		t.Fatalf("bundled answers lost text or absorbed a later answer: %#v", answered)
	}
}

func TestAsyncQuestionConfirmedTelegramAndRecoveredAnswers(t *testing.T) {
	for _, source := range []string{"telegram", "native_history", "dismiss", "failed", "old_generation"} {
		t.Run(source, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			actor := asyncRecoveryActor(t, a, runtime)
			actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "saved"))
			approval := actor.asyncQuestions["async:saved"]
			if source == "native_history" || source == "old_generation" {
				if source == "old_generation" {
					client, _, _ := a.manager.Client(runtime.ID)
					actor.runtime.Generation++
					a.manager.install(actor.runtime, client)
				}
				if err := server.SetMethodResult("thread/turns/list", map[string]any{"data": []map[string]any{{"id": "old", "items": []map[string]any{asyncRecoveryQuestion("saved"), asyncRecoveryUserInput("> Which hardware?\n\nMac\nSecond display")}}}}); err != nil {
					t.Fatal(err)
				}
				actor.recoverAsyncQuestions()
				actor.asyncRecoveredGeneration = 0
				actor.recoverAsyncQuestions()
			} else {
				command := asyncAnswerCommand(runtime, actor.session, approval)
				command.Arguments.Answers["q1"] = []string{"  Mac\nSecond display  "}
				if source == "dismiss" {
					command.Arguments.Answers, command.Arguments.Decision = nil, "dismiss"
				}
				if source == "failed" {
					server.SetRPCError("turn/start", -32603, "request rejected")
				}
				client, _, _ := a.manager.Client(runtime.ID)
				actor.answerAsyncQuestion(command, client, approval)
			}
			events, answered := answeredQuestionEvents(t, a.store)
			if source == "dismiss" || source == "failed" {
				if len(answered) != 0 {
					t.Fatalf("unconfirmed or stale answer event: %#v", answered)
				}
			} else if len(answered) != 1 || !reflect.DeepEqual(answered[0].Answers, map[string][]string{"q1": {"Mac\nSecond display"}}) {
				t.Fatalf("missing confirmed answer: %#v", answered)
			} else if events[0].RuntimeGeneration != runtime.Generation || answered[0].ID != approval.ID {
				t.Fatalf("recovered answer lost original generation/approval: %#v / %#v", events, answered)
			}
		})
	}
}

func TestAsyncQuestionAnswerOutboxIsAtomicAndDeduplicated(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	actor.rememberAsyncQuestion(asyncRecoveryApproval(actor, "atomic"))
	approval := actor.asyncQuestions["async:atomic"]
	if err := a.store.db.Update(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(math.MaxInt64))
	}); err != nil {
		t.Fatal(err)
	}
	answers := map[string][]string{"q1": {"Mac"}}
	if err := actor.recordAsyncAnswers(approval, answers); err == nil {
		t.Fatal("exhausted outbox accepted an answer")
	}
	records, _ := a.store.asyncQuestionRecords(actor.session.ID)
	if len(records) != 1 || len(records[0].AnsweredIDs) != 0 {
		t.Fatalf("failed answer append changed dedup state: %#v", records)
	}
	if err := a.store.db.Update(func(tx *workerdb.Tx) error { return tx.Bucket(bucketMeta).Put(keyNextEvent, sequenceKey(2)) }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := actor.recordAsyncAnswers(approval, answers); err != nil {
			t.Fatal(err)
		}
	}
	_, published := answeredQuestionEvents(t, a.store)
	if len(published) != 1 {
		t.Fatalf("durable answer replay was not deduplicated: %#v", published)
	}
}

func TestBlockingQuestionAnswerEventsRequireKnownText(t *testing.T) {
	for _, source := range []string{"telegram", "terminal_resolution"} {
		t.Run(source, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "blocking-question", "turn-live")
			if err := server.Request("item/tool/requestUserInput", 74, map[string]any{"threadId": session.ThreadID, "turnId": "turn-live", "itemId": "input-item", "questions": []map[string]any{
				{"id": "q1", "question": "Hardware?", "header": "Hardware", "options": []any{}},
				{"id": "q2", "question": "Private detail?", "header": "Private", "isSecret": true, "options": []any{}},
			}}); err != nil {
				t.Fatal(err)
			}
			var request codexadapter.Request
			select {
			case request = <-mustClient(t, a, runtime).Requests():
			case <-time.After(time.Second):
				t.Fatal("input request not received")
			}
			a.onRequest(runtime, request)
			var approval protocol.Approval
			waitFor(t, func() bool {
				events, _ := a.store.OutboxAfter(0)
				for _, event := range events {
					if event.Kind == "user_input_requested" && json.Unmarshal(event.Data, &approval) == nil {
						return true
					}
				}
				return false
			})
			if source == "telegram" {
				command := agentCommand(runtime, session, protocol.InputResponse)
				command.Arguments = protocol.Arguments{ApprovalID: approval.ID, RequestID: request.RequestID, Answers: map[string][]string{"q1": {"Mac\nSecond display"}, "q2": {"private answer"}}}
				waitAsyncCommand(t, a, command, CommandCompleted)
			} else {
				a.onEvent(runtime, codexadapter.Event{Kind: "server_request_resolved", ThreadID: session.ThreadID, RequestID: request.RequestID})
				waitFor(t, func() bool { return approvalResolved(t, a.store, request.RequestID) })
			}
			_, answers := answeredQuestionEvents(t, a.store)
			if source == "telegram" {
				if len(answers) != 1 || !reflect.DeepEqual(answers[0].Answers, map[string][]string{"q1": {"Mac\nSecond display"}, "q2": {"[REDACTED]"}}) || answers[0].ID != approval.ID {
					t.Fatalf("confirmed blocking answer projection = %#v", answers)
				}
			} else if len(answers) != 0 {
				t.Fatalf("text-free terminal resolution invented answers: %#v", answers)
			}
		})
	}
}
