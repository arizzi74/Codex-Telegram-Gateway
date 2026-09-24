package codexadapter

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"time"
)

const (
	maxPendingRPCs          = 256
	maxUpdateUnconfirmed    = 128
	maxUpdateReadTombstones = 128
	// MaxUpdateSafetyBlockers bounds diagnostic rows, independently of counts.
	MaxUpdateSafetyBlockers = 32
)

// UpdateSafetyStatus contains delivery diagnostics, never RPC parameters,
// response bodies, thread identities, or arbitrary server error text. Pending
// requests include detached calls whose transport write is still in progress.
// Overflow means an unsafe abandoned request could not be retained: it remains
// a blocker until this owned runtime is safely replaced.
type UpdateSafetyStatus struct {
	Ready               bool                  `json:"ready"`
	Closed              bool                  `json:"closed"`
	PendingRequests     int                   `json:"pending_requests"`
	PendingApprovals    int                   `json:"pending_approvals"`
	UnconfirmedRequests int                   `json:"unconfirmed_requests"`
	Overflow            bool                  `json:"overflow"`
	Blockers            []UpdateSafetyBlocker `json:"blockers,omitempty"`
}

// UpdateSafetyBlocker identifies a local RPC, with a finite allowlist of method
// names. Phase is pending, writing, or unconfirmed. These are observations only;
// UpdateSafety does not issue RPCs or change runtime state.
type UpdateSafetyBlocker struct {
	ID        int64     `json:"id"`
	Method    string    `json:"method"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"started_at"`
}

type pendingRPC struct {
	id        int64
	method    string
	readOnly  bool
	startedAt time.Time
	done      chan struct{}
	// All lifecycle fields are protected by Client.mu. Only live callers
	// retain reply payloads; abandoned records contain diagnostic metadata.
	writeDone    bool
	responseSeen bool
	rejected     bool
	reply        *rpcResponse
}

func (c *Client) UpdateSafety() UpdateSafetyStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := UpdateSafetyStatus{Ready: c.ready, Closed: c.cause != nil, PendingRequests: len(c.pending), PendingApprovals: len(c.serverRequests), UnconfirmedRequests: len(c.updateUnconfirmed), Overflow: c.updateOverflow}
	rows := make(map[int64]UpdateSafetyBlocker, len(c.pending)+len(c.updateUnconfirmed)+len(c.updateWriting))
	for id, request := range c.pending {
		rows[id] = updateBlocker(request, "pending")
	}
	for id, request := range c.updateUnconfirmed {
		rows[id] = updateBlocker(request, "unconfirmed")
	}
	for id, request := range c.updateWriting {
		if _, live := c.pending[id]; !live {
			if _, unsafe := c.updateUnconfirmed[id]; !unsafe {
				status.PendingRequests++
			}
		}
		rows[id] = updateBlocker(request, "writing")
	}
	ordered := make([]UpdateSafetyBlocker, 0, len(rows))
	for _, row := range rows {
		ordered = append(ordered, row)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	if len(ordered) > MaxUpdateSafetyBlockers {
		ordered = ordered[:MaxUpdateSafetyBlockers]
	}
	status.Blockers = ordered
	return status
}

func updateBlocker(request *pendingRPC, phase string) UpdateSafetyBlocker {
	return UpdateSafetyBlocker{ID: request.id, Method: request.method, Phase: phase, StartedAt: request.startedAt}
}

func (c *Client) finishRPCWrite(request *pendingRPC) {
	c.mu.Lock()
	defer c.mu.Unlock()
	request.writeDone = true
	delete(c.updateWriting, request.id)
}

func (c *Client) finishRPCCall(request *pendingRPC, admitted, consumed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, request.id)
	request.reply = nil
	if !admitted || c.cause != nil {
		return
	}
	// The unbuffered writer admits only one unfinished write. Keep it fenced
	// even when a read caller stops waiting before the writer acknowledges.
	if !request.writeDone {
		c.updateWriting[request.id] = request
	}
	if consumed || request.readOnly || request.rejected {
		if request.readOnly && !request.responseSeen {
			c.rememberUpdateReadLocked(request.id)
		}
		return
	}
	// A successful async mutation response is not proof of completed work if
	// its caller abandoned the result. Preserve that exact request forever
	// unless its first response definitively rejects it before execution.
	if len(c.updateUnconfirmed) >= maxUpdateUnconfirmed {
		c.updateOverflow = true
		return
	}
	c.updateUnconfirmed[request.id] = request
}

