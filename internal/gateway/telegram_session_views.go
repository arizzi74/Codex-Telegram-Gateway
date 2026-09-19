package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/iaia/telegramgw/internal/registry"
)

var errSessionDeliverySuppressed = errors.New("Telegram session delivery is no longer visible")

type telegramSessionVisibilityStore interface {
	SuppressTelegramDelivery(context.Context, string) (bool, error)
	SkipDelivery(context.Context, string) error
}

type telegramSessionPresentationStore interface {
	TelegramSessionPresentation(context.Context, string, int64, int64, string) (registry.TelegramSessionPresentation, error)
}

type telegramSessionAliasesStore interface {
	ListTelegramSessionAliases(context.Context, string, int64, int64, int64) ([]registry.TelegramSessionAlias, error)
}

// These extra fields belong to our durable checkpoint, never the Telegram API
// request. Reserving the header's space even in single-session mode lets queued
// chunks acquire their session label when the user turns multisession on.
type sessionDeliveryMessage struct {
	SendMessage
	SessionName   string `json:"_session_name,omitempty"`
	SessionMarker string `json:"_session_marker,omitempty"`
	SessionBody   string `json:"_session_body,omitempty"`
}

func (s *Sender) skipInvisibleDelivery(ctx context.Context, row registry.Delivery) (bool, error) {
	if store, ok := s.store.(telegramSessionVisibilityStore); ok {
		skip, err := store.SuppressTelegramDelivery(ctx, row.ID)
		if err != nil || !skip {
			return skip, err
		}
		return true, store.SkipDelivery(ctx, row.ID)
	}
	return s.skipProgress(ctx, row)
}

func (s *Sender) deliveryPresentation(ctx context.Context, row registry.Delivery) (registry.TelegramSessionPresentation, error) {
	store, ok := s.store.(telegramSessionPresentationStore)
	if !ok {
		return registry.TelegramSessionPresentation{}, nil
	}
	session, _, _ := deliveryRoute(row)
	if session == "" {
		return registry.TelegramSessionPresentation{}, nil
	}
	bot := row.BotID
	if bot == "" {
		bot = s.options.BotID
	}
	presentation, err := store.TelegramSessionPresentation(ctx, bot, row.ChatID, row.TopicID, session)
	if err != nil {
		return presentation, err
	}
	if s.options.Redactor != nil {
		presentation.Name = s.options.Redactor.Redact(presentation.Name)
	}
	// A session imported from Codex may use an arbitrarily long first-prompt
	// preview as its name. Keep ordinary names intact, but leave room for the
	// actual message when a preview exceeds half Telegram's message budget.
	if telegramTextLength(presentation.Name) > 2000 {
		remaining := 1999
		for index, r := range presentation.Name {
			remaining -= utf16.RuneLen(r)
			if remaining < 0 {
				presentation.Name = presentation.Name[:index] + "…"
				break
			}
		}
	}
	return presentation, nil
}

func sessionMessageHeader(name, marker string) string {
	return strings.TrimSpace(marker + " " + name)
}

func sessionDeliveryParts(parts []string, presentation registry.TelegramSessionPresentation, progress bool) ([]string, error) {
	if presentation.Name == "" {
		return parts, nil
	}
	limit := 4000 - telegramTextLength(sessionMessageHeader(presentation.Name, presentation.Marker)) - 2
	if limit < 128 {
		return nil, errors.New("Telegram session name is too long for a message header")
	}
	var chunks []string
	for _, part := range parts {
		if progress {
			chunks = append(chunks, compactProgressLimit(part, limit))
		} else {
			chunks = append(chunks, SplitText(part, limit)...)
		}
	}
	return chunks, nil
}

func formatSessionDeliveryMessage(checkpoint sessionDeliveryMessage, multiSession bool, kind string) SendMessage {
	message := checkpoint.SendMessage
	message.Text = checkpoint.SessionBody
	message.Entities = nil
	headerLength := 0
	if multiSession {
		header := sessionMessageHeader(checkpoint.SessionName, checkpoint.SessionMarker)
		message.Text = header + "\n\n" + checkpoint.SessionBody
		headerLength = telegramTextLength(header)
		message.Entities = append(message.Entities, TelegramEntity{Type: "bold", Length: headerLength})
		headerLength += 2
	}
	if kind == "tool_progress_message" {
		message.Entities = append(message.Entities, TelegramEntity{Type: "pre", Offset: headerLength, Length: telegramTextLength(checkpoint.SessionBody)})
	}
	return message
}

func (s *Sender) deliveryMessageForSend(ctx context.Context, row registry.Delivery, raw json.RawMessage) (SendMessage, error) {
	var checkpoint sessionDeliveryMessage
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return SendMessage{}, err
	}
	if checkpoint.SessionName == "" {
		return checkpoint.SendMessage, nil
	}
	presentation, err := s.deliveryPresentation(ctx, row)
	if err != nil {
		return SendMessage{}, err
	}
	return formatSessionDeliveryMessage(checkpoint, presentation.MultiSession, row.Kind), nil
}

func (s *Sender) renderMultiSession(ctx context.Context, row registry.Delivery, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	if !response.MultiSession {
		return "Multisession mode is off. Messages and typing now follow your selected session. Questions from other sessions can still be answered here without changing your selection.", nil, nil
	}
	text := "Multisession mode is on. Messages from all sessions begin with their session name and a stable colored marker. Telegram does not support choosing a different text background color for each session.\n\nUse /_<session_alias> <message> to send to that session without changing your selection. Send the command alone to select it. Ordinary messages use your selected session."
	store, ok := s.store.(telegramSessionAliasesStore)
	if !ok {
		return text, nil, nil
	}
	user := response.UserID
	if user == 0 {
		user = s.options.OwnerID
	}
	bot := row.BotID
	if bot == "" {
		bot = s.options.BotID
	}
	aliases, err := store.ListTelegramSessionAliases(ctx, bot, user, row.ChatID, row.TopicID)
	if err != nil {
		return "", nil, err
	}
	for _, alias := range aliases {
		text += fmt.Sprintf("\n\n/%s — %s", alias.Alias, alias.Name)
	}
	return text, nil, nil
}
