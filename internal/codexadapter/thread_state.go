package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ThreadTurnState retains only latest-turn lifecycle evidence, never messages,
// tool arguments, or arbitrary error text. Timestamps are Unix seconds; nil
// means the app server did not provide that evidence.
type ThreadTurnState struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	StartedAt   *int64 `json:"startedAt"`
	CompletedAt *int64 `json:"completedAt"`
}

// SubAgentIdentity projects an explicit SessionSource subAgent variant. Only
// thread_spawn carries ancestry; other or future variants cannot establish that
// a loaded thread is a spawned child that can be considered for recovery.
type SubAgentIdentity struct {
	Kind           string
	ParentThreadID string
	Depth          int32
	AgentPath      string
	AgentNickname  string
	AgentRole      string
}

// TerminalFailedSubAgent reports narrowly established failed-child evidence.
// It does not establish runtime quiescence: callers must separately fence new
// activity and verify queues, descendants, approvals, and background work.
// In particular, a loaded systemError thread has not necessarily shut down.
func (t Thread) TerminalFailedSubAgent() bool {
	child, turn := t.SubAgent, t.LatestTurn
	if strings.TrimSpace(t.ID) == "" || t.Source != "subAgent" || child == nil || child.Kind != "thread_spawn" || child.Depth < 1 ||
		strings.TrimSpace(child.ParentThreadID) == "" || child.ParentThreadID == t.ID || t.ParentThreadID != child.ParentThreadID ||
		(t.ThreadSource != "" && t.ThreadSource != "subagent") || t.Status != "systemError" || t.ActiveTurnID != "" ||
		turn == nil || strings.TrimSpace(turn.ID) == "" || turn.Status != "failed" || turn.StartedAt == nil || turn.CompletedAt == nil {
		return false
	}
	return *turn.StartedAt > 0 && *turn.CompletedAt >= *turn.StartedAt && *turn.CompletedAt <= time.Now().Unix()
}

func decodeSubAgentIdentity(raw json.RawMessage) *SubAgentIdentity {
	source, ok := uniqueJSONObject(raw)
	if !ok || len(source) != 1 || source["subAgent"] == nil {
		return nil
	}
	var kind string
	if json.Unmarshal(source["subAgent"], &kind) == nil {
		switch kind {
		case "review", "compact", "memory_consolidation":
			return &SubAgentIdentity{Kind: kind}
		default:
			return nil
		}
	}
	variant, ok := uniqueJSONObject(source["subAgent"])
	if !ok || len(variant) != 1 {
		return nil
	}
	if variant["other"] != nil {
		var other string
		if json.Unmarshal(variant["other"], &other) == nil && other != "" {
			return &SubAgentIdentity{Kind: "other"}
		}
		return nil
	}
	spawn, ok := uniqueJSONObject(variant["thread_spawn"])
	if !ok || spawn["parent_thread_id"] == nil || spawn["depth"] == nil {
		return nil
	}
	var wire struct {
		ParentThreadID string `json:"parent_thread_id"`
		Depth          *int32 `json:"depth"`
		AgentPath      string `json:"agent_path"`
		AgentNickname  string `json:"agent_nickname"`
		AgentRole      string `json:"agent_role"`
	}
	if json.Unmarshal(variant["thread_spawn"], &wire) != nil || wire.Depth == nil || *wire.Depth < 1 || strings.TrimSpace(wire.ParentThreadID) == "" {
		return nil
	}
	return &SubAgentIdentity{Kind: "thread_spawn", ParentThreadID: wire.ParentThreadID, Depth: *wire.Depth, AgentPath: wire.AgentPath, AgentNickname: wire.AgentNickname, AgentRole: wire.AgentRole}
}

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
	var rawReply json.RawMessage
	params := map[string]any{"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"}
	if err := c.request(ctx, "thread/turns/list", params, &rawReply, false); err != nil {
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
	page, unique := uniqueJSONObject(rawReply)
	var data []json.RawMessage
	if !unique || page["data"] == nil || json.Unmarshal(page["data"], &data) != nil || data == nil || len(data) > 1 {
		return Thread{}, errors.New("thread/turns/list returned an invalid state page")
	}
	thread.ActiveTurnID = ""
	thread.LatestTurn = nil
	if len(data) == 1 {
		var turn ThreadTurnState
		if _, unique := uniqueJSONObject(data[0]); !unique || json.Unmarshal(data[0], &turn) != nil {
			return Thread{}, errors.New("thread/turns/list returned invalid turn state")
		}
		if strings.TrimSpace(turn.ID) == "" {
			return Thread{}, errors.New("thread/turns/list omitted turn identity")
		}
		switch turn.Status {
		case "inProgress":
			thread.ActiveTurnID = turn.ID
		case "completed", "interrupted", "failed":
		default:
			return Thread{}, errors.New("thread/turns/list returned an unknown turn state")
		}
		thread.LatestTurn = &turn
	}
	return thread, nil
}
