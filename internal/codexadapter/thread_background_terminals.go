package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// VerifyThreadBackgroundTerminalsIdle checks the first terminal page without
// retaining command text, process details, or output. Only an explicit empty
// array without a continuation establishes that no background terminal is
// listed. This read never cleans up or terminates a terminal.
func (c *Client) VerifyThreadBackgroundTerminalsIdle(ctx context.Context, threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("worker update: background terminal verification requires a thread")
	}
	var raw json.RawMessage
	if err := c.request(ctx, "thread/backgroundTerminals/list", map[string]any{"threadId": threadID, "limit": 1}, &raw, false); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("worker update: background terminals could not be verified")
	}
	fields, valid := uniqueJSONObject(raw)
	var data []json.RawMessage
	var cursor string
	if !valid || fields["data"] == nil || json.Unmarshal(fields["data"], &data) != nil || data == nil ||
		(len(fields["nextCursor"]) > 0 && !bytes.Equal(bytes.TrimSpace(fields["nextCursor"]), []byte("null")) && json.Unmarshal(fields["nextCursor"], &cursor) != nil) {
		return errors.New("worker update: background terminals returned an invalid state page")
	}
	if len(data) != 0 || cursor != "" {
		return errors.New("worker update: background terminals are still present")
	}
	return nil
}
