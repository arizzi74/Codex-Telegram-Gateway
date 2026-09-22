package worker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const testPrivatePattern = `PRIVATE_[A-Z0-9]+`

func TestOutboundMetadataRedactsBeforeStorageWithoutMutatingLocalSession(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t, testPrivatePattern)
	defer cleanup()
	server.SetThreads([]map[string]any{{"id": "thread-private", "name": "PRIVATE_NAME", "preview": "PRIVATE_PREVIEW", "cwd": runtime.DefaultCWD, "source": "cli", "status": "idle"}}, nil)
	client, _, _ := a.manager.Client(runtime.ID)
	if err := a.manager.discover(a.ctx, runtime, client); err != nil {
		t.Fatal(err)
	}
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("local sessions = %#v, %v", sessions, err)
	}
	session := sessions[0]
	if session.Name != "PRIVATE_NAME" || session.Preview != "PRIVATE_PREVIEW" || session.CWD != runtime.DefaultCWD {
		t.Fatalf("local execution state changed: %#v", session)
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) == 0 {
		t.Fatalf("outbox = %#v, %v", events, err)
	}
	for _, event := range events {
		assertNoPrivateText(t, event.Data)
	}
	var published protocol.Session
	if err := json.Unmarshal(events[0].Data, &published); err != nil || published.Name != "[REDACTED]" || published.Preview != "[REDACTED]" || published.CWD != session.CWD || published.ID != session.ID || published.ThreadID != session.ThreadID {
		t.Fatalf("published session = %#v, %v", published, err)
	}
	// The same boundary covers nested command results and error messages.
	c := agentCommand(runtime, session, protocol.StartTurn)
	if _, err := a.store.Receive(c); err != nil {
		t.Fatal(err)
	}
	result := &protocol.Result{Session: &session, Text: "PRIVATE_TEXT", Error: &protocol.Error{Code: "stable_code", Message: "PRIVATE_ERROR"}}
	event, err := a.record(c, CommandFailed, result, "command_failed")
	if err != nil {
		t.Fatal(err)
	}
	assertNoPrivateText(t, event.Data)
	if session.Name != "PRIVATE_NAME" || result.Text != "PRIVATE_TEXT" {
		t.Fatal("redaction mutated the caller's local state")
	}
	// Path and opaque identifier fields are routing inputs, even if a pattern
	// happens to match them. They are never rewritten into invalid commands.
	routing, _ := json.Marshal(protocol.Session{ID: "PRIVATE_ID", ThreadID: "PRIVATE_THREAD", CWD: "/PRIVATE_PATH", Name: "PRIVATE_NAME"})
	routing, err = redactOutboundJSON(a.redactor, routing)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(routing, &published); err != nil || published.ID != "PRIVATE_ID" || published.ThreadID != "PRIVATE_THREAD" || published.CWD != "/PRIVATE_PATH" || published.Name != "[REDACTED]" {
		t.Fatalf("routing fields corrupted: %#v, %v", published, err)
	}
}

func TestBlockingRedactedDuplicateOptionsRouteOriginalAnswer(t *testing.T) {
	for selected := range 3 {
		t.Run(string(rune('A'+selected)), func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t, testPrivatePattern)
			defer cleanup()
			session := installSession(a, runtime, "blocking-redaction", "turn-live")
			rawOptions := []string{"PRIVATE_FIRST", "PRIVATE_SECOND", "[REDACTED]"}
			var options []map[string]any
			for _, label := range rawOptions {
				options = append(options, map[string]any{"label": label, "description": "PRIVATE_DESCRIPTION"})
			}
			if err := server.Request("item/tool/requestUserInput", 74, map[string]any{"threadId": session.ThreadID, "turnId": "turn-live", "itemId": "input-item", "questions": []map[string]any{{"id": "q1", "question": "PRIVATE_PROMPT", "header": "PRIVATE_HEADER", "options": options}}}); err != nil {
				t.Fatal(err)
			}
			var request codexadapter.Request
			select {
			case request = <-mustClient(t, a, runtime).Requests():
			case <-time.After(time.Second):
				t.Fatal("input request not received")
			}
			a.onRequest(runtime, request)
			approval := waitPublishedQuestion(t, a.store)
			labels := approval.Questions[0].Options
			if len(labels) != 3 || labels[0] == labels[1] || labels[1] == labels[2] {
				t.Fatalf("choices became ambiguous: %#v", labels)
			}
			command := agentCommand(runtime, session, protocol.InputResponse)
			command.Arguments = protocol.Arguments{ApprovalID: approval.ID, RequestID: request.RequestID, Answers: map[string][]string{"q1": {labels[selected]}}}
			waitAsyncCommand(t, a, command, CommandCompleted)
			waitFor(t, func() bool { return len(server.Responses()) == 1 })
			var response struct {
				Answers map[string]struct {
					Answers []string `json:"answers"`
				} `json:"answers"`
			}
			if err := json.Unmarshal(server.Responses()[0].Result, &response); err != nil || !reflect.DeepEqual(response.Answers["q1"].Answers, []string{rawOptions[selected]}) {
				t.Fatalf("wrong native answer: %s, %v", server.Responses()[0].Result, err)
			}
			events, _ := a.store.OutboxAfter(0)
			for _, event := range events {
				assertNoPrivateText(t, event.Data)
			}
		})
	}
}

