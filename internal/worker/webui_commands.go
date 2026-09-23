package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// gateway/command is a typed local bridge, not another native RPC escape hatch.
// The actor serializes it with Telegram commands and native lifecycle events.
// It has no durable command/outbox record: reconnecting cannot replay a mutation
// or deliver a browser-only result to a selected Telegram conversation.
type webUIActorCommand struct {
	history   *webUIHistoryRequest
	questions bool
	answer    *webUIQuestionAnswer
	ctx       context.Context
	runtime   protocol.Runtime
	session   protocol.Session
	name      string
	args      string
	reply     chan webUICommandReply
}

type webUICommandReply struct {
	history   json.RawMessage
	questions []webUIQuestion
	result    protocol.Result
	err       error
}

func webUICommandParams(params map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	for key := range params {
		if key != "name" && key != "args" {
			return nil, validationError("Unknown command parameter %q.", key)
		}
	}
	var name, args string
	if json.Unmarshal(params["name"], &name) != nil || name == "" || len(name) > 64 || !utf8.ValidString(name) {
		return nil, validationError("A Codex command name is required.")
	}
	if raw, ok := params["args"]; ok && (string(raw) == "null" || json.Unmarshal(raw, &args) != nil) {
		return nil, validationError("Command arguments must be text.")
	}
	if len(args) > 16<<10 || !utf8.ValidString(args) || strings.ContainsRune(args, 0) {
		return nil, validationError("Command arguments exceed the supported text limit.")
	}
	if !webUICommandSupported(name) {
		return nil, validationError("Unsupported Web UI command %q.", name)
	}
	return params, nil
}

func webUICommandSupported(name string) bool {
	switch name {
	case "status", "usage", "model", "reasoning", "permissions", "approvals", "fast", "plan", "personality", "compact", "review", "rename", "fork", "goal", "diff", "init", "mcp", "apps", "skills", "plugins", "hooks", "memories", "ps", "stop", "clean", "archive", "debug-config", "copy", "new", "delete", "pwd", "cwd":
		return true
	// These names are recognized by Codex, but belong to its terminal/local
	// integrations. Keep an explicit explanation rather than sending a prompt.
	case "approve", "side", "btw", "mention", "rollout", "app", "ide", "import", "logout", "feedback", "experimental", "raw", "keymap", "vim", "statusline", "title", "theme", "pets", "pet", "setup-default-sandbox", "sandbox-add-read-dir", "worktree", "recap", "voice", "agents", "export", "tui", "daemon", "warnings", "cd":
		return true
	default:
		return false
	}
}

