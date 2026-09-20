package gateway

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Sender) renderLastMessages(ctx context.Context, row registry.Delivery, event protocol.Event, page *protocol.HistoryPage, commandID string) ([]string, *TelegramKeyboard, error) {
	if page == nil || !page.Conversation || page.Limit < 1 || page.Limit > protocol.MaxHistoryLimit || len(page.Prompts) != 0 || len(page.Messages) > page.Limit {
		return nil, nil, errors.New("render messages: invalid page")
	}
	_, session, runtime, _, err := s.selectedIdentity(ctx, event.SessionID, event.RuntimeID)
	if err != nil {
		return nil, nil, err
	}
	label := sessionLabel(session)
	if len(page.Messages) == 0 {
		if page.Next != nil {
			return nil, nil, errors.New("render messages: empty page with older cursor")
		}
		return []string{"📜 " + label + " · No saved conversation messages to show."}, nil, nil
	}
	parts := make([]string, 0, len(page.Messages))
	total := 0
	seen := make(map[protocol.HistoryCursor]bool)
	for _, message := range page.Messages {
		cursor := protocol.HistoryCursor{TurnID: message.TurnID, ItemID: message.ItemID}
		length := utf8.RuneCountInString(message.Text)
		total += length
		if cursor.TurnID == "" || cursor.ItemID == "" || seen[cursor] || strings.TrimSpace(message.Text) == "" || !utf8.ValidString(message.Text) || length > 16000 || total > 64000 {
			return nil, nil, errors.New("render messages: invalid message")
		}
		seen[cursor] = true
		role := "👤 You"
		switch message.Role {
		case "user":
		case "assistant":
			role = "🤖 Codex"
		default:
			return nil, nil, errors.New("render messages: invalid role")
		}
		text := "📜 " + label + " · " + role + "\n" + historyTimestamp(message.Timestamp) + "\n\n" + message.Text
		if message.Truncated {
			text += "\n\n[Long message shortened in this history view.]"
		}
		parts = append(parts, text)
	}
	if page.Next == nil {
		return parts, nil, nil
	}
	first := page.Messages[0]
	if page.Next.TurnID != first.TurnID || page.Next.ItemID != first.ItemID {
		return nil, nil, errors.New("render messages: invalid older cursor")
	}
	keyboard, err := s.historyOlderKeyboard(ctx, row, event, session.ID, runtime.ID, commandID, &protocol.HistoryRequest{Limit: page.Limit, Before: page.Next, Messages: true})
	return parts, keyboard, err
}
