package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/iaia/telegramgw/internal/protocol"
)

var ErrHistoryCursorUnavailable = errors.New("saved message cursor is no longer available")

// ConversationMessages reads only saved user messages and final Codex replies.
// The result is chronological, including Telegram and CLI input. It scans turns
// newest first and stops as soon as the requested suffix and one older message
// have been found, instead of loading an entire thread for a single last reply.
// Attachment content is projected to labels and never retained or executed.
func (c *Client) ConversationMessages(ctx context.Context, threadID string, limit int, before *protocol.HistoryCursor) ([]protocol.HistoryMessage, bool, error) {
	return c.conversationMessages(ctx, threadID, limit, before, maxHistoryTurnPages)
}

func (c *Client) conversationMessages(ctx context.Context, threadID string, limit int, before *protocol.HistoryCursor, maxPages int) ([]protocol.HistoryMessage, bool, error) {
	if strings.TrimSpace(threadID) == "" || (&protocol.HistoryRequest{Limit: limit, Before: before}).Validate() != nil {
		return nil, false, errors.New("invalid conversation history request")
	}
	result := make([]protocol.HistoryMessage, 0, limit)
	seenTurns, seenCursors := make(map[string]bool), make(map[string]bool)
	cursor, foundBefore := "", before == nil
	finish := func(older bool) ([]protocol.HistoryMessage, bool, error) {
		for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
			result[left], result[right] = result[right], result[left]
		}
		return result, older, nil
	}
	for page := 0; page < maxPages; page++ {
		params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "full"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var reply struct {
			Data       json.RawMessage `json:"data"`
			NextCursor string          `json:"nextCursor"`
		}
		if err := c.request(ctx, "thread/turns/list", params, &reply, false); err != nil {
			return nil, false, err
		}
		turns, err := historyArray(reply.Data)
		if err != nil || len(turns) > 1 || (len(turns) == 0 && reply.NextCursor != "") {
			return nil, false, fmt.Errorf("invalid conversation turn page: %w", ErrHistoryUnavailable)
		}
		for _, raw := range turns {
			var turn struct {
				ID        string `json:"id"`
				ItemsView string `json:"itemsView"`
			}
			if json.Unmarshal(raw, &turn) != nil || strings.TrimSpace(turn.ID) == "" || seenTurns[turn.ID] || (turn.ItemsView != "" && turn.ItemsView != "full") {
				return nil, false, fmt.Errorf("invalid conversation turn: %w", ErrHistoryUnavailable)
			}
			seenTurns[turn.ID] = true
			messages, err := decodeConversationTurn(raw)
			if err != nil {
				return nil, false, err
			}
			for i := len(messages) - 1; i >= 0; i-- {
				message := messages[i]
				if !foundBefore {
					if message.TurnID == before.TurnID && message.ItemID == before.ItemID {
						foundBefore = true
					}
					continue
				}
				if len(result) == limit {
					return finish(true)
				}
				result = append(result, message)
			}
		}
		if reply.NextCursor == "" {
			if !foundBefore {
				return nil, false, ErrHistoryCursorUnavailable
			}
			return finish(false)
		}
		if seenCursors[reply.NextCursor] {
			return nil, false, fmt.Errorf("repeated conversation cursor: %w", ErrHistoryUnavailable)
		}
		seenCursors[reply.NextCursor] = true
		cursor = reply.NextCursor
	}
	return nil, false, fmt.Errorf("conversation page limit exceeded: %w", ErrHistoryUnavailable)
}

func decodeConversationTurn(raw json.RawMessage) ([]protocol.HistoryMessage, error) {
	var turn struct {
		ID          string          `json:"id"`
		Items       json.RawMessage `json:"items"`
		StartedAt   *int64          `json:"startedAt"`
		CompletedAt *int64          `json:"completedAt"`
	}
	if !historyObject(raw) || json.Unmarshal(raw, &turn) != nil || strings.TrimSpace(turn.ID) == "" || len(turn.ID) > 512 {
		return nil, fmt.Errorf("invalid conversation turn: %w", ErrHistoryUnavailable)
	}
	items, err := historyArray(turn.Items)
	if err != nil {
		return nil, err
	}
	result := make([]protocol.HistoryMessage, 0)
	seen := make(map[string]bool)
	for _, rawItem := range items {
		var kind struct {
			Type string `json:"type"`
		}
		if !historyObject(rawItem) || json.Unmarshal(rawItem, &kind) != nil {
			return nil, fmt.Errorf("invalid conversation item: %w", ErrHistoryUnavailable)
		}
		if kind.Type != "userMessage" && kind.Type != "agentMessage" {
			continue
		}
		var item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Phase   string          `json:"phase"`
			Text    *string         `json:"text"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rawItem, &item) != nil {
			return nil, fmt.Errorf("invalid conversation item: %w", ErrHistoryUnavailable)
		}
		message := protocol.HistoryMessage{TurnID: turn.ID, ItemID: item.ID, Timestamp: historyTimestamp(turn.StartedAt)}
		switch item.Type {
		case "userMessage":
			message.Role = "user"
			message.Text, err = historyUserText(item.Content)
			if err != nil {
				return nil, err
			}
		case "agentMessage":
			// Older runtimes omit phase; newer ones explicitly distinguish
			// temporary commentary from the final answer.
			if item.Phase != "" && item.Phase != "final_answer" {
				continue
			}
			if item.Text == nil {
				return nil, fmt.Errorf("missing conversation text: %w", ErrHistoryUnavailable)
			}
			message.Role, message.Text = "assistant", *item.Text
			if completedAt := historyTimestamp(turn.CompletedAt); completedAt != nil {
				message.Timestamp = completedAt
			}
		default:
			continue
		}
		if strings.TrimSpace(item.ID) == "" || len(item.ID) > 512 || seen[item.ID] {
			return nil, fmt.Errorf("invalid conversation message identity: %w", ErrHistoryUnavailable)
		}
		seen[item.ID] = true
		if strings.TrimSpace(message.Text) != "" {
			result = append(result, message)
		}
	}
	return result, nil
}
