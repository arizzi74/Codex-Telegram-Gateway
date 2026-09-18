package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrHistoryUnavailable means the server returned thread metadata without the
// requested history. It differs from a valid, empty turns array.
var ErrHistoryUnavailable = errors.New("codex user-prompt history is unavailable")

// UserPrompt is a stored user message, not an instruction to execute. The
// turn/item pair identifies a prompt for stable history pagination.
type UserPrompt struct {
	TurnID string
	ItemID string
	Text   string
}

// UserPrompts reads stored user messages in chronological order without
// resuming a thread, subscribing to its events, or starting a model turn.
// Only explicit userMessage text is projected; attachment locations and all
// other items (including raw reasoning) remain outside the returned history.
func (c *Client) UserPrompts(ctx context.Context, threadID string) ([]UserPrompt, error) {
	thread, err := c.ReadThread(ctx, threadID, true)
	if err != nil {
		return nil, err
	}
	if thread.ID != threadID {
		return nil, errors.New("thread/read returned a different history thread")
	}
	return decodeUserPrompts(thread.Raw)
}

func decodeUserPrompts(raw json.RawMessage) ([]UserPrompt, error) {
	var thread struct {
		Turns json.RawMessage `json:"turns"`
	}
	if err := json.Unmarshal(raw, &thread); err != nil {
		return nil, errors.New("invalid user-prompt history thread")
	}
	turns, err := historyArray(thread.Turns)
	if err != nil {
		return nil, fmt.Errorf("read history turns: %w", err)
	}
	result := make([]UserPrompt, 0)
	seen := make(map[[2]string]struct{})
	for _, rawTurn := range turns {
		var turn struct {
			ID    string          `json:"id"`
			Items json.RawMessage `json:"items"`
		}
		if !historyObject(rawTurn) || json.Unmarshal(rawTurn, &turn) != nil {
			return nil, errors.New("invalid user-prompt history turn")
		}
		items, err := historyArray(turn.Items)
		if err != nil {
			return nil, fmt.Errorf("read history items: %w", err)
		}
		for _, rawItem := range items {
			var kind struct {
				Type string `json:"type"`
			}
			if !historyObject(rawItem) || json.Unmarshal(rawItem, &kind) != nil {
				return nil, errors.New("invalid user-prompt history item")
			}
			if kind.Type != "userMessage" {
				continue
			}
			var item struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
			}
			if json.Unmarshal(rawItem, &item) != nil {
				return nil, errors.New("invalid user-prompt history user message")
			}
			if strings.TrimSpace(turn.ID) == "" || strings.TrimSpace(item.ID) == "" {
				return nil, errors.New("user-prompt history omitted a turn or item id")
			}
			key := [2]string{turn.ID, item.ID}
			if _, duplicate := seen[key]; duplicate {
				return nil, errors.New("user-prompt history repeated a turn and item id")
			}
			seen[key] = struct{}{}
			text, err := historyUserText(item.Content)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(text) != "" {
				result = append(result, UserPrompt{TurnID: turn.ID, ItemID: item.ID, Text: text})
			}
		}
	}
	return result, nil
}

func historyUserText(raw json.RawMessage) (string, error) {
	content, err := historyArray(raw)
	if err != nil {
		return "", fmt.Errorf("read history user content: %w", err)
	}
	var text strings.Builder
	for _, rawPart := range content {
		var part struct {
			Type string `json:"type"`
		}
		if !historyObject(rawPart) || json.Unmarshal(rawPart, &part) != nil {
			return "", errors.New("invalid user-prompt history content")
		}
		switch part.Type {
		case "text":
			var value struct {
				Text *string `json:"text"`
			}
			if json.Unmarshal(rawPart, &value) != nil || value.Text == nil {
				return "", errors.New("invalid user-prompt history text")
			}
			text.WriteString(*value.Text)
		case "image", "localImage":
			text.WriteString("[Image]")
		case "skill", "mention":
			text.WriteString("[Attachment]")
		}
	}
	return text.String(), nil
}

func historyArray(raw json.RawMessage) ([]json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, ErrHistoryUnavailable
	}
	if raw[0] != '[' {
		return nil, errors.New("invalid history array")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, errors.New("invalid history array")
	}
	return values, nil
}

func historyObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}
