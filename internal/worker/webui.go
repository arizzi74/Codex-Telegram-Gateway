package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const maxWebUIConnections = 8

// Relays are deliberately connection-local: reconnecting only re-reads state,
// never replays a turn mutation. The attachment proxy continues observing an
// accepted turn even when the browser closes this client connection.
type webUIRelays struct {
	c          *Connection
	ctx        context.Context
	writes     chan<- outbound
	mu         sync.Mutex
	inputBytes int // queued and in-flight browser input, protected by mu
	clients    map[string]*webUIRelay
	wg         sync.WaitGroup
}

type webUIRelay struct {
	pool            *webUIRelays
	id              string
	session         protocol.Session
	runtime         protocol.Runtime
	ctx             context.Context
	cancel          context.CancelFunc
	inputBytes      int // protected by pool.mu
	input           chan json.RawMessage
	mu              sync.Mutex
	pending         map[string]string       // browser request ID -> method
	requests        map[string]webUIRequest // native server request ID -> original request
	commands        map[string]struct{}     // never replay a completed command on this connection
	historyFallback bool
	itemRequests    map[string]webUIHistoryRequest
	historyRequests chan webUIRPC // bounded work for the browser input loop
	observerReady   bool          // durable observer admitted this resume before its RPC
	resumed         bool          // actual thread metadata passed workspace checks
}

type webUIRequest struct {
	method string
	params map[string]json.RawMessage
}

func newWebUIRelays(c *Connection, ctx context.Context, writes chan<- outbound) *webUIRelays {
	return &webUIRelays{c: c, ctx: ctx, writes: writes, clients: make(map[string]*webUIRelay)}
}

func (p *webUIRelays) close() {
	p.mu.Lock()
	for _, client := range p.clients {
		client.cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *webUIRelays) send(ctx context.Context, frame protocol.WebUIFrame) error {
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.c.sendRedactedEnvelope(sendCtx, p.writes, "webui", frame)
}

func (p *webUIRelays) fail(id, message string) {
	_ = p.send(p.ctx, protocol.WebUIFrame{ID: id, Action: "error", Error: message})
}

func (p *webUIRelays) receive(frame protocol.WebUIFrame) {
	if err := frame.Validate(); err != nil {
		p.fail(frame.ID, "Invalid web UI frame.")
		return
	}
	switch frame.Action {
	case "open":
		runtime, session, err := p.target(frame)
		if err != nil {
			p.fail(frame.ID, err.Error())
			return
		}
		p.mu.Lock()
		if p.clients[frame.ID] != nil || len(p.clients) >= maxWebUIConnections {
			p.mu.Unlock()
			p.fail(frame.ID, "Too many web UI connections or connection already open.")
			return
		}
		ctx, cancel := context.WithCancel(p.ctx)
		client := &webUIRelay{pool: p, id: frame.ID, runtime: runtime, session: session, ctx: ctx, cancel: cancel, input: make(chan json.RawMessage, 4), historyRequests: make(chan webUIRPC, 4), pending: make(map[string]string), requests: make(map[string]webUIRequest)}
		p.clients[frame.ID] = client
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			defer func() {
				client.cancel()
				p.mu.Lock()
				p.inputBytes -= client.inputBytes
				client.inputBytes = 0
				delete(p.clients, frame.ID)
				p.mu.Unlock()
			}()
			if err := client.run(); err != nil && client.ctx.Err() == nil {
				p.fail(client.id, "Codex connection ended. Reconnect to refresh the session; submitted prompts are never replayed.")
			}
		}()
	case "input", "close":
		p.mu.Lock()
		client := p.clients[frame.ID]
		p.mu.Unlock()
		if client == nil {
			if frame.Action != "close" {
				p.fail(frame.ID, "Web UI connection is not open.")
			}
			return
		}
		if frame.Action == "close" {
			client.cancel()
			return
		}
		if !client.reserveInput(len(frame.Data)) {
			client.cancel()
			p.fail(frame.ID, "Web UI input exceeded the byte limit. Reconnect before sending another image.")
			return
		}
		select {
		case client.input <- append(json.RawMessage(nil), frame.Data...):
		case <-client.ctx.Done():
			client.releaseInput(len(frame.Data))
		default:
			client.releaseInput(len(frame.Data))
			client.cancel()
			p.fail(frame.ID, "Web UI input exceeded the connection limit. Reconnect to refresh the session.")
		}
	default:
		p.fail(frame.ID, "Unsupported web UI action.")
	}
}

