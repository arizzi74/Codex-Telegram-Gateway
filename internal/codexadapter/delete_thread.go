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

// ReadThreadForDeletion verifies current turn state, including a newly created
// empty thread whose turn log has not been materialized yet. Only app-server's
// explicit no-first-message response permits the metadata-only fallback.
func (c *Client) ReadThreadForDeletion(ctx context.Context, threadID string) (Thread, error) {
	thread, err := c.ReadThreadState(ctx, threadID)
	if err == nil {
		return thread, nil
	}
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32600 ||
		!strings.Contains(rpc.Message, "thread "+threadID+" is not materialized yet") ||
		!strings.Contains(rpc.Message, "thread/turns/list is unavailable before first user message") {
		return Thread{}, err
	}
	thread, err = c.ReadThread(ctx, threadID, false)
	if err != nil {
		return Thread{}, err
	}
	if thread.ID != threadID {
		return Thread{}, errors.New("thread/read returned a different thread")
	}
	return thread, nil
}
