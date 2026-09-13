// Package codexadapter isolates the Codex app-server JSON-RPC protocol from
// the rest of the worker. It speaks only newline-delimited JSON over a local
// stdio transport.
package codexadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrClosed is returned after the underlying app-server process exits or
	// the client is closed.
	ErrClosed = errors.New("codex app-server connection closed")
	// ErrNotInitialized prevents accidental requests before the required
	// initialize/initialized handshake.
	ErrNotInitialized = errors.New("codex app-server is not initialized")
	// ErrAlreadyInitialized is returned when Initialize is called twice.
	ErrAlreadyInitialized = errors.New("codex app-server is already initialized")
	// ErrStaleTurn means an operation referred to a different known active turn.
	ErrStaleTurn = errors.New("codex app-server turn is not the expected active turn")
	// ErrMethodUnavailable marks a JSON-RPC -32601 response. The worker can use
	// Supports to expose a deterministic degraded runtime state.
	ErrMethodUnavailable = errors.New("codex app-server method unavailable")
	// ErrRequestNotPending rejects a stale or duplicate response to a
	// server-initiated approval/input request.
	ErrRequestNotPending = errors.New("codex app-server request is no longer pending")
)

// ClientInfo identifies this integration during the app-server handshake.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// Config controls a Client. Command defaults to "codex" and each request gets
// RequestTimeout when its caller did not already provide a deadline.
type Config struct {
	Command string
	Args    []string
	// WorkingDirectory becomes the app-server process working directory.
	WorkingDirectory string
	// Stderr receives process diagnostics. Supply a redacting writer; the
	// adapter deliberately does not log raw process output itself.
	Stderr         io.Writer
	ClientInfo     ClientInfo
	RequestTimeout time.Duration
	EventBuffer    int
	RequestBuffer  int
}

func (c Config) normalized() Config {
	if c.Command == "" {
		c.Command = "codex"
	}
	if len(c.Args) == 0 {
		c.Args = []string{"app-server", "--listen", "stdio://"}
	}
	if c.ClientInfo.Name == "" {
		c.ClientInfo.Name = "telegramgw"
	}
	if c.ClientInfo.Version == "" {
		c.ClientInfo.Version = "dev"
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.EventBuffer <= 0 {
		c.EventBuffer = 128
	}
	if c.RequestBuffer <= 0 {
		c.RequestBuffer = 32
	}
	return c
}

// Transport is a bidirectional JSONL connection. Wait is optional, but a
// process-backed transport should provide it so pending calls fail promptly on
// process exit. Close must be safe to call more than once.
type Transport struct {
	In    io.WriteCloser
	Out   io.ReadCloser
	Wait  <-chan error
	Close func() error
}

// Client owns one reader and one writer goroutine. Events and Requests retain
// the raw params so new App Server messages remain available to the worker even
// when this package has not added typed projections for them yet.
type Client struct {
	config Config
	t      Transport

	writeCh   chan outbound
	eventIn   chan Event
	events    chan Event
	requestIn chan Request
	reqs      chan Request
	done      chan struct{}

	mu             sync.Mutex
	pending        map[int64]chan rpcResponse
	active         map[string]string
	cause          error
	ready          bool
	info           InitializeInfo
	methods        map[string]bool
	serverRequests map[string]Request
	pid            int
	localSocket    string
	nextID         atomic.Int64
	stop           sync.Once
}

type outbound struct {
	v   any
	err chan error
}

type rpcRequest struct {
	Method string `json:"method"`
	ID     int64  `json:"id"`
	Params any    `json:"params"`
}

type rpcNotification struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// RPCError is the error object returned by app-server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("codex app-server RPC error %d: %s", e.Code, e.Message)
}