func (r *webUIRelay) reserveInput(n int) bool {
	r.pool.mu.Lock()
	defer r.pool.mu.Unlock()
	if r.ctx.Err() != nil || r.pool.clients[r.id] != r || n > protocol.MaxWebUIInputBytes || r.inputBytes+n > 20<<20 || r.pool.inputBytes+n > 64<<20 {
		return false
	}
	r.inputBytes += n
	r.pool.inputBytes += n
	return true
}

func (r *webUIRelay) releaseInput(n int) {
	r.pool.mu.Lock()
	n = min(n, r.inputBytes)
	r.inputBytes -= n
	r.pool.inputBytes -= n
	r.pool.mu.Unlock()
}

func (p *webUIRelays) target(frame protocol.WebUIFrame) (protocol.Runtime, protocol.Session, error) {
	var runtime protocol.Runtime
	for _, candidate := range p.c.snapshot() {
		if candidate.ID == frame.RuntimeID {
			runtime = candidate
			break
		}
	}
	if runtime.ID == "" || runtime.Generation != frame.RuntimeGeneration || runtime.State != "running" || !filepath.IsAbs(runtime.LocalSocket) {
		return runtime, protocol.Session{}, errors.New("The runtime changed or is unavailable. Refresh the session list.")
	}
	if runtime.WorkerID != "" && runtime.WorkerID != p.c.cfg.WorkerID {
		return runtime, protocol.Session{}, errors.New("Runtime belongs to another worker.")
	}
	sessions, err := p.c.store.ListSessions(runtime.ID)
	if err != nil {
		return runtime, protocol.Session{}, errors.New("Session inventory is unavailable.")
	}
	for _, session := range sessions {
		if session.ID != frame.SessionID || session.WorkerID != p.c.cfg.WorkerID || session.Archived || session.Deleted || session.ThreadID == "" {
			continue
		}
		if _, err := auth.CanonicalWorkspace(session.CWD, p.c.cfg.AllowedWorkspaceRoots); err != nil {
			return runtime, session, errors.New("Session workspace is not allowed.")
		}
		return runtime, session, nil
	}
	return runtime, protocol.Session{}, errors.New("Session is unavailable or no longer belongs to this runtime.")
}

