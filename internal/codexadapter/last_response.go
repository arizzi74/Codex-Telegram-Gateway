package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
)

// LastAgentResponse reads items newest first so /copy never downloads an
// entire thread's accumulated inline images in a single RPC response.
func (c *Client) LastAgentResponse(ctx context.Context, threadID string) (string, error) {
	if threadID == "" {
		return "", errors.New("thread id is required")
	}
	cursor := ""
	seen := map[string]bool{}
	for range 1000 {
		params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": "desc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var reply struct {
			Data       json.RawMessage `json:"data"`
			NextCursor string          `json:"nextCursor"`
		}
		if err := c.request(ctx, "thread/items/list", params, &reply, false); err != nil {
			return "", err
		}
		entries, err := historyArray(reply.Data)
		if err != nil || len(entries) > 1 {
			return "", errors.New("invalid saved response page")
		}
		for _, raw := range entries {
			var entry struct {
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(raw, &entry) != nil || entry.Item.Type == "" {
				return "", errors.New("invalid saved response item")
			}
			if entry.Item.Type == "agentMessage" && entry.Item.Text != "" {
				return entry.Item.Text, nil
			}
		}
		if reply.NextCursor == "" {
			return "", nil
		}
		if seen[reply.NextCursor] {
			return "", errors.New("saved response repeated a page cursor")
		}
		seen[reply.NextCursor] = true
		cursor = reply.NextCursor
	}
	return "", ErrHistoryUnavailable
}
