package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type answerProgressAPI struct {
	progressReplacementAPIFake
	live map[int64]SendMessage
}

func (a *answerProgressAPI) Send(_ context.Context, message SendMessage) (int64, error) {
	a.messages = append(a.messages, message)
	id := int64(len(a.messages))
	a.live[id] = message
	return id, nil
}

func (a *answerProgressAPI) EditFormatted(ctx context.Context, id int64, message SendMessage) error {
	if err := a.progressReplacementAPIFake.EditFormatted(ctx, id, message); err != nil {
		return err
	}
	a.live[id] = message
	return nil
}

func (a *answerProgressAPI) DeleteMessage(ctx context.Context, chat, id int64) error {
	if err := a.progressReplacementAPIFake.DeleteMessage(ctx, chat, id); err != nil {
		return err
	}
	delete(a.live, id)
	return nil
}

// Exercise the real durable registry with the sender: repositioning must create
// new Telegram messages below the answered question, then edit those new IDs.
func TestAnsweredQuestionRepostsProgressBelowNextQuestionAndKeepsLaterEdits(t *testing.T) {
	ctx := t.Context()
	store := testRegistry(t)
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "test-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("test-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime, session := uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{
		ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	event := func(kind string, payload any) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		if err := store.IngestEvent(ctx, worker.ID, connection, protocol.Event{
			ID: uuid.NewString(), Seq: sequence, WorkerID: worker.ID.String(), RuntimeID: runtime.String(), RuntimeGeneration: 1,
			SessionID: session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data,
		}); err != nil {
			t.Fatal(err)
		}
	}
	event("session_discovered", protocol.Session{ID: session.String(), WorkerID: worker.ID.String(), RuntimeID: runtime.String(), ThreadID: "thread-progress", Name: "Test session", CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()})
	selected, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "select", Target: session.String()})
	if err != nil || selected.ErrorCode != "" {
		t.Fatalf("select session: %+v %v", selected, err)
	}
	api := &answerProgressAPI{live: map[int64]SendMessage{}}
	options := SenderOptions{BotID: "bot", OwnerID: 10}
	sender := NewSender(store, api, nil, options)
	flush := func() {
		t.Helper()
		if err := sender.flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	flush() // Connection acknowledgement precedes turn output.
	event("turn_started", protocol.Result{TurnID: "turn-progress"})
	event("agent_progress_message", protocol.Result{TurnID: "turn-progress", Text: "Inspecting settings"})
	flush()
	commentaryID := int64(len(api.messages))
	event("tool_progress_message", protocol.Result{TurnID: "turn-progress", Text: "cat settings.json"})
	flush()
	toolID := int64(len(api.messages))
	if commentaryID == toolID {
		t.Fatal("commentary and tools did not occupy independent messages")
	}
	event("user_input_requested", protocol.Approval{
		ID: uuid.NewString(), RequestID: "ask", ThreadID: "thread-progress", TurnID: "turn-progress", Type: "user_input",
		Questions: []protocol.Question{{ID: "system", Prompt: "Which system?"}, {ID: "equipment", Prompt: "Which equipment?"}},
	})
	flush()
	questionID := int64(len(api.messages))
	if !strings.Contains(api.live[questionID].Text, "Which system?") {
		t.Fatalf("question not sent: %+v", api.live[questionID])
	}
	accepted, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 2, UserID: 10, ChatID: 20, ReplyToMessageID: questionID, Text: "Linux"})
	if err != nil || accepted.ErrorCode != "" || !accepted.ProgressReposition {
		t.Fatalf("answer was not accepted: %+v %v", accepted, err)
	}
	flush()
	flush() // The answered question is edited as a separate durable delivery.
	nextQuestionID := int64(len(api.messages))
	if nextQuestionID <= questionID || !strings.Contains(api.live[nextQuestionID].Text, "Which equipment?") {
		t.Fatalf("next question missing: %+v", api.live[nextQuestionID])
	}
	if answered := api.live[questionID]; answered.Text != "Question: Which system?\n\nAnswer: Linux" || answered.Keyboard == nil || len(answered.Keyboard.Rows) != 0 {
		t.Fatalf("answered question was not simplified in place: %+v", answered)
	}
	flush()
	if int64(len(api.messages)) != nextQuestionID {
		t.Fatal("progress reposted before old messages were deleted")
	}
	for range 2 {
		if err := sender.flushDeletions(ctx, store, api); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.deleted) != 2 || api.live[commentaryID].Text != "" || api.live[toolID].Text != "" {
		t.Fatalf("old progress survived: deleted=%v", api.deleted)
	}
	// Restarting the sender must not lose the reposted slots or their formatting.
	sender = NewSender(store, api, nil, options)
	flush()
	flush()
	if len(api.messages) != int(nextQuestionID)+2 || len(api.edits) != 1 || api.editIDs[0] != questionID {
		t.Fatalf("progress was edited above the question instead of reposted: sends=%d edits=%d", len(api.messages), len(api.edits))
	}
	newIDs := map[string]int64{}
	for id := nextQuestionID + 1; id <= int64(len(api.messages)); id++ {
		message := api.live[id]
		if !message.DisableNotification {
			t.Fatal("restored progress sent a notification")
		}
		if strings.Contains(message.Text, "cat settings.json") {
			newIDs["tool_progress_message"] = id
			if len(message.Entities) != 1 || message.Entities[0].Type != "pre" || message.Entities[0].Length != telegramTextLength(message.Text) {
				t.Fatalf("restored tool lost monospace: %+v", message)
			}
		} else if strings.Contains(message.Text, "Inspecting settings") {
			newIDs["agent_progress_message"] = id
		} else {
			t.Fatalf("unexpected restored progress: %+v", message)
		}
	}
	if len(newIDs) != 2 || api.live[questionID].Text == "" || api.live[nextQuestionID].Text == "" {
		t.Fatal("progress slots or retained questions were lost")
	}
	for _, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		previousEdits := len(api.editIDs)
		event(kind, protocol.Result{TurnID: "turn-progress", Text: "New " + kind})
		flush()
		if len(api.editIDs) != previousEdits+1 {
			t.Fatalf("%s did not edit its reposted message", kind)
		}
		if got := api.editIDs[len(api.editIDs)-1]; got != newIDs[kind] {
			t.Fatalf("%s edited message %d, want reposted %d", kind, got, newIDs[kind])
		}
	}
	if len(api.messages) != int(nextQuestionID)+2 || len(api.edits) != 3 {
		t.Fatal("live progress updates created more temporary messages")
	}
}