// Start launches the normal direct local stdio app-server and completes the
// mandatory handshake before returning the client.
func Start(ctx context.Context, config Config) (*Client, error) {
	config = config.normalized()
	// The context governs the startup handshake. Do not attach it to the child:
	// callers commonly use a short startup deadline, while a healthy app-server
	// must keep running after Start returns.
	cmd := exec.Command(config.Command, config.Args...)
	cmd.Dir = config.WorkingDirectory
	cmd.Stderr = config.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open codex stdin: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("open codex stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("start codex app-server: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var closeOnce sync.Once
	t := Transport{
		In: in, Out: out, Wait: wait,
		Close: func() (closeErr error) {
			closeOnce.Do(func() {
				_ = in.Close()
				_ = out.Close()
				if cmd.Process != nil {
					closeErr = cmd.Process.Kill()
					if errors.Is(closeErr, os.ErrProcessDone) {
						closeErr = nil
					}
				}
			})
			return closeErr
		},
	}
	c := New(t, config)
	c.mu.Lock()
	c.pid = cmd.Process.Pid
	c.mu.Unlock()
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// New constructs a client around an established transport. It is useful for
// in-memory fake app-server tests. Call Initialize before typed operations.
func New(t Transport, config Config) *Client {
	config = config.normalized()
	c := &Client{
		config: config, t: t,
		writeCh: make(chan outbound), eventIn: make(chan Event), events: make(chan Event, config.EventBuffer),
		requestIn: make(chan Request), reqs: make(chan Request, config.RequestBuffer), done: make(chan struct{}),
		pending: make(map[int64]chan rpcResponse), active: make(map[string]string), methods: defaultMethods(), serverRequests: make(map[string]Request),
	}
	go c.writer()
	go c.eventDispatcher()
	go c.requestDispatcher()
	go c.reader()
	if t.Wait != nil {
		go func() {
			err, ok := <-t.Wait
			if !ok {
				err = nil
			}
			if err == nil {
				err = ErrClosed
			}
			c.fail(fmt.Errorf("codex app-server exited: %w", err))
		}()
	}
	return c
}

func defaultMethods() map[string]bool {
	return map[string]bool{
		"initialize": true, "thread/start": true, "thread/resume": true,
		"thread/read": true, "thread/list": true, "thread/loaded/list": true,
		"turn/start": true, "turn/steer": true, "turn/interrupt": true,
	}
}

// Events yields all server notifications, including unknown notifications.
func (c *Client) Events() <-chan Event { return c.events }

// Requests yields server-initiated JSON-RPC requests such as approvals and
// input prompts. The Request ID must be returned verbatim through Reply.
func (c *Client) Requests() <-chan Request { return c.reqs }

// RequestPending reports whether a server-initiated request is still awaiting
// a reply. Coordinators must check it when consuming Requests because a
// serverRequest/resolved notification can arrive first on the event stream.
func (c *Client) RequestPending(requestID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, pending := c.serverRequests[requestID]
	return pending
}

// Err reports the terminal process or transport error after Done closes.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// Done closes when the transport is no longer usable.
func (c *Client) Done() <-chan struct{} { return c.done }

// PID is the directly spawned app-server PID, or zero for a supplied
// transport such as an in-memory fake or a stdio proxy.
func (c *Client) PID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pid
}

// LocalSocket is the owned private Unix app-server socket for a shared
// runtime. Direct stdio runtimes return an empty string.
func (c *Client) LocalSocket() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localSocket
}

