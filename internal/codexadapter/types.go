package codexadapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Event is a server notification. Params and Raw are retained exactly enough
// for the worker to handle protocol additions before this package grows a
// dedicated projection. Unknown is true for messages outside the core methods
// known by this version of the adapter.
type Event struct {
	// Kind is a stable worker-facing name. Worker code must switch on Kind,
	// never on the evolving app-server Method.
	Kind      string
	Method    string
	Params    json.RawMessage
	Raw       json.RawMessage
	ThreadID  string
	TurnID    string
	ItemID    string
	ItemType  string
	State     string
	Text      string
	Thread    *Thread
	RequestID string
	Unknown   bool
	Err       error
}

// Request is a server-initiated JSON-RPC request, normally an approval or user
// input prompt. ID is raw deliberately: app-server request IDs must be echoed
// exactly, including a future string-valued ID.
type Request struct {
	// RequestID is a stable, lossless encoding of the JSON-RPC ID. It avoids
	// collisions between e.g. numeric 1 and string "1".
	RequestID      string
	Kind           string
	ID             json.RawMessage
	Method         string
	Params         json.RawMessage
	ThreadID       string
	TurnID         string
	ItemID         string
	ApprovalType   string
	Summary        string
	Reason         string
	Command        string
	CWD            string
	Decisions      []string
	DecisionValues []json.RawMessage
	Questions      []Question
	Permissions    json.RawMessage
}

// Question and Choice are normalized request_user_input prompts.
type Question struct {
	ID       string
	Header   string
	Prompt   string
	IsOther  bool
	IsSecret bool
	Choices  []Choice
}

type Choice struct{ Label, Description string }

func newEvent(method string, params json.RawMessage) Event {
	threadID, turnID, itemID := ids(params)
	event := Event{Kind: eventKind(method), Method: method, Params: cloneRaw(params), Raw: cloneRaw(params), ThreadID: threadID, TurnID: turnID, ItemID: itemID, Unknown: !knownNotification(method)}
	var value struct {
		Status json.RawMessage `json:"status"`
		State  json.RawMessage `json:"state"`
		Delta  string          `json:"delta"`
		Text   string          `json:"text"`
		Thread json.RawMessage `json:"thread"`
		Turn   struct {
			Status string `json:"status"`
		} `json:"turn"`
		Item struct {
			Status string `json:"status"`
			Text   string `json:"text"`
			Type   string `json:"type"`
		} `json:"item"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &value) == nil {
		event.State = first(statusName(value.State), statusName(value.Status), value.Turn.Status, value.Item.Status)
		event.Text = first(value.Delta, value.Text, value.Item.Text)
		event.ItemType = value.Item.Type
		if method == "item/completed" && value.Item.Type == "agentMessage" {
			event.Kind = "final_agent_message"
		}
		if method == "turn/completed" {
			switch event.State {
			case "failed":
				event.Kind = "turn_failed"
			case "interrupted":
				event.Kind = "turn_interrupted"
			}
		}
		if len(value.Thread) != 0 {
			if thread, err := decodeThread(value.Thread); err == nil {
				event.Thread = &thread
			}
		}
		if len(value.RequestID) != 0 {
			event.RequestID = stableRequestID(value.RequestID)
		}
	}
	return event
}

func newRequest(method string, id, params json.RawMessage) Request {
	threadID, turnID, itemID := ids(params)
	request := Request{RequestID: stableRequestID(id), Kind: requestKind(method), ID: cloneRaw(id), Method: method, Params: cloneRaw(params), ThreadID: threadID, TurnID: turnID, ItemID: itemID}
	var value struct {
		Reason             string          `json:"reason"`
		Command            string          `json:"command"`
		CWD                string          `json:"cwd"`
		Permissions        json.RawMessage `json:"permissions"`
		AvailableDecisions json.RawMessage `json:"availableDecisions"`
		Questions          []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			IsOther  bool   `json:"isOther"`
			IsSecret bool   `json:"isSecret"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(params, &value)
	request.Reason, request.Command, request.CWD, request.Permissions = value.Reason, value.Command, value.CWD, cloneRaw(value.Permissions)
	switch request.Kind {
	case "approval_requested":
		request.ApprovalType = approvalType(method)
		choices := decodeAvailableDecisions(value.AvailableDecisions)
		// App-server v0.154 schemas do not include availableDecisions for
		// command/file requests. Use the versioned scalar enum only when the
		// field is absent or null; an explicit [] means no decision is offered.
		if value.AvailableDecisions == nil || string(value.AvailableDecisions) == "null" {
			choices = schemaApprovalDecisions(method)
		}
		request.Decisions, request.DecisionValues = approvalDecisions(choices)
		request.Summary = approvalSummary(request.ApprovalType, value.Command, value.CWD, value.Reason)
	case "input_requested":
		for _, question := range value.Questions {
			projection := Question{ID: question.ID, Header: question.Header, Prompt: question.Question, IsOther: question.IsOther, IsSecret: question.IsSecret}
			for _, option := range question.Options {
				projection.Choices = append(projection.Choices, Choice{Label: option.Label, Description: option.Description})
			}
			request.Questions = append(request.Questions, projection)
		}
		request.Summary = "Codex requested user input"
	}
	return request
}

func decodeAvailableDecisions(raw json.RawMessage) []json.RawMessage {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

// schemaApprovalDecisions is the generated v0.154 response enum fallback for
// requests that do not include availableDecisions. When app-server supplies
// that field, its exact scalar or structured values always take precedence.
func schemaApprovalDecisions(method string) []json.RawMessage {
	var values []string
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		values = []string{"accept", "acceptForSession", "decline", "cancel"}
	default:
		return nil
	}
	result := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		result = append(result, encoded)
	}
	return result
}

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stableRequestID(raw json.RawMessage) string {
	return "jsonrpc:" + base64.RawURLEncoding.EncodeToString(raw)
}

