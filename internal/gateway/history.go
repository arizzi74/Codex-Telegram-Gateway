package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Keep saved prompts in separate bot messages so each can be read, copied, or
// replied to independently. They use ordinary checkpointed delivery; none are
// passed to the inbound command parser or temporary-progress cleanup.
func (s *Sender) renderDeliveryParts(ctx context.Context, row registry.Delivery) ([]string, *TelegramKeyboard, error) {
	var event protocol.Event
	var result protocol.Result
	if row.Kind == "command_completed" && json.Unmarshal(row.Payload, &event) == nil &&
		event.Kind == row.Kind && json.Unmarshal(event.Data, &result) == nil && result.History != nil {
		parts, keyboard, err := s.renderHistory(ctx, row, event, result.History, result.CommandID)
		if err != nil {
			return nil, nil, err
		}
		var chunks []string
		for _, part := range parts {
			if s.options.Redactor != nil {
				part = s.options.Redactor.Redact(part)
			}
			chunks = append(chunks, SplitText(part, 4000)...)
		}
		return chunks, keyboard, nil
	}
	text, keyboard, err := s.render(ctx, row)
	if err != nil || text == "" {
		return nil, keyboard, err
	}
	if isProgressDelivery(row.Kind) {
		return []string{compactProgress(text)}, nil, nil
	}
	return SplitText(text, 4000), keyboard, nil
}

func (s *Sender) renderHistory(ctx context.Context, row registry.Delivery, event protocol.Event, page *protocol.HistoryPage, commandID string) ([]string, *TelegramKeyboard, error) {
	if page != nil && page.Conversation {
		return s.renderLastMessages(ctx, row, event, page, commandID)
	}
	if page == nil || page.Limit < 1 || page.Limit > protocol.MaxHistoryLimit || len(page.Prompts) > page.Limit {
		return nil, nil, errors.New("render history: invalid page")
	}
	_, session, runtime, _, err := s.selectedIdentity(ctx, event.SessionID, event.RuntimeID)
	if err != nil {
		return nil, nil, err
	}
	header := "📜 Prompt history · " + sessionLabel(session)
	if len(page.Prompts) == 0 {
		if page.Next != nil {
			return nil, nil, errors.New("render history: empty page with older cursor")
		}
		return []string{header + "\n\nNo saved Codex prompts to show. Prompts already recorded through Telegram are omitted."}, nil, nil
	}
	parts := []string{header + "\n\nSaved prompts, oldest first within this page. Prompts already recorded through Telegram are omitted."}
	for _, prompt := range page.Prompts {
		if prompt.TurnID == "" || prompt.ItemID == "" || strings.TrimSpace(prompt.Text) == "" {
			return nil, nil, errors.New("render history: invalid prompt")
		}
		text := "👤 You · Codex\n\n" + prompt.Text
		if prompt.Truncated {
			text += "\n\n[Long prompt shortened in this history view.]"
		}
		parts = append(parts, text)
	}
	if page.Next == nil {
		return parts, nil, nil
	}
	keyboard, err := s.historyOlderKeyboard(ctx, row, event, session.ID, runtime.ID, commandID, &protocol.HistoryRequest{Limit: page.Limit, Before: page.Next})
	return parts, keyboard, err
}

func (s *Sender) historyOlderKeyboard(ctx context.Context, row registry.Delivery, event protocol.Event, session, runtime, commandID string, request *protocol.HistoryRequest) (*TelegramKeyboard, error) {
	if err := request.Validate(); err != nil {
		return nil, errors.New("render history: invalid older cursor")
	}
	sessionID, err := requiredUUID("session", session)
	if err != nil {
		return nil, err
	}
	runtimeID, err := requiredUUID("runtime", runtime)
	if err != nil {
		return nil, err
	}
	store, ok := s.store.(interface {
		HistoryRequester(context.Context, uuid.UUID) (int64, error)
	})
	if !ok {
		return nil, errors.New("render history: requester lookup unavailable")
	}
	id, err := requiredUUID("command", commandID)
	if err != nil {
		return nil, err
	}
	requester, err := store.HistoryRequester(ctx, id)
	if err != nil || requester == 0 {
		return nil, errors.New("render history: requester unavailable")
	}
	token, err := s.callback(ctx, row, registry.Callback{Action: "history", UserID: requester, SessionID: sessionID, RuntimeID: runtimeID, Generation: int64(event.RuntimeGeneration), History: request})
	if err != nil {
		return nil, err
	}
	label := "Older prompts"
	if request.Messages {
		label = "Older messages"
	}
	return &TelegramKeyboard{Rows: [][]TelegramButton{{{Text: label, Data: token}}}}, nil
}
