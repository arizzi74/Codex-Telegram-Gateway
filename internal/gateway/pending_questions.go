package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type pendingQuestionStore interface {
	ListPendingQuestions(context.Context, string, int64, int64, int64, int) (registry.PendingQuestionsPage, error)
}

func (s *Sender) renderPendingQuestions(ctx context.Context, row registry.Delivery, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	store, ok := s.store.(pendingQuestionStore)
	if !ok {
		return "", nil, errors.New("render pending questions: registry is unavailable")
	}
	page, err := store.ListPendingQuestions(ctx, s.options.BotID, response.UserID, row.ChatID, row.TopicID, response.SessionPage)
	if err != nil {
		return "", nil, err
	}
	var text strings.Builder
	text.WriteString("Pending questions and approvals · all sessions")
	keyboard := &TelegramKeyboard{}
	if len(page.Requests) == 0 {
		text.WriteString("\n\nNo pending requests on this page. Questions already answered or no longer active are omitted.")
	}
	for i, item := range page.Requests {
		kind := "Approval"
		if len(item.Approval.Questions) > 0 {
			kind = "Question"
		}
		number := response.SessionPage*registry.PendingQuestionsPageSize + i + 1
		fmt.Fprintf(&text, "\n\n%d. %s\n%s", number, item.SessionName, kind)
		sessionID, err := requiredUUID("session", item.SessionID)
		if err != nil {
			return "", nil, err
		}
		runtimeID, err := requiredUUID("runtime", item.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		approvalID, err := requiredUUID("approval", item.Approval.ID)
		if err != nil {
			return "", nil, err
		}
		token, err := s.callback(ctx, row, registry.Callback{Action: "question_open", UserID: response.UserID, SessionID: sessionID, RuntimeID: runtimeID, ApprovalID: approvalID, Generation: item.Generation})
		if err != nil {
			return "", nil, err
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: fmt.Sprintf("Open %d · %s", number, kind), Data: token}})
	}
	var navigation []TelegramButton
	for _, entry := range []struct {
		label string
		page  int
		show  bool
	}{{"Previous", response.SessionPage - 1, response.SessionPage > 0}, {"Refresh", response.SessionPage, true}, {"Next", response.SessionPage + 1, page.HasMore}} {
		if !entry.show {
			continue
		}
		token, err := s.callback(ctx, row, registry.Callback{Action: "questions", UserID: response.UserID, SessionPage: entry.page})
		if err != nil {
			return "", nil, err
		}
		navigation = append(navigation, TelegramButton{Text: entry.label, Data: token})
	}
	keyboard.Rows = append(keyboard.Rows, navigation)
	text.WriteString("\n\nOpen a request to answer it. Your selected session stays unchanged.")
	return text.String(), keyboard, nil
}

func (s *Sender) renderPendingApproval(ctx context.Context, row registry.Delivery, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	approvalID, err := requiredUUID("approval", response.ApprovalID)
	if err != nil {
		return "", nil, err
	}
	store, ok := s.store.(telegramRenderStore)
	if !ok {
		return "", nil, errors.New("render pending approval: registry is unavailable")
	}
	approval, err := store.PendingApproval(ctx, approvalID)
	if errors.Is(err, registry.ErrTelegramTarget) {
		return "This approval request is no longer pending.", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	_, session, runtime, worker, err := s.selectedIdentity(ctx, response.SessionID, response.RuntimeID)
	if err != nil {
		return "", nil, err
	}
	event := protocol.Event{SessionID: session.ID, RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation}
	return s.renderApproval(ctx, row, humanIdentity(worker, runtime, session)+"\nSession: "+s.sessionListLabel(session), event, approval)
}