func eventKind(method string) string {
	switch method {
	case "thread/started":
		return "thread_started"
	case "thread/status/changed":
		return "thread_status_changed"
	case "thread/closed":
		return "thread_closed"
	case "turn/started":
		return "turn_started"
	case "turn/completed":
		return "turn_completed"
	case "item/started":
		return "item_started"
	case "item/completed":
		return "item_completed"
	case "item/agentMessage/delta":
		return "agent_message_delta"
	case "serverRequest/resolved":
		return "server_request_resolved"
	default:
		return "unknown"
	}
}

func requestKind(method string) string {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval":
		return "approval_requested"
	case "item/tool/requestUserInput", "tool/requestUserInput":
		return "input_requested"
	case "mcpServer/elicitation/request":
		return "elicitation_requested"
	default:
		return "server_request"
	}
}

func approvalType(method string) string {
	switch method {
	case "item/commandExecution/requestApproval":
		return "command_execution"
	case "item/fileChange/requestApproval":
		return "file_change"
	case "item/permissions/requestApproval":
		return "permissions"
	default:
		return "unknown"
	}
}

func approvalDecisions(raw []json.RawMessage) ([]string, []json.RawMessage) {
	values := make([]json.RawMessage, 0, len(raw))
	var labels []string
	for _, value := range raw {
		value = cloneRaw(value)
		values = append(values, value)
		var label string
		if json.Unmarshal(value, &label) == nil {
			labels = append(labels, label)
		}
	}
	return labels, values
}

func approvalSummary(kind, command, cwd, reason string) string {
	parts := []string{kind}
	if command != "" {
		parts = append(parts, command)
	}
	if cwd != "" {
		parts = append(parts, cwd)
	}
	if reason != "" {
		parts = append(parts, reason)
	}
	return strings.Join(parts, ": ")
}

func cloneRaw(v json.RawMessage) json.RawMessage {
	if v == nil {
		return nil
	}
	return append(json.RawMessage(nil), v...)
}

