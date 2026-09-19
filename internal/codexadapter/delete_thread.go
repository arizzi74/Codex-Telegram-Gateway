package codexadapter

import (
	"context"
	"errors"
	"strings"
)

// DeleteThread permanently deletes the persisted Codex thread and its spawned
// descendants. App-server owns the thread files; this operation never removes
// the working directory. In particular, it must not fall back to archiving.
func (c *Client) DeleteThread(ctx context.Context, threadID string) error {
	if strings.TrimSpace(threadID) == "" {
		return errors.New("thread id is required")
	}
	return c.request(ctx, "thread/delete", map[string]any{"threadId": threadID}, nil, false)
}

// ReadThreadForDeletion uses the same fresh, bounded state check as discovery
// and update preparation, including threads without a first user message.
func (c *Client) ReadThreadForDeletion(ctx context.Context, threadID string) (Thread, error) {
	return c.ReadThreadState(ctx, threadID)
}