func (r *webUIRelay) run() error {
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", r.runtime.LocalSocket)
	}}
	defer transport.CloseIdleConnections()
	handshake, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	conn, _, err := websocket.Dial(handshake, "ws://codex.local/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		cancel()
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes - (64 << 10))
	initialize := map[string]any{"id": "webui-initialize", "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "telegramgw_webui", "title": "Codex Gateway Web UI", "version": buildinfo.Version}, "capabilities": map[string]any{"experimentalApi": true}}}
	data, _ := json.Marshal(initialize)
	if err = conn.Write(handshake, websocket.MessageText, data); err == nil {
		var kind websocket.MessageType
		kind, data, err = conn.Read(handshake)
		if err == nil {
			var reply struct {
				ID     string          `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result json.RawMessage `json:"result"`
			}
			if kind != websocket.MessageText || json.Unmarshal(data, &reply) != nil || reply.ID != "webui-initialize" || len(reply.Result) == 0 || (len(reply.Error) > 0 && string(reply.Error) != "null") {
				err = errors.New("invalid app-server initialization")
			}
		}
	}
	if err == nil {
		err = conn.Write(handshake, websocket.MessageText, []byte(`{"method":"initialized"}`))
	}
	cancel()
	if err != nil {
		return err
	}
	ready, _ := json.Marshal(map[string]any{"session": r.session, "runtime": r.runtime})
	if err = r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "ready", Data: ready}); err != nil {
		return err
	}
	readDone := make(chan error, 1)
	go func() { readDone <- r.read(conn) }()
	defer func() { conn.CloseNow(); <-readDone }()
	for {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case err := <-readDone:
			readDone <- err
			return err
		case data := <-r.input:
			err := r.forwardInput(conn, data)
			r.releaseInput(len(data))
			if err != nil {
				return err
			}
		case request := <-r.historyRequests:
			if err := r.history(request); err != nil {
				return err
			}
		}
	}
}

func (r *webUIRelay) forwardInput(conn *websocket.Conn, data []byte) error {
	forward, err := r.clientMessage(data)
	if err != nil {
		if sendErr := r.reject(data, err); sendErr != nil {
			return sendErr
		}
		return nil
	}
	// Revalidate the immutable target immediately before every client RPC.
	if _, _, err := r.pool.target(protocol.WebUIFrame{RuntimeID: r.runtime.ID, RuntimeGeneration: r.runtime.Generation, SessionID: r.session.ID}); err != nil {
		return err
	}
	var rpc webUIRPC
	if json.Unmarshal(forward, &rpc) == nil && rpc.Method == "thread/resume" {
		// Subscribe on the existing input loop before writing the browser RPC.
		// The native reader remains available while admission or a session
		// actor is busy, and no prompt is enabled before both checks finish.
		if observe := r.pool.c.observeWebUI; observe != nil {
			ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
			err := observe(ctx, r.runtime, r.session)
			cancel()
			if err != nil {
				return err
			}
		}
		r.mu.Lock()
		r.observerReady = true
		r.mu.Unlock()
	}
	if json.Unmarshal(forward, &rpc) == nil && rpc.Method == "thread/items/list" {
		r.mu.Lock()
		fallback := r.historyFallback
		r.mu.Unlock()
		if fallback {
			if err := r.history(rpc); err != nil {
				return err
			}
			return nil
		}
	}
	if json.Unmarshal(forward, &rpc) == nil && (rpc.Method == "gateway/questions" || rpc.Method == "gateway/answer") {
		if err := r.questions(rpc); err != nil {
			return err
		}
		return nil
	}
	if json.Unmarshal(forward, &rpc) == nil && rpc.Method == "gateway/command" {
		if err := r.command(rpc); err != nil {
			return err
		}
		return nil
	}
	writeCtx, stop := context.WithTimeout(r.ctx, 10*time.Second)
	err = conn.Write(writeCtx, websocket.MessageText, forward)
	stop()
	if err != nil {
		return err
	}
	return nil
}

func (r *webUIRelay) read(conn *websocket.Conn) error {
	for {
		kind, data, err := conn.Read(r.ctx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText {
			return errors.New("unexpected app-server frame")
		}
		forward, err := r.serverMessage(data)
		if err != nil {
			return err
		}
		if len(forward) == 0 {
			continue
		}
		forward, err = redactWebUIJSON(r.pool.c.store.redactor.Load(), forward)
		if err != nil {
			return err
		}
		if err := r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: forward}); err != nil {
			return err
		}
	}
}

type webUIRPC struct {
	JSONRPC string                     `json:"jsonrpc,omitempty"`
	ID      json.RawMessage            `json:"id,omitempty"`
	Method  string                     `json:"method,omitempty"`
	Params  map[string]json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage            `json:"result,omitempty"`
	Error   json.RawMessage            `json:"error,omitempty"`
}

func webUIID(id json.RawMessage) string {
	if len(id) == 0 || len(id) > 256 || string(id) == "null" {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return ""
	}
	switch value := value.(type) {
	case string:
		if value != "" {
			return "s:" + value
		}
	case json.Number:
		if _, err := value.Int64(); err == nil {
			return "n:" + string(value)
		}
	}
	return ""
}

func (r *webUIRelay) clientMessage(data []byte) ([]byte, error) {
	if len(data) > protocol.MaxWebUIInputBytes {
		return nil, errors.New("Web request exceeds the 15 MiB limit.")
	}
	var rpc webUIRPC
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rpc); err != nil {
		return nil, errors.New("Invalid app-server request.")
	}
	if rpc.JSONRPC != "" && rpc.JSONRPC != "2.0" {
		return nil, errors.New("Invalid JSON-RPC version.")
	}
	id := webUIID(rpc.ID)
	if id == "" {
		return nil, errors.New("A bounded request ID is required.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rpc.Method == "" {
		request, ok := r.requests[id]
		if !r.resumed || !ok || len(rpc.Result) == 0 || len(rpc.Error) > 0 || rpc.Params != nil {
			return nil, errors.New("This question or approval is no longer pending on this connection.")
		}
		var redactor *auth.Redactor
		if r.pool != nil {
			redactor = r.pool.c.store.redactor.Load()
		}
		result, err := webUIAnswer(request, rpc.Result, redactor)
		if err != nil {
			return nil, err
		}
		rpc.Result = result
		delete(r.requests, id)
		return json.Marshal(rpc)
	}
	if len(rpc.Result) > 0 || len(rpc.Error) > 0 || r.pending[id] != "" || len(r.pending) >= 64 {
		return nil, errors.New("Invalid or duplicate request.")
	}
	params, err := webUIParams(rpc.Method, rpc.Params, r.session.ThreadID)
	if err != nil {
		return nil, err
	}
	switch rpc.Method {
	case "gateway/command", "gateway/questions", "gateway/answer", "turn/start", "turn/steer", "turn/interrupt", "thread/turns/list", "thread/items/list", "account/rateLimits/read":
		// Stored inventory is a discovery snapshot. An existing terminal can
		// have moved the thread since then, so never admit browser mutations
		// until this connection has checked the actual resume response.
		if !r.resumed {
			return nil, errors.New("Resume the session before reading history or usage limits, or controlling a turn.")
		}
		if rpc.Method == "gateway/command" || rpc.Method == "gateway/answer" {
			if _, used := r.commands[id]; used || len(r.commands) >= 1024 {
				return nil, errors.New("This command ID has already been used or the connection command limit was reached. Commands are never replayed.")
			}
			if r.commands == nil {
				r.commands = make(map[string]struct{})
			}
			r.commands[id] = struct{}{}
		}
	case "thread/resume":
		for _, method := range r.pending {
			if method == "thread/resume" {
				return nil, errors.New("Session resume is already in progress.")
			}
		}
		r.observerReady = false
		r.resumed = false
	}
	rpc.Params = params
	if rpc.Method == "thread/items/list" {
		var request webUIHistoryRequest
		_ = json.Unmarshal(params["cursor"], &request.Cursor)
		_ = json.Unmarshal(params["sortDirection"], &request.Direction)
		_ = json.Unmarshal(params["limit"], &request.Limit)
		if r.itemRequests == nil {
			r.itemRequests = make(map[string]webUIHistoryRequest)
		}
		r.itemRequests[id] = request
	}
	r.pending[id] = rpc.Method
	return json.Marshal(rpc)
}

func webUIParams(method string, params map[string]json.RawMessage, thread string) (map[string]json.RawMessage, error) {
	if method == "gateway/command" {
		return webUICommandParams(params)
	}
	if method == "gateway/questions" || method == "gateway/answer" {
		return webUIQuestionParams(method, params)
	}
	allowed := map[string]bool{"threadId": true}
	switch method {
	case "thread/resume":
	case "thread/read":
		allowed["includeTurns"] = true
	case "thread/turns/list":
		for _, key := range []string{"cursor", "limit", "sortDirection", "itemsView"} {
			allowed[key] = true
		}
	case "thread/items/list":
		for _, key := range []string{"cursor", "limit", "sortDirection"} {
			allowed[key] = true
		}
	case "model/list":
		allowed = map[string]bool{"cursor": true, "limit": true}
	case "account/rateLimits/read":
		allowed = map[string]bool{"excludeResetCreditDetails": true}
	case "turn/start":
		for _, key := range []string{"input", "model", "effort"} {
			allowed[key] = true
		}
	case "turn/steer":
		allowed["input"], allowed["expectedTurnId"] = true, true
	case "turn/interrupt":
		allowed["turnId"] = true
	default:
		return nil, errors.New("This app-server method is not available in the web UI.")
	}
	if params == nil {
		params = make(map[string]json.RawMessage)
	}
	for key, raw := range params {
		if !allowed[key] {
			return nil, fmt.Errorf("Parameter %s is not available in the web UI.", key)
		}
		switch key {
		case "excludeResetCreditDetails":
			var exclude bool
			if json.Unmarshal(raw, &exclude) != nil || !exclude {
				return nil, errors.New("Reset credit details are not available in the web UI.")
			}
		case "threadId":
			var value string
			if json.Unmarshal(raw, &value) != nil || (value != "" && value != thread) {
				return nil, errors.New("Request targets another session.")
			}
		case "limit":
			var value int
			if json.Unmarshal(raw, &value) != nil || value < 1 || value > 100 {
				return nil, errors.New("History and model pages must contain 1–100 entries.")
			}
			if method == "thread/items/list" && value > 20 {
				return nil, errors.New("Conversation pages are limited to 20 items.")
			}
		case "includeTurns":
			var value bool
			if json.Unmarshal(raw, &value) != nil {
				return nil, errors.New("Invalid history option.")
			}
			if value {
				return nil, errors.New("Use paginated thread/turns/list to read conversation history.")
			}
		case "input":
			var input []map[string]json.RawMessage
			if json.Unmarshal(raw, &input) != nil || len(input) == 0 || len(input) > 16 {
				return nil, errors.New("Text or an image is required (at most 16 input parts).")
			}
			textBytes, images := 0, 0
			for _, entry := range input {
				var kind string
				if len(entry) != 2 || json.Unmarshal(entry["type"], &kind) != nil {
					return nil, errors.New("Only text and uploaded images are supported.")
				}
				switch kind {
				case "text":
					var text string
					if json.Unmarshal(entry["text"], &text) != nil || strings.TrimSpace(text) == "" {
						return nil, errors.New("Text input must not be empty.")
					}
					textBytes += len(text)
					if textBytes > protocol.MaxWebUITextBytes {
						return nil, errors.New("Total prompt text exceeds the 256 KiB limit.")
					}
				case "image":
					images++
					if images > protocol.MaxImageCount {
						return nil, errors.New("Only one image can be sent per prompt.")
					}
					var url string
					if json.Unmarshal(entry["url"], &url) != nil {
						return nil, errors.New("An inline image data URL is required.")
					}
					if err := protocol.ValidateWebUIImageURL(url); err != nil {
						return nil, err
					}
				default:
					return nil, errors.New("Only text and uploaded PNG, JPEG, or GIF images are supported.")
				}
			}
		default:
			var value string
			if json.Unmarshal(raw, &value) != nil || len(value) > 4096 {
				return nil, errors.New("Invalid app-server request option.")
			}
			if key == "sortDirection" && value != "asc" && value != "desc" {
				return nil, errors.New("Invalid history sort direction.")
			}
			if key == "itemsView" && value != "full" && value != "notLoaded" {
				return nil, errors.New("Invalid history view.")
			}
			if key == "effort" && value != "none" && value != "minimal" && value != "low" && value != "medium" && value != "high" && value != "xhigh" && value != "max" && value != "ultra" {
				return nil, errors.New("Invalid reasoning effort.")
			}
		}
	}
	if method != "model/list" && method != "account/rateLimits/read" {
		params["threadId"], _ = json.Marshal(thread)
	}
	if method == "account/rateLimits/read" {
		params["excludeResetCreditDetails"] = json.RawMessage("true")
	}
	if method == "thread/read" {
		params["includeTurns"] = json.RawMessage("false")
	}
	if method == "thread/resume" {
		params["excludeTurns"] = json.RawMessage("true")
	}
	if method == "thread/turns/list" || method == "thread/items/list" {
		if params["limit"] == nil {
			params["limit"] = json.RawMessage("20")
		}
		if params["sortDirection"] == nil {
			params["sortDirection"] = json.RawMessage(`"desc"`)
		}
	}
	if method == "turn/start" || method == "turn/steer" {
		if params["input"] == nil {
			return nil, errors.New("Text input is required.")
		}
	}
	for _, key := range []string{"turnId", "expectedTurnId"} {
		if (method == "turn/interrupt" && key == "turnId") || (method == "turn/steer" && key == "expectedTurnId") {
			var value string
			if json.Unmarshal(params[key], &value) != nil || value == "" {
				return nil, errors.New("An explicit active turn ID is required.")
			}
		}
	}
	return params, nil
}

func (r *webUIRelay) serverMessage(data []byte) ([]byte, error) {
	var rpc webUIRPC
	if json.Unmarshal(data, &rpc) != nil {
		return nil, errors.New("invalid app-server output")
	}
	id := webUIID(rpc.ID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if rpc.Method == "" {
		if id == "" || r.pending[id] == "" {
			return nil, nil
		}
		method := r.pending[id]
		if method == "gateway/command" || method == "gateway/questions" || method == "gateway/answer" {
			return nil, nil // Only the session actor may complete local commands.
		}
		if method == "thread/items/list" {
			request, exists := r.itemRequests[id]
			var failure struct {
				Code int `json:"code"`
			}
			if exists && r.pool != nil && r.pool.c.historyWebUI != nil && json.Unmarshal(rpc.Error, &failure) == nil && failure.Code == -32601 {
				r.historyFallback = true
				if r.historyRequests != nil {
					// Keep the request reserved until the input loop completes it.
					// A slow legacy history read must not stall native events.
					select {
					case r.historyRequests <- webUIRPC{ID: rpc.ID}:
						return nil, nil
					default:
						return nil, errors.New("too many pending conversation requests")
					}
				}
				delete(r.itemRequests, id)
				delete(r.pending, id)
				r.mu.Unlock()
				result, err := r.historyResult(rpc.ID, request)
				r.mu.Lock()
				return result, err
			}
			delete(r.itemRequests, id)
		}
		delete(r.pending, id)
		if method == "thread/items/list" && len(rpc.Result) > 0 && (len(rpc.Error) == 0 || string(rpc.Error) == "null") {
			var redactor *auth.Redactor
			if r.pool != nil {
				redactor = r.pool.c.store.redactor.Load()
			}
			result, err := webUIItemPage(rpc.Result, redactor)
			if err != nil {
				return nil, err
			}
			return json.Marshal(webUIRPC{JSONRPC: rpc.JSONRPC, ID: rpc.ID, Result: result})
		}
		if method == "account/rateLimits/read" {
			// Account limits belong to the worker's authenticated Codex runtime,
			// not a thread. Expose only the quota fields needed for status; never
			// relay account details, reset-credit records, or future additions.
			result, err := webUIRateLimitsPayload(rpc.Result)
			if !r.resumed || err != nil || (len(rpc.Error) > 0 && string(rpc.Error) != "null") {
				var nativeError struct {
					Code int `json:"code"`
				}
				if json.Unmarshal(rpc.Error, &nativeError) != nil || nativeError.Code == 0 {
					nativeError.Code = -32000
				}
				failure, _ := json.Marshal(map[string]any{"code": nativeError.Code, "message": "Codex usage limits are unavailable."})
				return json.Marshal(webUIRPC{JSONRPC: rpc.JSONRPC, ID: rpc.ID, Error: failure})
			}
			return json.Marshal(webUIRPC{JSONRPC: rpc.JSONRPC, ID: rpc.ID, Result: result})
		}
		if (method == "thread/resume" || method == "thread/read") && len(rpc.Result) > 0 {
			var result struct {
				CWD    *string `json:"cwd"`
				Thread struct {
					ID             string          `json:"id"`
					CWD            string          `json:"cwd"`
					Source         json.RawMessage `json:"source"`
					ThreadSource   string          `json:"threadSource"`
					ParentThreadID string          `json:"parentThreadId"`
					Ephemeral      bool            `json:"ephemeral"`
				} `json:"thread"`
			}
			if json.Unmarshal(rpc.Result, &result) != nil || result.Thread.ID != r.session.ThreadID {
				return nil, errors.New("app-server returned another thread")
			}
			var source string
			if len(result.Thread.Source) > 0 && string(result.Thread.Source) != "null" && json.Unmarshal(result.Thread.Source, &source) != nil {
				source = "unknown"
			}
			if !(codexadapter.Thread{Source: source, ThreadSource: result.Thread.ThreadSource, ParentThreadID: result.Thread.ParentThreadID, Ephemeral: result.Thread.Ephemeral}).UserSession() {
				return nil, errors.New("internal sessions cannot be opened in the web UI")
			}
			currentCWD := result.Thread.CWD
			if method == "thread/resume" && result.CWD != nil {
				currentCWD = *result.CWD
			}
			if currentCWD == "" {
				return nil, errors.New("app-server omitted the current session workspace")
			}
			if _, err := auth.CanonicalWorkspace(currentCWD, r.pool.c.cfg.AllowedWorkspaceRoots); err != nil {
				return nil, errors.New("session workspace is no longer allowed")
			}
			if method == "thread/resume" {
				if r.pool.c.observeWebUI != nil && !r.observerReady {
					return nil, errors.New("web UI observer is not ready")
				}
				r.resumed = true
			}
		}
		return data, nil
	}
	thread := webUIThread(rpc.Params)
	if rpc.Method == "account/rateLimits/updated" {
		// This is the sole permitted account-wide notification. Accept it
		// only for a validated session and keep it a notification, never an
		// actionable server request. No background rate-limit polling is used.
		if !r.resumed || len(rpc.ID) > 0 || (thread != "" && thread != r.session.ThreadID) {
			return nil, nil
		}
		raw, err := json.Marshal(rpc.Params)
		if err != nil {
			return nil, nil
		}
		visible, err := webUIRateLimitsPayload(raw)
		if err != nil {
			return nil, nil
		}
		var params map[string]json.RawMessage
		if json.Unmarshal(visible, &params) != nil {
			return nil, nil
		}
		return json.Marshal(webUIRPC{JSONRPC: rpc.JSONRPC, Method: rpc.Method, Params: params})
	}
	if !r.resumed || thread != r.session.ThreadID {
		return nil, nil
	}
	// Regexes cannot safely redact a secret split between independent delta
	// frames. Match the established Telegram boundary: completed messages and
	// complete tool calls/output are emitted, while raw content deltas stay on
	// the worker. Status, item lifecycle, and questions are still live.
	if strings.Contains(strings.ToLower(rpc.Method), "delta") {
		return nil, nil
	}
	if rpc.Method == "serverRequest/resolved" {
		for _, key := range []string{"requestId", "request_id"} {
			if request := webUIID(rpc.Params[key]); request != "" {
				delete(r.requests, request)
			}
		}
	}
	if rpc.Method == "turn/completed" || rpc.Method == "thread/deleted" {
		clear(r.requests)
	}
	if id != "" {
		switch rpc.Method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput", "item/permissions/requestApproval":
		default:
			return nil, nil
		}
		if len(r.requests) >= 64 {
			return nil, errors.New("too many pending app-server requests")
		}
		r.requests[id] = webUIRequest{method: rpc.Method, params: rpc.Params}
	}
	return data, nil
}

// Preserve the native shape so the browser can merge sparse rolling updates
// into the initial snapshot, including the optional multi-bucket view. Pointers
// preserve missing/null fields without inventing zero usage or a plan type.
type webUIRateLimitWindow struct {
	UsedPercent        *int   `json:"usedPercent,omitempty"`
	WindowDurationMins *int64 `json:"windowDurationMins,omitempty"`
	ResetsAt           *int64 `json:"resetsAt,omitempty"`
}

type webUIRateLimitSnapshot struct {
	LimitID   string                `json:"limitId,omitempty"`
	LimitName string                `json:"limitName,omitempty"`
	Primary   *webUIRateLimitWindow `json:"primary,omitempty"`
	Secondary *webUIRateLimitWindow `json:"secondary,omitempty"`
	PlanType  string                `json:"planType,omitempty"`
}

func webUIRateLimitsPayload(raw json.RawMessage) (json.RawMessage, error) {
	var result *struct {
		RateLimits           *webUIRateLimitSnapshot            `json:"rateLimits,omitempty"`
		RateLimitsByLimitID  map[string]*webUIRateLimitSnapshot `json:"rateLimitsByLimitId,omitempty"`
		OrdinaryUsageAllowed *bool                              `json:"ordinaryUsageAllowed,omitempty"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return nil, errors.New("invalid account rate-limit data")
	}
	return json.Marshal(result)
}

func webUIThread(params map[string]json.RawMessage) string {
	for _, key := range []string{"threadId", "thread_id"} {
		var value string
		if json.Unmarshal(params[key], &value) == nil && value != "" {
			return value
		}
	}
	var thread struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(params["thread"], &thread)
	return thread.ID
}

func webUIAnswer(request webUIRequest, raw json.RawMessage, redactor *auth.Redactor) (json.RawMessage, error) {
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, errors.New("Invalid question or approval response.")
	}
	switch request.method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		var decision string
		if len(result) != 1 || json.Unmarshal(result["decision"], &decision) != nil {
			return nil, errors.New("Choose an approval decision.")
		}
		switch decision {
		case "accept", "acceptForSession", "decline", "cancel":
		default:
			return nil, errors.New("Unsupported approval decision.")
		}
	case "item/permissions/requestApproval":
		// The browser offers only a grant for this turn, never a durable policy
		// edit. Use the exact requested local permissions to handle redacted
		// paths and prevent adding unrelated permissions in a crafted response.
		for key := range result {
			if key != "permissions" && key != "scope" {
				return nil, errors.New("Invalid permission response.")
			}
		}
		var scope string
		if value := result["scope"]; len(value) > 0 && (json.Unmarshal(value, &scope) != nil || scope != "turn") {
			return nil, errors.New("Web permission grants are limited to the current turn.")
		}
		var grant map[string]any
		if json.Unmarshal(result["permissions"], &grant) != nil || grant == nil {
			return nil, errors.New("Invalid requested permissions.")
		}
		if len(grant) > 0 {
			requested := request.params["permissions"]
			visible, err := redactWebUIJSON(redactor, requested)
			if err != nil || !webUIEqualJSON(result["permissions"], visible) {
				return nil, errors.New("Only the permissions requested by Codex may be granted.")
			}
			result["permissions"] = requested
		}
		result["scope"] = json.RawMessage(`"turn"`)
	case "item/tool/requestUserInput":
		if len(result) != 1 {
			return nil, errors.New("Invalid question response.")
		}
		var questions []struct {
			ID      string `json:"id"`
			Options []struct {
				Label string `json:"label"`
			} `json:"options"`
		}
		var answers map[string]struct {
			Answers []string `json:"answers"`
		}
		if json.Unmarshal(request.params["questions"], &questions) != nil || json.Unmarshal(result["answers"], &answers) != nil || len(answers) == 0 {
			return nil, errors.New("A text or option answer is required.")
		}
		known := make(map[string]bool)
		for _, question := range questions {
			known[question.ID] = true
			answer, ok := answers[question.ID]
			if !ok {
				continue
			}
			if len(answer.Answers) == 0 || len(answer.Answers) > 16 {
				return nil, errors.New("A bounded question answer is required.")
			}
			labels := make([]string, len(question.Options))
			for i, option := range question.Options {
				labels[i] = option.Label
			}
			visible := redactedQuestionOptions(redactor, labels)
			for i, text := range answer.Answers {
				if len(text) > 256<<10 {
					return nil, errors.New("Question answer is too long.")
				}
				for j, label := range visible {
					if text == label {
						answer.Answers[i] = labels[j]
						break
					}
				}
			}
			answers[question.ID] = answer
		}
		for id := range answers {
			if !known[id] {
				return nil, errors.New("Answer targets an unknown question.")
			}
		}
		result["answers"], _ = json.Marshal(answers)
	default:
		return nil, errors.New("Unsupported native question type.")
	}
	return json.Marshal(result)
}