// ExecutableVersion returns the installed CLI's version text. It is separate
// from the running process handshake so startup supervisors can record the
// exact executable version before opening a runtime.
func ExecutableVersion(ctx context.Context, executable string) (string, error) {
	if executable == "" {
		executable = "codex"
	}
	output, err := exec.CommandContext(ctx, executable, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("read %s version: %w", executable, err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (c *Client) writer() {
	for {
		select {
		case <-c.done:
			return
		case item := <-c.writeCh:
			data, err := json.Marshal(item.v)
			if err == nil {
				data = append(data, '\n')
				_, err = c.t.In.Write(data)
			}
			if item.err != nil {
				item.err <- err
			}
			if err != nil {
				c.fail(fmt.Errorf("write codex app-server message: %w", err))
				return
			}
		}
	}
}

// eventDispatcher prevents a slow event consumer from stopping the sole JSONL
// reader. The queue is intentionally lossless: dropping an unknown protocol
// notification would make forward compatibility worse than bounded memory.
func (c *Client) eventDispatcher() {
	var queue []Event
	for {
		if len(queue) == 0 {
			select {
			case <-c.done:
				return
			case event := <-c.eventIn:
				queue = append(queue, event)
			}
			continue
		}
		select {
		case <-c.done:
			return
		case event := <-c.eventIn:
			queue = append(queue, event)
		case c.events <- queue[0]:
			queue[0] = Event{}
			queue = queue[1:]
		}
	}
}

func (c *Client) emitEvent(event Event) {
	select {
	case <-c.done:
		return
	case c.eventIn <- event:
	}
}

// requestDispatcher gives approval and input coordinators a lossless stream
// without allowing a slow coordinator to block the JSONL reader.
func (c *Client) requestDispatcher() {
	var queue []Request
	for {
		if len(queue) == 0 {
			select {
			case <-c.done:
				return
			case request := <-c.requestIn:
				queue = append(queue, request)
			}
			continue
		}
		select {
		case <-c.done:
			return
		case request := <-c.requestIn:
			queue = append(queue, request)
		case c.reqs <- queue[0]:
			queue[0] = Request{}
			queue = queue[1:]
		}
	}
}

func (c *Client) emitRequest(request Request) {
	select {
	case <-c.done:
		return
	case c.requestIn <- request:
	}
}

func (c *Client) reader() {
	s := bufio.NewScanner(c.t.Out)
	s.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		c.handleLine(line)
	}
	if err := s.Err(); err != nil {
		c.fail(fmt.Errorf("read codex app-server message: %w", err))
	} else {
		c.fail(ErrClosed)
	}
}

func (c *Client) handleLine(line []byte) {
	var message struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(line, &message); err != nil {
		c.emitEvent(Event{Method: "adapter/malformed", Raw: json.RawMessage(line), Unknown: true, Err: err})
		return
	}
	if message.Method != "" {
		if len(message.ID) != 0 {
			request := newRequest(message.Method, message.ID, message.Params)
			c.mu.Lock()
			c.serverRequests[request.RequestID] = request
			c.mu.Unlock()
			c.emitRequest(request)
			return
		}
		event := newEvent(message.Method, message.Params)
		c.track(event)
		c.emitEvent(event)
		return
	}
	if len(message.ID) == 0 {
		c.emitEvent(Event{Method: "adapter/malformed", Raw: json.RawMessage(line), Unknown: true, Err: errors.New("message has neither method nor id")})
		return
	}
	var id int64
	if err := json.Unmarshal(message.ID, &id); err != nil {
		c.emitEvent(Event{Method: "adapter/unmatchedResponse", Raw: json.RawMessage(line), Unknown: true, Err: fmt.Errorf("non-numeric response id: %w", err)})
		return
	}
	c.mu.Lock()
	pending := c.pending[id]
	c.mu.Unlock()
	if pending == nil {
		c.emitEvent(Event{Method: "adapter/unmatchedResponse", Raw: json.RawMessage(line), Unknown: true})
		return
	}
	pending <- rpcResponse{ID: message.ID, Result: message.Result, Error: message.Error}
}

func (c *Client) fail(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.stop.Do(func() {
		c.mu.Lock()
		c.cause = err
		pending := c.pending
		c.pending = make(map[int64]chan rpcResponse)
		c.mu.Unlock()
		close(c.done)
		for _, response := range pending {
			response <- rpcResponse{Error: &RPCError{Code: -32000, Message: err.Error()}}
		}
		if c.t.Close != nil {
			_ = c.t.Close()
		}
	})
}

// Close ends the local transport and fails any calls that have not completed.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	return nil
}

func (c *Client) send(ctx context.Context, value any) error {
	ack := make(chan error, 1)
	item := outbound{v: value, err: ack}
	select {
	case <-c.done:
		return c.closedErr()
	case <-ctx.Done():
		return ctx.Err()
	case c.writeCh <- item:
	}
	select {
	case <-c.done:
		return c.closedErr()
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ack:
		return err
	}
}

func (c *Client) closedErr() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

func (c *Client) request(ctx context.Context, method string, params any, result any, allowUninitialized bool) error {
	if !allowUninitialized {
		c.mu.Lock()
		ready := c.ready
		c.mu.Unlock()
		if !ready {
			return ErrNotInitialized
		}
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	id := c.nextID.Add(1)
	response := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.cause != nil {
		err := c.cause
		c.mu.Unlock()
		return err
	}
	c.pending[id] = response
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(ctx, rpcRequest{Method: method, ID: id, Params: params}); err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.closedErr()
	case <-ctx.Done():
		return ctx.Err()
	case reply := <-response:
		if reply.Error != nil {
			if reply.Error.Code == -32601 {
				c.mu.Lock()
				c.methods[method] = false
				c.mu.Unlock()
				return &MethodUnavailableError{Method: method, RPC: reply.Error}
			}
			return reply.Error
		}
		if result != nil && len(reply.Result) != 0 {
			if err := json.Unmarshal(reply.Result, result); err != nil {
				return fmt.Errorf("decode %s response: %w", method, err)
			}
		}
		return nil
	}
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.config.RequestTimeout)
}