func ids(params json.RawMessage) (threadID, turnID, itemID string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(params, &object) != nil {
		return "", "", ""
	}
	decodeString := func(key string) string {
		var value string
		_ = json.Unmarshal(object[key], &value)
		return value
	}
	threadID, turnID, itemID = decodeString("threadId"), decodeString("turnId"), decodeString("itemId")
	if raw := object["thread"]; len(raw) != 0 {
		var value struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &value) == nil && threadID == "" {
			threadID = value.ID
		}
	}
	if raw := object["turn"]; len(raw) != 0 {
		var value struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(raw, &value) == nil {
			if turnID == "" {
				turnID = value.ID
			}
			if threadID == "" {
				threadID = value.ThreadID
			}
		}
	}
	if raw := object["item"]; len(raw) != 0 {
		var value struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &value) == nil && itemID == "" {
			itemID = value.ID
		}
	}
	return threadID, turnID, itemID
}

func knownNotification(method string) bool {
	if strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "item/") {
		return true
	}
	switch method {
	case "serverRequest/resolved":
		return true
	default:
		return false
	}
}

func (c *Client) track(event Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event.Method {
	case "turn/started":
		if event.ThreadID != "" && event.TurnID != "" {
			c.active[event.ThreadID] = event.TurnID
		}
	case "turn/completed":
		if event.ThreadID != "" && (event.TurnID == "" || c.active[event.ThreadID] == event.TurnID) {
			delete(c.active, event.ThreadID)
		}
	case "serverRequest/resolved":
		if event.RequestID != "" {
			delete(c.serverRequests, event.RequestID)
		}
	}
}

// Thread is a stable worker-domain view of an app-server thread. Raw retains
// all protocol fields that do not belong in the domain interface yet.
type Thread struct {
	ID            string
	SessionID     string
	Name          string
	Preview       string
	CWD           string
	Model         string
	ModelProvider string
	Status        string
	CreatedAt     int64
	UpdatedAt     int64
	Ephemeral     bool
	ActiveTurnID  string
	Raw           json.RawMessage
}

// Turn is the stable worker-domain view of a Codex turn.
type Turn struct {
	ID       string
	ThreadID string
	Status   string
	Raw      json.RawMessage
}

func decodeThread(raw json.RawMessage) (Thread, error) {
	var wire struct {
		ID            string          `json:"id"`
		SessionID     string          `json:"sessionId"`
		Name          string          `json:"name"`
		Preview       string          `json:"preview"`
		CWD           string          `json:"cwd"`
		Model         string          `json:"model"`
		ModelProvider string          `json:"modelProvider"`
		Status        json.RawMessage `json:"status"`
		CreatedAt     int64           `json:"createdAt"`
		UpdatedAt     int64           `json:"updatedAt"`
		Ephemeral     bool            `json:"ephemeral"`
		Turns         []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Thread{}, err
	}
	thread := Thread{ID: wire.ID, SessionID: wire.SessionID, Name: wire.Name, Preview: wire.Preview, CWD: wire.CWD, Model: wire.Model, ModelProvider: wire.ModelProvider, Status: statusName(wire.Status), CreatedAt: wire.CreatedAt, UpdatedAt: wire.UpdatedAt, Ephemeral: wire.Ephemeral, Raw: cloneRaw(raw)}
	for _, turn := range wire.Turns {
		if turn.Status == "inProgress" {
			thread.ActiveTurnID = turn.ID
			break
		}
	}
	return thread, nil
}

func statusName(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var object struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.Type
	}
	return ""
}

func decodeTurn(raw json.RawMessage) (Turn, error) {
	var wire struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Turn{}, err
	}
	return Turn{ID: wire.ID, ThreadID: wire.ThreadID, Status: wire.Status, Raw: cloneRaw(raw)}, nil
}

// ThreadOptions contains settings shared by thread/start and thread/resume.
// Empty values are omitted so Codex may apply its configured defaults.
type ThreadOptions struct {
	Model          string
	CWD            string
	ApprovalPolicy string
	Sandbox        string
	Personality    string
	ServiceName    string
}