func webUIEqualJSON(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}

func (r *webUIRelay) reject(data []byte, cause error) error {
	var rpc webUIRPC
	_ = json.Unmarshal(data, &rpc)
	if webUIID(rpc.ID) == "" {
		return r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "error", Error: "Invalid web UI request."})
	}
	body, _ := json.Marshal(map[string]any{"id": rpc.ID, "error": map[string]any{"code": -32602, "message": cause.Error()}})
	return r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: body})
}

// Native events add new content field names over time. Redact every string at
// this remote boundary except protocol identities and enums, rather than only
// the smaller normalized Telegram event vocabulary. Never expose rollout files.
func redactWebUIJSON(redactor *auth.Redactor, data []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var visit func(any, string) any
	visit = func(value any, key string) any {
		switch value := value.(type) {
		case string:
			switch key {
			case "id", "approvalId", "approval_id", "threadId", "thread_id", "turnId", "turn_id", "itemId", "item_id", "requestId", "request_id", "method", "type", "status", "phase", "role", "reasoningEffort", "effort", "cursor", "nextCursor", "backwardsCursor", "prevCursor", "turnsBackwardsCursor", "itemsBackwardsCursor":
				return value
			}
			if redactor != nil {
				return redactor.Redact(value)
			}
			return value
		case map[string]any:
			if value["type"] == "image" || value["type"] == "localImage" {
				// Native user-message echoes/history contain the original inline
				// bytes or a local path. The browser and Telegram transcript need
				// a placeholder, never another copy of the attachment or its path.
				return map[string]any{"type": "text", "text": "[Image]"}
			}
			if value["type"] == "reasoning" {
				// Only public summaries belong in the remote UI. Raw reasoning
				// content and provider/encrypted additions remain on the worker.
				for field := range value {
					if field != "id" && field != "type" && field != "summary" && field != "createdAt" && field != "status" {
						delete(value, field)
					}
				}
			}
			for field, entry := range value {
				if (key == "thread" && field == "path") || field == "rolloutPath" {
					delete(value, field)
					continue
				}
				value[field] = visit(entry, field)
			}
			return value
		case []any:
			// Preserve distinct selectable labels when different sensitive
			// options redact to the same text. The original labels remain local.
			var labels []string
			if key == "options" && len(value) > 0 {
				for _, entry := range value {
					option, ok := entry.(map[string]any)
					if !ok {
						labels = nil
						break
					}
					label, ok := option["label"].(string)
					if !ok {
						labels = nil
						break
					}
					labels = append(labels, label)
				}
				if len(labels) > 0 {
					labels = redactedQuestionOptions(redactor, labels)
				}
			}
			for i, entry := range value {
				value[i] = visit(entry, key)
				if len(labels) > 0 {
					value[i].(map[string]any)["label"] = labels[i]
				}
			}
			return value
		default:
			return value
		}
	}
	return json.Marshal(visit(value, ""))
}
