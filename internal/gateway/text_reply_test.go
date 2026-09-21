package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func textReplyFixture(t *testing.T) (*renderStoreFake, registry.Delivery) {
	t.Helper()
	store := renderFixture()
	store.approval = protocol.Approval{ID: testApproval.String(), Async: true, Questions: []protocol.Question{
		{ID: "equipment", Prompt: "Which equipment is available?", Options: []string{"Mac", "Linux"}},
	}}
	row := uiRow(t, registry.AcceptResult{View: "input_prompt", TextReply: true, UserID: 42,
		SessionID: testSessionID.String(), RuntimeID: testRuntimeID.String(), ApprovalID: testApproval.String(), QuestionID: "equipment"})
	row.ChatID = 42
	return store, row
}

func TestTextReplyExplicitlyOpensPrivateComposerAndKeepsQuestionRoute(t *testing.T) {
	store, row := textReplyFixture(t)
	sender := testSender(store, nil)
	messages, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(messages) != 1 {
		t.Fatalf("render text reply: %d %v", len(messages), err)
	}
	message, err := sender.deliveryMessageForSend(context.Background(), row, messages[0])
	if err != nil || message.Keyboard == nil || !message.Keyboard.ForceReply || len(message.Keyboard.Rows) != 0 || len(store.callbacks) != 0 {
		t.Fatalf("button repeated instead of opening composer: %+v %v", message, err)
	}
	for _, want := range []string{"Answer for auth-fix", "Which equipment is available?", "message box below", "press Send", "choose Reply", "/tgquestions"} {
		if !strings.Contains(message.Text, want) {
			t.Fatalf("missing %q in %q", want, message.Text)
		}
	}
	session, _, approval := deliveryRoute(row)
	if session != testSessionID.String() || approval != testApproval.String() || deliveryQuestion(row) != "equipment" {
		t.Fatal("text composer lost its question route")
	}

	// Opening a request without choosing text must keep its answer buttons.
	var response registry.AcceptResult
	_ = json.Unmarshal(row.Payload, &response)
	response.TextReply = false
	row.Payload, _ = json.Marshal(response)
	_, markup, err := sender.render(context.Background(), row)
	if err != nil || markup == nil || markup.ForceReply || len(markup.Rows) != 4 {
		t.Fatalf("ordinary question stole composer focus: %+v %v", markup, err)
	}
}

func TestTextReplyGroupAndExpiredRequestDoNotForceComposer(t *testing.T) {
	store, row := textReplyFixture(t)
	row.ChatID = -10042
	text, markup, err := testSender(store, nil).render(context.Background(), row)
	if err != nil || markup != nil || !strings.Contains(text, "Long-press this message and choose Reply") {
		t.Fatalf("group text reply: %q %+v %v", text, markup, err)
	}
	row.ChatID = 42
	store.approvalErr = registry.ErrTelegramTarget
	text, markup, err = testSender(store, nil).render(context.Background(), row)
	if err != nil || markup != nil || !strings.Contains(text, "no longer pending") {
		t.Fatalf("expired question opened composer: %q %+v %v", text, markup, err)
	}
}

func TestQueuedQuestionAnsweredInTerminalDoesNotRetryOrRestoreControls(t *testing.T) {
	for _, view := range []string{"input_pending", "input_prompt"} {
		t.Run(view, func(t *testing.T) {
			store, row := textReplyFixture(t)
			// The equipment answer arrived after this UI delivery was queued;
			// only another question remains in the same pending request.
			store.approval.Questions = []protocol.Question{{ID: "display", Prompt: "Which display?", Options: []string{"One", "Two"}}}
			var response registry.AcceptResult
			if err := json.Unmarshal(row.Payload, &response); err != nil {
				t.Fatal(err)
			}
			response.View, response.TextReply = view, view == "input_prompt"
			row.Payload, _ = json.Marshal(response)
			sender := testSender(store, nil)
			messages, err := sender.renderDeliveryMessages(t.Context(), row)
			if err != nil || len(messages) != 1 {
				t.Fatalf("obsolete field cannot finish delivery: messages=%d err=%v", len(messages), err)
			}
			message, err := sender.deliveryMessageForSend(t.Context(), row, messages[0])
			if err != nil || message.Text != "This input request is no longer pending." || message.Keyboard != nil || len(store.callbacks) != 0 {
				t.Fatalf("obsolete field restored question controls: %+v, %v", message, err)
			}
			if session, _, approval := deliveryRoute(row); session != testSessionID.String() || approval != testApproval.String() || deliveryQuestion(row) != "equipment" {
				t.Fatal("obsolete question lost the original route needed by its answer edit")
			}
		})
	}
}

func TestLongTextReplyForcesOnlyLastChunkAndSurvivesSessionFormatting(t *testing.T) {
	store, row := textReplyFixture(t)
	store.approval.Questions[0].Prompt = strings.Repeat("Long question text. ", 500)
	sender := testSender(store, nil)
	messages, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(messages) < 2 {
		t.Fatalf("long question: %d %v", len(messages), err)
	}
	for i, raw := range messages {
		var message SendMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		forced := message.Keyboard != nil && message.Keyboard.ForceReply
		if forced != (i == len(messages)-1) {
			t.Fatalf("wrong composer chunk %d: %+v", i, message)
		}
		formatted := formatSessionDeliveryMessage(sessionDeliveryMessage{SendMessage: message, SessionName: "Other session", SessionBody: message.Text}, true, row.Kind)
		if (formatted.Keyboard != nil && formatted.Keyboard.ForceReply) != forced || telegramTextLength(formatted.Text) > 4096 {
			t.Fatal("multisession formatting lost composer or exceeded Telegram limit")
		}
	}
}