func (o ThreadOptions) fields() map[string]any {
	params := make(map[string]any)
	if o.Model != "" {
		params["model"] = o.Model
	}
	if o.CWD != "" {
		params["cwd"] = o.CWD
	}
	if o.ApprovalPolicy != "" {
		params["approvalPolicy"] = o.ApprovalPolicy
	}
	if o.Sandbox != "" {
		params["sandbox"] = o.Sandbox
	}
	if o.Personality != "" {
		params["personality"] = o.Personality
	}
	if o.ServiceName != "" {
		params["serviceName"] = o.ServiceName
	}
	return params
}

// StartThread starts a new thread. The returned ID is always supplied by
// app-server; this package never invents thread IDs.
func (c *Client) StartThread(ctx context.Context, options ThreadOptions) (Thread, error) {
	var reply struct {
		Thread json.RawMessage `json:"thread"`
	}
	if err := c.request(ctx, "thread/start", options.fields(), &reply, false); err != nil {
		return Thread{}, err
	}
	thread, err := decodeThread(reply.Thread)
	if err != nil {
		return Thread{}, fmt.Errorf("decode thread/start thread: %w", err)
	}
	if thread.ID == "" {
		return Thread{}, errors.New("thread/start response omitted thread id")
	}
	return thread, nil
}

// ResumeThread loads an existing persisted thread before it receives a turn.
func (c *Client) ResumeThread(ctx context.Context, threadID string, options ThreadOptions) (Thread, error) {
	if threadID == "" {
		return Thread{}, errors.New("thread id is required")
	}
	params := options.fields()
	delete(params, "serviceName") // thread/resume does not accept serviceName.
	params["threadId"] = threadID
	var reply struct {
		Thread json.RawMessage `json:"thread"`
	}
	if err := c.request(ctx, "thread/resume", params, &reply, false); err != nil {
		return Thread{}, err
	}
	thread, err := decodeThread(reply.Thread)
	if err != nil {
		return Thread{}, fmt.Errorf("decode thread/resume thread: %w", err)
	}
	if thread.ID == "" {
		return Thread{}, errors.New("thread/resume response omitted thread id")
	}
	return thread, nil
}

// ReadThread reads stored metadata and, if requested, its turn history.
func (c *Client) ReadThread(ctx context.Context, threadID string, includeTurns bool) (Thread, error) {
	if threadID == "" {
		return Thread{}, errors.New("thread id is required")
	}
	var reply struct {
		Thread json.RawMessage `json:"thread"`
	}
	if err := c.request(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": includeTurns}, &reply, false); err != nil {
		return Thread{}, err
	}
	thread, err := decodeThread(reply.Thread)
	if err != nil {
		return Thread{}, fmt.Errorf("decode thread/read thread: %w", err)
	}
	return thread, nil
}

// ThreadPage is one cursor page from thread/list or thread/loaded/list.
type ThreadPage struct {
	Threads    []Thread
	NextCursor string
}

// ListThreads returns one persisted-thread page. Call ListAllThreads to follow
// cursors through a bounded discovery scan.
func (c *Client) ListThreads(ctx context.Context, cursor string, limit int) (ThreadPage, error) {
	params := map[string]any{"sourceKinds": []string{"cli", "vscode", "exec", "appServer"}}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if limit > 0 {
		params["limit"] = limit
	}
	return c.listThreads(ctx, "thread/list", params)
}

// LoadedThreads returns a page of currently in-memory thread IDs and metadata.
// It is deliberately separate from ListThreads: persisted notLoaded threads are
// not treated as running.
func (c *Client) LoadedThreads(ctx context.Context, cursor string, limit int) (ThreadPage, error) {
	params := map[string]any{}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if limit > 0 {
		params["limit"] = limit
	}
	return c.listThreads(ctx, "thread/loaded/list", params)
}