// Initialize performs the one-time handshake and then emits initialized.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	if c.ready {
		c.mu.Unlock()
		return ErrAlreadyInitialized
	}
	c.mu.Unlock()
	var reply InitializeInfo
	params := struct {
		ClientInfo ClientInfo `json:"clientInfo"`
	}{ClientInfo: c.config.ClientInfo}
	if err := c.request(ctx, "initialize", params, &reply, true); err != nil {
		return err
	}
	if err := c.send(ctx, rpcNotification{Method: "initialized", Params: struct{}{}}); err != nil {
		return err
	}
	c.mu.Lock()
	c.ready = true
	c.info = reply
	c.mu.Unlock()
	return nil
}

// Notify sends a post-handshake JSON-RPC notification for a capability not yet
// represented by a typed domain method.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return errors.New("notification method is required")
	}
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		return ErrNotInitialized
	}
	return c.send(ctx, rpcNotification{Method: method, Params: params})
}

// InitializeInfo is the negotiated app-server information returned by
// initialize.
type InitializeInfo struct {
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
	UserAgent      string `json:"userAgent"`
}

// MethodUnavailableError explains a required protocol method missing from the
// connected app-server version.
type MethodUnavailableError struct {
	Method string
	RPC    *RPCError
}

func (e *MethodUnavailableError) Error() string {
	return fmt.Sprintf("%s: %s", ErrMethodUnavailable, e.Method)
}

func (e *MethodUnavailableError) Unwrap() error { return ErrMethodUnavailable }

// Capabilities is the version-aware method availability view. A false method
// was observed as JSON-RPC -32601 on this connection.
type Capabilities struct {
	Initialized bool
	Methods     map[string]bool
}

// Capabilities returns a copy so callers cannot mutate client state.
func (c *Client) Capabilities() Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	methods := make(map[string]bool, len(c.methods))
	for method, available := range c.methods {
		methods[method] = available
	}
	return Capabilities{Initialized: c.ready, Methods: methods}
}

// Supports reports whether the adapter has not observed a method as missing.
func (c *Client) Supports(method string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.methods[method]
}

// InitializeInfo returns the handshake information received from app-server.
func (c *Client) InitializeInfo() (InitializeInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info, c.ready
}

// Reply sends a success response to an exact server request ID. It is the
// caller's responsibility to choose a result shape valid for Request.Method.
func (c *Client) Reply(ctx context.Context, request Request, result any) error {
	if len(request.ID) == 0 {
		return errors.New("reply requires a server request id")
	}
	tracked, err := c.takeRequest(request.RequestID)
	if err != nil {
		return err
	}
	return c.replyRaw(ctx, tracked.ID, result)
}

// ReplyError sends an exact JSON-RPC error response to a server request.
func (c *Client) ReplyError(ctx context.Context, request Request, code int, message string, data any) error {
	if len(request.ID) == 0 {
		return errors.New("reply requires a server request id")
	}
	tracked, err := c.takeRequest(request.RequestID)
	if err != nil {
		return err
	}
	return c.replyErrorRaw(ctx, tracked.ID, code, message, data)
}

// ApprovalResponse describes the worker's validated approval choice. For a
// permission grant, omit Permissions to grant exactly the requested subset;
// provide a JSON array only to grant a narrower subset.
type ApprovalResponse struct {
	// Decision is a scalar decision selected from Request.Decisions. For a
	// structured decision supplied by app-server, preserve it in DecisionValue.
	Decision      string
	DecisionValue json.RawMessage
	Permissions   json.RawMessage
	Scope         string
}

