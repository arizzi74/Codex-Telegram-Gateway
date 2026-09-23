package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func (r *webUIRelay) history(rpc webUIRPC) error {
	r.mu.Lock()
	request, ok := r.itemRequests[webUIID(rpc.ID)]
	delete(r.itemRequests, webUIID(rpc.ID))
	delete(r.pending, webUIID(rpc.ID))
	r.mu.Unlock()
	if !ok {
		return errors.New("missing conversation request")
	}
	raw, err := r.historyResult(rpc.ID, request)
	if err != nil {
		return err
	}
	raw, err = redactWebUIJSON(r.pool.c.store.redactor.Load(), raw)
	if err != nil {
		return err
	}
	if len(raw) > webUIHistoryPageBytes {
		return errors.New("conversation frame exceeds display limit")
	}
	return r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: raw})
}

func (r *webUIRelay) historyResult(id json.RawMessage, request webUIHistoryRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	result, err := r.pool.c.historyWebUI(ctx, r.runtime, r.session, request)
	reply := webUIRPC{ID: id, Result: result}
	if err != nil {
		message := "Conversation could not be loaded. Reconnect to refresh it."
		var validation *CodexCommandValidationError
		if errors.As(err, &validation) {
			message = safeErrorMessage(r.pool.c.store.redactor.Load(), validation.Message, message)
		}
		reply.Result = nil
		reply.Error, _ = json.Marshal(map[string]any{"code": -32000, "message": message})
	}
	return json.Marshal(reply)
}

// Native item pages count individual transcript entries across all turns. A
// twenty-turn page can contain thousands of tool/reasoning/message entries.
// Keep the native cursor contract and item timestamps without synthesizing any
// dates or loading complete turns to attach this lightweight browser view.
func webUIItemPage(raw json.RawMessage, redactors ...*auth.Redactor) (json.RawMessage, error) {
	var page struct {
		Data            []webUIHistoryEntry `json:"data"`
		NextCursor      *string             `json:"nextCursor,omitempty"`
		BackwardsCursor *string             `json:"backwardsCursor,omitempty"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Data == nil || len(page.Data) > 20 {
		return nil, errors.New("Codex returned an invalid or oversized conversation page.")
	}
	for _, cursor := range []*string{page.NextCursor, page.BackwardsCursor} {
		if cursor != nil && len(*cursor) > 4096 {
			return nil, errors.New("Codex returned an invalid conversation cursor.")
		}
	}
	seen := make(map[string]bool, len(page.Data))
	for i, entry := range page.Data {
		var item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(entry.Item, &item) != nil || item.ID == "" || len(item.ID) > 512 || item.Type == "" || len(item.Type) > 128 || entry.TurnID == "" || len(entry.TurnID) > 512 || seen[item.ID] {
			return nil, errors.New("Codex returned an invalid conversation item.")
		}
		seen[item.ID] = true
		var redactor *auth.Redactor
		if len(redactors) > 0 {
			redactor = redactors[0]
		}
		items, err := webUISanitizeHistoryItems(redactor, []json.RawMessage{entry.Item})
		if err != nil {
			return nil, err
		}
		page.Data[i].Item = items[0]
	}
	result, err := json.Marshal(page)
	if len(result) > webUIHistoryPageBytes {
		return nil, errors.New("Codex returned an oversized conversation page.")
	}
	return result, err
}