func (c *Client) rememberUpdateReadLocked(id int64) {
	delete(c.updateReadTombstones, c.updateReadIDs[c.updateReadNext])
	c.updateReadIDs[c.updateReadNext] = id
	c.updateReadNext = (c.updateReadNext + 1) % maxUpdateReadTombstones
	c.updateReadTombstones[id] = struct{}{}
}

func (c *Client) acceptRPCResponse(id int64, response rpcResponse, rejection bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cause != nil {
		return true
	}
	if pending := c.pending[id]; pending != nil {
		if !pending.responseSeen {
			pending.responseSeen, pending.rejected = true, rejection
			pending.reply = &response
			close(pending.done)
		}
		return true
	}
	if abandoned := c.updateUnconfirmed[id]; abandoned != nil {
		if !abandoned.responseSeen {
			abandoned.responseSeen, abandoned.rejected = true, rejection
			if rejection {
				delete(c.updateUnconfirmed, id)
			}
		}
		return true
	}
	if _, read := c.updateReadTombstones[id]; read {
		delete(c.updateReadTombstones, id)
		return true
	}
	return false
}

// Keep this explicit: a method suffix or an unknown future API is not evidence
// that a request cannot enqueue work. Fresh worker idle verification remains
// mandatory after a metadata read is abandoned.
func updateReadOnlyMethod(method string) bool {
	switch method {
	case "thread/list", "thread/loaded/list", "thread/read", "thread/turns/list", "thread/items/list", "thread/queue/list", "thread/goal/get", "thread/backgroundTerminals/list",
		"config/read", "configRequirements/read", "account/usage/read", "account/rateLimits/read", "model/list", "permissionProfile/list", "experimentalFeature/list",
		"mcpServerStatus/list", "app/installed", "skills/list", "hooks/list", "plugin/list":
		return true
	default:
		return false
	}
}

func updateMethodName(method string) string {
	if updateReadOnlyMethod(method) {
		return method
	}
	switch method {
	case "initialize", "thread/start", "thread/resume", "turn/start", "turn/steer", "turn/interrupt", "thread/settings/update", "thread/compact/start", "thread/name/set", "thread/fork", "review/start", "thread/goal/set", "thread/goal/clear", "thread/backgroundTerminals/clean", "thread/archive", "thread/delete", "thread/memoryMode/set":
		return method
	default:
		return "unknown"
	}
}

// Only standard pre-execution rejections can retire an abandoned mutation.
// Internal/server errors may follow partially accepted work. Require an exact,
// unambiguous envelope instead of trusting permissive struct unmarshalling.
func validateRPCResponse(raw []byte) (bool, bool) {
	object, ok := uniqueJSONObject(raw)
	if !ok || object["method"] != nil || object["id"] == nil || (object["result"] != nil) == (object["error"] != nil) {
		return false, false
	}
	var id int64
	if json.Unmarshal(object["id"], &id) != nil || bytes.Equal(bytes.TrimSpace(object["id"]), []byte("null")) {
		return false, false
	}
	if version, exists := object["jsonrpc"]; exists {
		var text string
		if json.Unmarshal(version, &text) != nil || text != "2.0" {
			return false, false
		}
	}
	if object["result"] != nil {
		return true, false
	}
	failure, ok := uniqueJSONObject(object["error"])
	if !ok || failure["code"] == nil || failure["message"] == nil {
		return false, false
	}
	var code int
	var message string
	if json.Unmarshal(failure["code"], &code) != nil || bytes.Equal(bytes.TrimSpace(failure["code"]), []byte("null")) || json.Unmarshal(failure["message"], &message) != nil || bytes.Equal(bytes.TrimSpace(failure["message"]), []byte("null")) {
		return false, false
	}
	return true, code == -32600 || code == -32601 || code == -32602
}

func uniqueJSONObject(raw []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || object[key] != nil {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		object[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return object, true
}
