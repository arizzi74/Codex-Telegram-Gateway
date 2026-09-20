package codexadapter

import (
	"context"
	"errors"
	"strings"
)

// Routine discovery must not reopen saved JSONL conversations to repair
// metadata. Current app-server versions can answer from their state database.
// Older versions may ignore this optional field or explicitly reject it; only
// an explicit unsupported-field error enables the compatibility fallback.
func (c *Client) requestThreadList(ctx context.Context, params map[string]any, reply any) error {
	c.mu.Lock()
	unsupported := c.stateDBListUnsupported
	c.mu.Unlock()
	if !unsupported {
		params["useStateDbOnly"] = true
	}
	err := c.request(ctx, "thread/list", params, reply, false)
	if unsupported || !unsupportedStateDBListField(err) {
		return err
	}
	c.mu.Lock()
	c.stateDBListUnsupported = true
	c.mu.Unlock()
	delete(params, "useStateDbOnly")
	return c.request(ctx, "thread/list", params, reply, false)
}

func unsupportedStateDBListField(err error) bool {
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != -32602 {
		return false
	}
	message := strings.ToLower(rpc.Message)
	if !strings.Contains(message, "usestatedbonly") && !strings.Contains(message, "use_state_db_only") {
		return false
	}
	return strings.Contains(message, "unknown") || strings.Contains(message, "unsupported") || strings.Contains(message, "unrecognized")
}