func TestRecoveredAsyncRedactedChoiceRoutesOriginalPromptAndAnswer(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t, testPrivatePattern)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "redacted")
	approval.Summary = "PRIVATE_SUMMARY"
	approval.Questions = []protocol.Question{{ID: "q1", Prompt: "PRIVATE_PROMPT", Header: "PRIVATE_HEADER", Options: []string{"PRIVATE_FIRST", "PRIVATE_SECOND"}}}
	actor.rememberAsyncQuestion(approval)
	// Recover exclusively from the private local record after discarding the
	// actor's in-memory question. A generation change publishes a new identity.
	actor.asyncQuestions = nil
	actor.runtime.Generation++
	client, _, _ := a.manager.Client(runtime.ID)
	a.manager.install(actor.runtime, client)
	actor.recoverAsyncQuestions()
	approval = actor.asyncQuestions[approval.RequestID]
	if len(approval.Questions) != 1 || approval.Questions[0].Prompt != "PRIVATE_PROMPT" {
		t.Fatalf("raw recovery lost: %#v", approval)
	}
	published := waitPublishedQuestion(t, a.store)
	command := asyncAnswerCommand(actor.runtime, actor.session, approval)
	command.Arguments.Answers["q1"] = []string{published.Questions[0].Options[1]}
	if _, err := a.store.Receive(command); err != nil {
		t.Fatal(err)
	}
	actor.answerAsyncQuestion(command, client, approval)
	found := false
	for _, call := range server.Calls() {
		if call.Method == "turn/start" && strings.Contains(string(call.Params), "PRIVATE_PROMPT") && strings.Contains(string(call.Params), "PRIVATE_SECOND") {
			found = true
		}
	}
	if !found {
		t.Fatal("async answer did not reach the native session with its original prompt and chosen value")
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		assertNoPrivateText(t, event.Data)
	}
}

func TestExistingOutboxIsSanitizedWithoutLosingReplayIdentity(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	data, _ := json.Marshal(protocol.Approval{ID: uuid.NewString(), Questions: []protocol.Question{{ID: "q1", Prompt: "PRIVATE_PROMPT", Options: []string{"PRIVATE_FIRST", "PRIVATE_SECOND"}}}})
	before, err := store.AppendEvent(protocol.Event{Kind: "user_input_requested", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	redactor, _ := newWorkerRedactor([]string{testPrivatePattern})
	for range 2 {
		if err := store.configureRedactor(redactor); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].ID != before.ID || events[0].Seq != before.Seq || !events[0].OccurredAt.Equal(before.OccurredAt) {
		t.Fatalf("replay identity changed: %#v, %v", events, err)
	}
	assertNoPrivateText(t, events[0].Data)
	var approval protocol.Approval
	_ = json.Unmarshal(events[0].Data, &approval)
	if !reflect.DeepEqual(approval.Questions[0].Options, []string{"[REDACTED] (option 1)", "[REDACTED] (option 2)"}) {
		t.Fatalf("repeat sanitization changed option mapping: %#v", approval)
	}
}

func TestConnectionMetadataAndAcknowledgementsAreRedacted(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t, testPrivatePattern)
	defer cleanup()
	c := &Connection{store: a.store}
	runtime.Name, runtime.CodexVersion = "PRIVATE_RUNTIME", "PRIVATE_VERSION"
	for _, payload := range []any{
		protocol.Hello{WorkerID: a.cfg.WorkerID, WorkerName: "PRIVATE_WORKER", Hostname: "PRIVATE_HOST", Runtimes: []protocol.Runtime{runtime}},
		protocol.Heartbeat{WorkerID: a.cfg.WorkerID, Runtimes: []protocol.Runtime{runtime}},
		protocol.CommandAck{CommandID: uuid.NewString(), Error: &protocol.Error{Code: "stable", Message: "PRIVATE_ERROR"}},
	} {
		writes := make(chan outbound, 1)
		done := make(chan error, 1)
		go func() { done <- c.sendRedactedEnvelope(context.Background(), writes, "hello", payload) }()
		request := <-writes
		request.done <- nil
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(request.envelope)
		assertNoPrivateText(t, wire)
	}
}

func waitPublishedQuestion(t *testing.T, store *Store) protocol.Approval {
	t.Helper()
	var approval protocol.Approval
	waitFor(t, func() bool {
		events, _ := store.OutboxAfter(0)
		for _, event := range events {
			if event.Kind == "user_input_requested" && json.Unmarshal(event.Data, &approval) == nil {
				assertNoPrivateText(t, event.Data)
				return true
			}
		}
		return false
	})
	return approval
}

func assertNoPrivateText(t *testing.T, data []byte) {
	t.Helper()
	if strings.Contains(string(data), "PRIVATE_") {
		t.Fatalf("private display text persisted/transmitted: %s", data)
	}
}
