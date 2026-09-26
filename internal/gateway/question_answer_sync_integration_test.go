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

type questionSyncAPI struct {
	progressReplacementAPIFake
	live       map[int64]SendMessage
	editsCount int
}

func (a *questionSyncAPI) Send(_ context.Context, message SendMessage) (int64, error) {
	a.messages = append(a.messages, message)
	id := int64(len(a.messages))
	a.live[id] = message
	return id, nil
}

func (a *questionSyncAPI) Edit(_ context.Context, chat, id int64, text string, keyboard *TelegramKeyboard) error {
	a.editsCount++
	a.live[id] = SendMessage{ChatID: chat, Text: text, Keyboard: keyboard}
	return nil
}

func (a *questionSyncAPI) EditFormatted(_ context.Context, id int64, message SendMessage) error {
	a.editsCount++
	a.live[id] = message
	return nil
}

func (a *questionSyncAPI) DeleteMessage(ctx context.Context, chat, id int64) error {
	if err := a.progressAPIFake.DeleteMessage(ctx, chat, id); err != nil {
		return err
	}
	delete(a.live, id)
	return nil
}

// The native terminal answer arrives while another session is selected. It
// must update the original bot message without posting output from that session
// or moving the user's selection, including after an event/sender replay.
func TestTerminalQuestionAnswerEditsOriginalInBackgroundSession(t *testing.T) {
	ctx := t.Context()
	store := testRegistry(t)
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "question-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("test-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime, first, second := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	emit := func(session uuid.UUID, kind string, value any) protocol.Event {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		event := protocol.Event{ID: uuid.NewString(), Seq: sequence, WorkerID: worker.ID.String(), RuntimeID: runtime.String(), RuntimeGeneration: 1, SessionID: session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}
		if err := store.IngestEvent(ctx, worker.ID, connection, event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	for _, id := range []uuid.UUID{first, second} {
		emit(id, "session_discovered", protocol.Session{ID: id.String(), WorkerID: worker.ID.String(), RuntimeID: runtime.String(), ThreadID: id.String(), Name: "Question session", CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()})
	}
	api := &questionSyncAPI{live: make(map[int64]SendMessage)}
	options := SenderOptions{BotID: "bot", OwnerID: 10}
	sender := NewSender(store, api, nil, options)
	flush := func() {
		t.Helper()
		if err := sender.flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "select", Target: second.String()}); err != nil || result.ErrorCode != "" {
		t.Fatalf("select: %+v %v", result, err)
	}
	flush()
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:question", ThreadID: first.String(), TurnID: "question-turn", ItemID: "question", Async: true, Type: "user_input", State: "pending", Questions: []protocol.Question{{ID: "q1", Prompt: "Does the pointer disappear?", Options: []string{"Yes", "No"}}}}
	emit(first, "user_input_requested", approval)
	flush()
	messageID := int64(len(api.messages))
	if messageID != 2 || api.live[messageID].Keyboard == nil {
		t.Fatalf("question not delivered: %+v", api.live)
	}
	var textReplyToken string
	for _, row := range api.live[messageID].Keyboard.Rows {
		for _, button := range row {
			if button.Text == "Reply with text" {
				textReplyToken = strings.TrimPrefix(button.Data, "cb:")
			}
		}
	}
	if textReplyToken == "" {
		t.Fatal("question has no text-reply button")
	}
	if result, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 2, UserID: 10, ChatID: 20, CallbackToken: textReplyToken, CallbackMessageID: messageID}); err != nil || !result.TextReply {
		t.Fatalf("open text reply: %+v %v", result, err)
	}
	flush()
	helperID := int64(len(api.messages))
	if helperID != 3 || api.live[helperID].Keyboard == nil || !api.live[helperID].Keyboard.ForceReply {
		t.Fatalf("native reply helper not delivered: %+v", api.live)
	}
	answer := "Yes, while captured.\nControl+Option releases it."
	approval.Answers = map[string][]string{"q1": {answer}}
	approval.State = "answered"
	answered := emit(first, "user_input_answered", approval)
	approval.State = "cleared"
	emit(first, "approval_resolved", approval)
	// A sender restart must retain the queued edit and exact target.
	sender = NewSender(store, api, nil, options)
	flush()
	flush() // The sender leases one operation at a time: edit, then helper cleanup.
	got := api.live[messageID]
	want := "Question: Does the pointer disappear?\n\nAnswer: " + answer
	if got.Text != want || (got.Keyboard != nil && (got.Keyboard.ForceReply || len(got.Keyboard.Rows) > 0)) || api.editsCount != 1 || len(api.messages) != 3 {
		t.Fatalf("answer did not replace question: %+v; edits=%d sends=%d", got, api.editsCount, len(api.messages))
	}
	if _, remains := api.live[helperID]; remains || len(api.deleted) != 1 || api.deleted[0] != [2]int64{20, helperID} {
		t.Fatalf("answer did not remove only its temporary reply helper: live=%+v deleted=%v", api.live, api.deleted)
	}
	if err := store.IngestEvent(ctx, worker.ID, connection, answered); err != nil {
		t.Fatal(err)
	}
	flush()
	if api.editsCount != 1 || len(api.messages) != 3 || len(api.deleted) != 1 {
		t.Fatal("replayed answer duplicated an edit/deletion or posted a message")
	}
	if result, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 3, UserID: 10, ChatID: 20, Action: "status"}); err != nil || result.SessionID != second.String() {
		t.Fatalf("answer changed selection: %+v %v", result, err)
	}
}