// ReplyApproval builds the version-specific response internally and consumes
// the tracked request before writing, preventing a second stale reply.
func (c *Client) ReplyApproval(ctx context.Context, requestID string, response ApprovalResponse) error {
	request, err := c.snapshotRequest(requestID)
	if err != nil {
		return err
	}
	if request.Kind != "approval_requested" {
		return errors.New("server request is not an approval")
	}
	var payload any
	switch request.ApprovalType {
	case "command_execution", "file_change":
		decision, err := approvalDecision(request, response)
		if err != nil {
			return err
		}
		payload = map[string]any{"decision": decision}
	case "permissions":
		var permissions json.RawMessage
		scope := response.Scope
		switch response.Decision {
		case "grant":
			permissions = response.Permissions
			if len(permissions) == 0 {
				permissions = request.Permissions
			}
		case "decline":
			permissions = json.RawMessage("{}")
		default:
			return fmt.Errorf("invalid permissions decision %q", response.Decision)
		}
		if !jsonObject(permissions) {
			return errors.New("permission grant requires requested permissions")
		}
		if scope == "" {
			scope = "turn"
		}
		if scope != "turn" && scope != "session" {
			return fmt.Errorf("invalid permission scope %q", scope)
		}
		payload = map[string]any{"permissions": permissions, "scope": scope}
	default:
		return fmt.Errorf("unsupported approval type %q", request.ApprovalType)
	}
	request, err = c.takeRequest(requestID)
	if err != nil {
		return err
	}
	return c.replyRaw(ctx, request.ID, payload)
}

func jsonObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return len(raw) != 0 && json.Unmarshal(raw, &object) == nil && object != nil
}

func approvalDecision(request Request, response ApprovalResponse) (json.RawMessage, error) {
	if len(response.DecisionValue) != 0 {
		for _, allowed := range request.DecisionValues {
			if string(allowed) == string(response.DecisionValue) {
				return cloneRaw(response.DecisionValue), nil
			}
		}
		return nil, fmt.Errorf("decision is not available for %s", request.ApprovalType)
	}
	if !contains(request.Decisions, response.Decision) {
		return nil, fmt.Errorf("invalid %s decision %q", request.ApprovalType, response.Decision)
	}
	return json.RawMessage(fmt.Sprintf("%q", response.Decision)), nil
}

// ReplyAnswers responds to a normalized request_user_input request.
func (c *Client) ReplyAnswers(ctx context.Context, requestID string, answers map[string][]string) error {
	request, err := c.snapshotRequest(requestID)
	if err != nil {
		return err
	}
	if request.Kind != "input_requested" {
		return errors.New("server request is not user input")
	}
	known := make(map[string]struct{}, len(request.Questions))
	for _, question := range request.Questions {
		known[question.ID] = struct{}{}
		values, present := answers[question.ID]
		if !present || len(values) == 0 {
			return fmt.Errorf("missing answer for question %q", question.ID)
		}
		for _, value := range values {
			if value == "" {
				return fmt.Errorf("empty answer for question %q", question.ID)
			}
		}
	}
	for id := range answers {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("answer for unknown question %q", id)
		}
	}
	payload := make(map[string]any, len(answers))
	for id, values := range answers {
		payload[id] = map[string]any{"answers": values}
	}
	request, err = c.takeRequest(requestID)
	if err != nil {
		return err
	}
	return c.replyRaw(ctx, request.ID, map[string]any{"answers": payload})
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (c *Client) takeRequest(requestID string) (Request, error) {
	c.mu.Lock()
	ready := c.ready
	request, found := c.serverRequests[requestID]
	if found {
		delete(c.serverRequests, requestID)
	}
	c.mu.Unlock()
	if !ready {
		return Request{}, ErrNotInitialized
	}
	if !found {
		return Request{}, ErrRequestNotPending
	}
	return request, nil
}

func (c *Client) snapshotRequest(requestID string) (Request, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready {
		return Request{}, ErrNotInitialized
	}
	request, found := c.serverRequests[requestID]
	if !found {
		return Request{}, ErrRequestNotPending
	}
	return request, nil
}

func (c *Client) replyRaw(ctx context.Context, id json.RawMessage, result any) error {
	return c.send(ctx, struct {
		ID     json.RawMessage `json:"id"`
		Result any             `json:"result"`
	}{ID: id, Result: result})
}

func (c *Client) replyErrorRaw(ctx context.Context, id json.RawMessage, code int, message string, data any) error {
	return c.send(ctx, struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data,omitempty"`
		} `json:"error"`
	}{ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	}{Code: code, Message: message, Data: data}})
}
