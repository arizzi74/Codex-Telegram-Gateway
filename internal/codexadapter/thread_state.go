package codexadapter

import (
	"context"
	"errors"
	"strings"
)

// ReadThreadState reads metadata and the latest turn's state without fetching
// any turn items. Image-heavy history must not grow routine discovery or idle
// checks beyond the app-server response limit. A newly created thread has no
// turn log until its first user message; only that explicit app-server response
// permits a fresh metadata-only read instead of the bounded turn list.
func (c *Client) ReadThreadState(ctx context.Context, threadID string) (Thread, error) {
	thread, err := c.ReadThread(ctx, threadID, false)
	if err != nil {
		return Thread{}, err
	}
	if thread.ID != threadID {
		return Thread{}, errors.New("thread/read returned a different thread")
	}
	var reply struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}
	if err := c.request(ctx, "thread/turns/list", params, &reply, false); err != nil {
		var rpc *RPCError
		if !errors.As(err, &rpc) || rpc.Code != -32600 ||
			!strings.Contains(rpc.Message, "thread "+threadID+" is not materialized yet") ||
			!strings.Contains(rpc.Message, "thread/turns/list is unavailable before first user message") {
			return Thread{}, err
		}
		// Do not return the earlier snapshot: the first turn may have started
		// between the metadata read and the unavailable-history response.
		thread, err = c.ReadThread(ctx, threadID, false)
		if err != nil {
			return Thread{}, err
		}
		if thread.ID != threadID {
			return Thread{}, errors.New("thread/read returned a different thread")
		}
		return thread, nil
	}
	if reply.Data == nil || len(reply.Data) > 1 {
		return Thread{}, errors.New("thread/turns/list returned an invalid state page")
	}
	thread.ActiveTurnID = ""
	if len(reply.Data) == 1 {
		turn := reply.Data[0]
		if turn.ID == "" {
			return Thread{}, errors.New("thread/turns/list omitted turn identity")
		}
		switch turn.Status {
		case "inProgress":
			thread.ActiveTurnID = turn.ID
		case "completed", "interrupted", "failed":
		default:
			return Thread{}, errors.New("thread/turns/list returned an unknown turn state")
		}
	}
	return thread, nil
}