func (r *webUIRelay) command(rpc webUIRPC) error {
	var name, args string
	_ = json.Unmarshal(rpc.Params["name"], &name)
	_ = json.Unmarshal(rpc.Params["args"], &args)
	ctx, cancel := context.WithTimeout(r.ctx, 45*time.Second)
	defer cancel()
	var result protocol.Result
	var err error
	if execute := r.pool.c.commandWebUI; execute != nil {
		result, err = execute(ctx, r.runtime, r.session, name, args)
	} else {
		err = validationError("Update this worker to enable Web UI commands.")
	}
	r.mu.Lock()
	delete(r.pending, webUIID(rpc.ID))
	r.mu.Unlock()
	reply := webUIRPC{ID: rpc.ID}
	if err != nil {
		message := "The command response was not confirmed. Inspect the session before retrying; it will not be replayed."
		var validation *CodexCommandValidationError
		var commandError *protocol.Error
		var rpcError *codexadapter.RPCError
		switch {
		case errors.As(err, &validation):
			message = safeErrorMessage(r.pool.c.store.redactor.Load(), validation.Message, "The command is invalid.")
		case errors.As(err, &commandError):
			message = safeErrorMessage(r.pool.c.store.redactor.Load(), commandError.Message, "The command is unavailable.")
		case errors.Is(err, ErrUpdatePrepared):
			message = "Worker update is in progress. Reconnect after it restarts."
		case errors.Is(err, codexadapter.ErrMethodUnavailable):
			message = "This Codex runtime does not support that command. Update Codex and retry."
		case errors.As(err, &rpcError):
			message = codexRPCMessage(r.pool.c.store.redactor.Load(), rpcError)
		}
		reply.Error, _ = json.Marshal(map[string]any{"code": -32000, "message": message})
	} else {
		reply.Result, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	data, err = redactWebUIJSON(r.pool.c.store.redactor.Load(), data)
	if err != nil {
		return err
	}
	return r.pool.send(r.ctx, protocol.WebUIFrame{ID: r.id, Action: "output", Data: data})
}

func (a *Agent) executeWebUICommand(ctx context.Context, runtime protocol.Runtime, session protocol.Session, name, args string) (protocol.Result, error) {
	if !webUICommandSupported(name) {
		return protocol.Result{}, validationError("Unsupported Web UI command.")
	}
	reply, err := a.dispatchWebUIActor(webUIActorCommand{ctx: ctx, runtime: runtime, session: session, name: name, args: strings.TrimSpace(args)})
	return reply.result, err
}

func (a *Agent) dispatchWebUIActor(request webUIActorCommand) (webUICommandReply, error) {
	ctx, runtime, session := request.ctx, request.runtime, request.session
	// Hold update admission through actor execution. An update cannot reserve
	// the runtime between validation and a command that starts work.
	if err := a.lockWebUIAdmission(ctx); err != nil {
		return webUICommandReply{}, err
	}
	defer a.updateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return webUICommandReply{}, err
	}
	if a.update != nil {
		return webUICommandReply{}, ErrUpdatePrepared
	}
	a.mu.RLock()
	actor := a.sessions[session.ID]
	a.mu.RUnlock()
	if actor == nil || actor.identityThread != session.ThreadID || actor.identityRuntime != runtime.ID {
		return webUICommandReply{}, validationError("Session is no longer available. Refresh the session list.")
	}
	request.reply = make(chan webUICommandReply, 1)
	select {
	case actor.webCommands <- request:
	case <-ctx.Done():
		return webUICommandReply{}, ctx.Err()
	case <-a.ctx.Done():
		return webUICommandReply{}, a.ctx.Err()
	}
	select {
	case reply := <-request.reply:
		return reply, reply.err
	case <-ctx.Done():
		return webUICommandReply{}, ctx.Err()
	case <-a.ctx.Done():
		return webUICommandReply{}, a.ctx.Err()
	}
}