func (c *Client) listThreads(ctx context.Context, method string, params map[string]any) (ThreadPage, error) {
	if method == "thread/loaded/list" {
		var reply struct {
			Data       []string `json:"data"`
			NextCursor string   `json:"nextCursor"`
		}
		if err := c.request(ctx, method, params, &reply, false); err != nil {
			return ThreadPage{}, err
		}
		page := ThreadPage{NextCursor: reply.NextCursor}
		for _, id := range reply.Data {
			page.Threads = append(page.Threads, Thread{ID: id})
		}
		return page, nil
	}
	var reply struct {
		Data       []json.RawMessage `json:"data"`
		NextCursor string            `json:"nextCursor"`
	}
	if err := c.request(ctx, method, params, &reply, false); err != nil {
		return ThreadPage{}, err
	}
	page := ThreadPage{NextCursor: reply.NextCursor}
	for _, raw := range reply.Data {
		thread, err := decodeThread(raw)
		if err != nil {
			return ThreadPage{}, fmt.Errorf("decode %s thread: %w", method, err)
		}
		page.Threads = append(page.Threads, thread)
	}
	return page, nil
}

// ListAllThreads follows persisted list cursors until completion or max. A max
// of zero means no adapter-imposed cap; workers should provide their policy cap.
func (c *Client) ListAllThreads(ctx context.Context, pageSize, max int) ([]Thread, error) {
	var all []Thread
	for cursor := ""; ; {
		page, err := c.ListThreads(ctx, cursor, pageSize)
		if err != nil {
			return nil, err
		}
		if max > 0 && len(all)+len(page.Threads) > max {
			all = append(all, page.Threads[:max-len(all)]...)
			return all, nil
		}
		all = append(all, page.Threads...)
		if max > 0 && len(all) == max {
			return all, nil
		}
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
}

// StartTurn submits one text input and records the returned turn immediately.
func (c *Client) StartTurn(ctx context.Context, threadID, text string) (Turn, error) {
	if threadID == "" {
		return Turn{}, errors.New("thread id is required")
	}
	var reply struct {
		Turn json.RawMessage `json:"turn"`
	}
	params := map[string]any{"threadId": threadID, "input": []map[string]string{{"type": "text", "text": text}}}
	if err := c.request(ctx, "turn/start", params, &reply, false); err != nil {
		return Turn{}, err
	}
	turn, err := decodeTurn(reply.Turn)
	if err != nil {
		return Turn{}, fmt.Errorf("decode turn/start turn: %w", err)
	}
	if turn.ID == "" {
		return Turn{}, errors.New("turn/start response omitted turn id")
	}
	if turn.ThreadID == "" {
		turn.ThreadID = threadID
	}
	c.mu.Lock()
	c.active[threadID] = turn.ID
	c.mu.Unlock()
	return turn, nil
}

// Steer appends text only to the explicitly expected in-flight turn.
func (c *Client) Steer(ctx context.Context, threadID, expectedTurnID, text string) (Turn, error) {
	if threadID == "" || expectedTurnID == "" {
		return Turn{}, errors.New("thread id and expected turn id are required")
	}
	if !c.matchesActive(threadID, expectedTurnID) {
		return Turn{}, ErrStaleTurn
	}
	var reply struct {
		TurnID string `json:"turnId"`
	}
	params := map[string]any{"threadId": threadID, "expectedTurnId": expectedTurnID, "input": []map[string]string{{"type": "text", "text": text}}}
	if err := c.request(ctx, "turn/steer", params, &reply, false); err != nil {
		return Turn{}, err
	}
	if reply.TurnID == "" {
		return Turn{}, errors.New("turn/steer response omitted turn id")
	}
	return Turn{ID: reply.TurnID, ThreadID: threadID}, nil
}

// Interrupt asks app-server to interrupt the explicitly expected active turn.
func (c *Client) Interrupt(ctx context.Context, threadID, expectedTurnID string) error {
	if threadID == "" || expectedTurnID == "" {
		return errors.New("thread id and expected turn id are required")
	}
	if !c.matchesActive(threadID, expectedTurnID) {
		return ErrStaleTurn
	}
	return c.request(ctx, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": expectedTurnID}, nil, false)
}

func (c *Client) matchesActive(threadID, turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	known := c.active[threadID]
	return known == "" || known == turnID
}
