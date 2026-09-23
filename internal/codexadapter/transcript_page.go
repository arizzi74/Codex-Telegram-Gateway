package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// TranscriptTurnPage is the legacy one-turn source for the bounded Web UI
// transcript. Public-summary filtering and display limits are applied by the
// worker before any item is cached or sent to a browser.
type TranscriptTurnPage struct {
	Data       []TranscriptTurn `json:"data"`
	NextCursor string           `json:"nextCursor"`
}

type TranscriptTurn struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	ItemsView   string            `json:"itemsView"`
	Items       []json.RawMessage `json:"items"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
}

// ReadTranscriptTurnPage requests at most one source turn. Official runtimes
// without thread/items/list can only bound history at this granularity; the
// existing native transport frame ceiling still applies to the cold read.
func (c *Client) ReadTranscriptTurnPage(ctx context.Context, threadID, cursor, direction string, full bool) (TranscriptTurnPage, error) {
	if strings.TrimSpace(threadID) == "" || len(threadID) > 512 || len(cursor) > 4096 || (direction != "asc" && direction != "desc") {
		return TranscriptTurnPage{}, errors.New("invalid transcript page request")
	}
	view := "notLoaded"
	if full {
		view = "full"
	}
	params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": direction, "itemsView": view}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var page TranscriptTurnPage
	if err := c.request(ctx, "thread/turns/list", params, &page, false); err != nil {
		var rpc *RPCError
		if cursor == "" && errors.As(err, &rpc) && rpc.Code == -32600 && strings.Contains(rpc.Message, "thread "+threadID+" is not materialized yet") && strings.Contains(rpc.Message, "thread/turns/list is unavailable before first user message") {
			return TranscriptTurnPage{Data: []TranscriptTurn{}}, nil
		}
		return TranscriptTurnPage{}, err
	}
	if page.Data == nil || len(page.Data) > 1 || len(page.NextCursor) > 4096 || (cursor != "" && cursor == page.NextCursor) || (len(page.Data) == 0 && page.NextCursor != "") {
		return TranscriptTurnPage{}, ErrHistoryUnavailable
	}
	for _, turn := range page.Data {
		if turn.ID == "" || len(turn.ID) > 512 || (turn.ItemsView != "" && turn.ItemsView != view) {
			return TranscriptTurnPage{}, ErrHistoryUnavailable
		}
		switch turn.Status {
		case "inProgress", "completed", "interrupted", "failed":
		default:
			return TranscriptTurnPage{}, ErrHistoryUnavailable
		}
		if !full && len(turn.Items) > 0 {
			return TranscriptTurnPage{}, ErrHistoryUnavailable
		}
	}
	return page, nil
}
