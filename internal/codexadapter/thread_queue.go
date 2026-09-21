package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// VerifyThreadQueueIdle verifies only the first queued input. Update admission
// needs proof that the queue is empty; it never needs to fetch all queued text.
// The data field must be an explicit array. The protocol permits an omitted or
// null continuation cursor; a nonempty page or cursor is never an idle queue.
func (c *Client) VerifyThreadQueueIdle(ctx context.Context, threadID string) error {
	if threadID == "" {
		return errors.New("worker update: native queue verification requires a thread")
	}
	var raw json.RawMessage
	if err := c.request(ctx, "thread/queue/list", map[string]any{"threadId": threadID, "limit": 1}, &raw, false); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// RPC errors may include server-supplied text. Avoid copying that text
		// or any queued prompt into updater diagnostics.
		return errors.New("worker update: native queued inputs could not be verified")
	}
	reply, valid := queueStateObject(raw)
	var data []json.RawMessage
	var cursor string
	if !valid || len(reply["data"]) == 0 || json.Unmarshal(reply["data"], &data) != nil || data == nil ||
		(len(reply["nextCursor"]) > 0 && !bytes.Equal(bytes.TrimSpace(reply["nextCursor"]), []byte("null")) && json.Unmarshal(reply["nextCursor"], &cursor) != nil) {
		return errors.New("worker update: native queue returned an invalid state page")
	}
	if len(data) != 0 || cursor != "" {
		return errors.New("worker update: native CLI inputs are still queued")
	}
	return nil
}

// Duplicate fields are ambiguous: a later empty data field must not hide a
// nonempty queue earlier in the same response.
func queueStateObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, false
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		fields[key] = value
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, false
	}
	_, err = decoder.Token()
	return fields, err == io.EOF
}
