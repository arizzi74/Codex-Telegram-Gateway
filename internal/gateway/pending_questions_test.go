package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type pendingQuestionsStoreFake struct {
	*renderStoreFake
	page              registry.PendingQuestionsPage
	user, chat, topic int64
}

func (f *pendingQuestionsStoreFake) ListPendingQuestions(_ context.Context, bot string, user, chat, topic int64, page int) (registry.PendingQuestionsPage, error) {
	f.user, f.chat, f.topic = user, chat, topic
	return f.page, nil
}

func TestRenderPendingQuestionsFullNamesBoundedControls(t *testing.T) {
	fullName := "A complete session name " + strings.Repeat("long name ", 20)
	store := &pendingQuestionsStoreFake{renderStoreFake: renderFixture(), page: registry.PendingQuestionsPage{HasMore: true, Requests: []registry.PendingQuestionRequest{
		{SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String(), Generation: 7, SessionName: fullName, Approval: protocol.Approval{ID: testApproval.String(), Questions: []protocol.Question{{ID: "q1", Prompt: "Choose?"}}}},
		{SessionID: testOtherSess.String(), RuntimeID: testOtherRun.String(), Generation: 2, SessionName: "Other session", Approval: protocol.Approval{ID: testApproval.String(), Decisions: []string{"accept", "decline"}}},
	}}}
	sender := NewSender(store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), SenderOptions{BotID: "bot", OwnerID: 42})
	payload, _ := json.Marshal(registry.AcceptResult{View: "questions", UserID: 99, SessionPage: 1})
	text, keyboard, err := sender.render(context.Background(), registry.Delivery{Kind: "ui_response", ChatID: 20, TopicID: 7, Payload: payload})
	if err != nil || !strings.Contains(text, fullName) || !strings.Contains(text, "9.") || !strings.Contains(text, "Approval") || keyboard == nil || len(keyboard.Rows) != 3 || len(keyboard.Rows[2]) != 3 {
		t.Fatalf("pending menu: %q %#v %v", text, keyboard, err)
	}
	if store.user != 99 || store.chat != 20 || store.topic != 7 {
		t.Fatalf("missing requester scope: %#v", store)
	}
	for _, callback := range store.callbacks {
		if callback.UserID != 99 || callback.ChatID != 20 || callback.TopicID != 7 {
			t.Fatalf("wrong callback scope: %#v", callback)
		}
	}
	if store.callbacks[0].Action != "question_open" || store.callbacks[0].SessionID != testSessionID || store.callbacks[0].Generation != 7 || store.callbacks[1].Generation != 2 {
		t.Fatalf("unfrozen question targets: %#v", store.callbacks)
	}
}

func TestRenderPendingQuestionsEmptyAndApprovalReplay(t *testing.T) {
	store := &pendingQuestionsStoreFake{renderStoreFake: renderFixture()}
	sender := NewSender(store, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), SenderOptions{BotID: "bot", OwnerID: 42})
	payload, _ := json.Marshal(registry.AcceptResult{View: "questions", UserID: 42})
	text, keyboard, err := sender.render(context.Background(), registry.Delivery{Kind: "ui_response", ChatID: 20, Payload: payload})
	if err != nil || !strings.Contains(text, "No pending requests") || keyboard == nil || len(keyboard.Rows) != 1 {
		t.Fatalf("empty pending list: %q %#v %v", text, keyboard, err)
	}
	store.approval = protocol.Approval{ID: testApproval.String(), Type: "command", Summary: "Run a command", Decisions: []string{"accept", "decline"}}
	payload, _ = json.Marshal(registry.AcceptResult{View: "approval_prompt", UserID: 42, ApprovalID: testApproval.String(), SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String()})
	text, keyboard, err = sender.render(context.Background(), registry.Delivery{Kind: "ui_response", ChatID: 20, Payload: payload})
	if err != nil || !strings.Contains(text, "Approval required") || !strings.Contains(text, "auth-fix") || keyboard == nil || len(keyboard.Rows) != 2 {
		t.Fatalf("approval replay: %q %#v %v", text, keyboard, err)
	}
	store.approvalErr = registry.ErrTelegramTarget
	text, keyboard, err = sender.render(context.Background(), registry.Delivery{Kind: "ui_response", ChatID: 20, Payload: payload})
	if err != nil || !strings.Contains(text, "no longer pending") || keyboard != nil {
		t.Fatalf("resolved approval replay: %q %#v %v", text, keyboard, err)
	}
}

func TestRenderPendingAsyncQuestionOffersDismiss(t *testing.T) {
	store := renderFixture()
	store.approval = protocol.Approval{ID: testApproval.String(), Async: true, Questions: []protocol.Question{{ID: "q1", Prompt: "Choose one?", Options: []string{"One", "Two"}}}}
	payload, _ := json.Marshal(registry.AcceptResult{View: "input_prompt", UserID: 99, ApprovalID: testApproval.String(), SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String(), QuestionID: "q1"})
	text, keyboard, err := testSender(store, nil).render(context.Background(), registry.Delivery{Kind: "ui_response", ChatID: 20, Payload: payload})
	if err != nil || !strings.Contains(text, "without sending an answer") || keyboard == nil || len(keyboard.Rows) != 4 || keyboard.Rows[3][0].Text != "Dismiss question" {
		t.Fatalf("async dismissal UI: %q %#v %v", text, keyboard, err)
	}
	callback := store.callbacks[3]
	if callback.Action != "dismiss_input" || callback.SessionID != testSessionID || callback.ApprovalID != testApproval || callback.Generation != 7 || callback.UserID != 99 {
		t.Fatalf("unfrozen dismissal callback: %#v", callback)
	}
}