// Telegram admission can hold this mutex while awaiting an actor. Browser
// work shares the update fence but must honor both caller and worker shutdown.
func (a *Agent) lockWebUIAdmission(ctx context.Context) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.ctx.Err(); err != nil {
			return err
		}
		if a.updateMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *sessionActor) webUICommand(request webUIActorCommand) (protocol.Result, error) {
	ctx, name, args := request.ctx, request.name, request.args
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	if request.runtime.ID != s.runtime.ID || request.runtime.Generation != s.runtime.Generation || request.session.ID != s.session.ID || request.session.ThreadID != s.session.ThreadID || request.session.WorkerID != s.agent.cfg.WorkerID || request.session.RuntimeID != s.runtime.ID || s.session.Archived || s.session.Deleted {
		return protocol.Result{}, validationError("The session or runtime changed. Refresh the session list.")
	}
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != request.runtime.Generation || runtime.State != "running" {
		return protocol.Result{}, validationError("The runtime changed or is unavailable. Reconnect to the session.")
	}
	cwd, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return protocol.Result{}, validationError("Session workspace is no longer allowed.")
	}
	needsIdle := name == "delete" || codexCommandNeedsIdle(name, args)
	busy := func() bool {
		return !updateSessionIdle(s.session) || s.activeCommand != nil || s.awaitingTurnStart || len(s.queue) != 0 || len(s.pending) != 0
	}
	if needsIdle && busy() {
		return protocol.Result{}, validationError("This session has an active turn or pending work. Wait for it to finish before running /%s.", name)
	}
	// Inventory and actor snapshots may trail an attached terminal. Validate
	// current native metadata again, and use a bounded one-turn state read for
	// mutations that require an idle session. No historical turn items fetched.
	var thread codexadapter.Thread
	if needsIdle {
		thread, err = client.ReadThreadState(ctx, s.session.ThreadID)
	} else {
		thread, err = client.ReadThread(ctx, s.session.ThreadID, false)
	}
	if err != nil {
		return protocol.Result{}, err
	}
	actualCWD, err := auth.CanonicalWorkspace(thread.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil || actualCWD != cwd || thread.ID != s.session.ThreadID || !thread.UserSession() {
		return protocol.Result{}, validationError("The session workspace or identity changed. Reconnect before running a command.")
	}
	if needsIdle && (busy() || thread.ActiveTurnID != "" || (thread.Status != "idle" && thread.Status != "notLoaded" && thread.Status != "systemError")) {
		return protocol.Result{}, validationError("This session has an active turn. Wait for it to finish before running /%s.", name)
	}
	var result protocol.Result
	switch name {
	case "new":
		result, err = s.webUINewSession(ctx, client, args)
	case "delete":
		if args != "confirm "+s.session.ThreadID {
			return protocol.Result{}, validationError("Deleting a session requires confirmation for this exact session. Its working directory and files will be kept.")
		}
		result, err = s.deleteSession(ctx, client, protocol.Command{SessionID: s.session.ID, ThreadID: s.session.ThreadID, RuntimeID: s.runtime.ID, RuntimeGeneration: s.runtime.Generation, Arguments: protocol.Arguments{CWD: s.session.CWD}})
	case "pwd", "cwd":
		if args != "" {
			return protocol.Result{}, usageError(name)
		}
		result.Text = cwd
	case "worktree", "recap", "voice", "agents", "export", "tui", "daemon", "warnings", "cd":
		result.Text = "The Web UI does not support /" + name + " through this runtime API. Use it in the Codex terminal on the worker."
	default:
		result, err = s.executeCodexCommand(ctx, client, name, args)
	}
	if err != nil {
		return protocol.Result{}, err
	}
	if result.State == "" {
		result.State = "completed"
	}
	if result.State == "running" && result.Session == nil {
		// /compact acknowledges before its turn ID exists. Preserve busy state
		// until native lifecycle notifications arrive without inventing a
		// Telegram command correlation or an unanswerable pending request.
		s.session.ActiveTurnID = result.TurnID
		s.session.State, s.session.Loaded = "running", true
		s.save()
	}
	return result, nil
}

func (s *sessionActor) webUINewSession(ctx context.Context, client *codexadapter.Client, args string) (protocol.Result, error) {
	var options struct {
		Name string `json:"name"`
		CWD  string `json:"cwd"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(args))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&options) != nil || decoder.Decode(new(any)) != io.EOF {
		return protocol.Result{}, validationError("New session expects a name and an optional existing working directory.")
	}
	name, err := protocol.NormalizeSessionName(options.Name)
	if err != nil || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return protocol.Result{}, validationError("Enter a session name of at most 120 characters.")
	}
	if options.CWD == "" {
		options.CWD = s.session.CWD
	}
	cwd, err := auth.CanonicalWorkspace(options.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return protocol.Result{}, validationError("Choose an existing working directory under this worker's allowed roots.")
	}
	// Official 0.156 cannot rejoin an empty paginated thread until its first
	// prompt materializes the source rollout. Its supported legacy contract
	// permits immediate browser attachment and still supports bounded reads.
	thread, err := client.StartThread(ctx, codexadapter.ThreadOptions{CWD: cwd, ApprovalPolicy: "on-request", Sandbox: "workspace-write", HistoryMode: "legacy"})
	if err != nil {
		return protocol.Result{}, err
	}
	actualCWD, err := auth.CanonicalWorkspace(thread.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil || actualCWD != cwd || thread.ID == s.session.ThreadID || !thread.UserSession() {
		return protocol.Result{}, validationError("Codex returned an unexpected session. Refresh the session list before retrying.")
	}
	if err := client.RenameThread(ctx, thread.ID, name); err != nil {
		return protocol.Result{}, err
	}
	thread.Name, thread.CWD = name, cwd
	created, err := s.agent.store.UpsertRuntimeSession(s.runtime, sessionFromThread(s.runtime, thread, true))
	if err != nil {
		return protocol.Result{}, err
	}
	if err := s.agent.emit(s.runtime, created.ID, "session_discovered", created); err != nil {
		return protocol.Result{}, err
	}
	s.agent.onSession(s.runtime, created)
	return protocol.Result{Text: "Created “" + name + "”.", Session: &created}, nil
}
