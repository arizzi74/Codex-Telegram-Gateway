package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Agent joins runtime supervision, independent session actors, and the durable
// transport. The network context never owns a runtime or an accepted turn.
type Agent struct {
	cfg           config.WorkerConfig
	store         *Store
	log           *slog.Logger
	manager       *RuntimeManager
	ctx           context.Context
	mu            sync.RWMutex
	group         sync.WaitGroup
	sessions      map[string]*sessionActor
	creates       map[string]chan protocol.Command
	fatal         chan error
	redactor      *auth.Redactor
	newConnection func(config.WorkerConfig, *Store, *slog.Logger, func() []protocol.Runtime, func(context.Context, protocol.Command) (protocol.CommandAck, error)) (*Connection, error)
}

func NewAgent(cfg config.WorkerConfig, store *Store, logger *slog.Logger) (*Agent, error) {
	if cfg.MaxQueuedTurns == 0 {
		cfg.MaxQueuedTurns = 20
	}
	if logger == nil {
		logger = slog.Default()
	}
	redactor, err := auth.NewRedactor(append([]string{`cwk_[a-fA-F0-9]{64}`, `sk-[A-Za-z0-9_-]{20,}`, `[0-9]{6,12}:[A-Za-z0-9_-]{30,}`}, cfg.RedactPatterns...), "")
	if err != nil {
		return nil, err
	}
	a := &Agent{cfg: cfg, store: store, log: logger, sessions: map[string]*sessionActor{}, creates: map[string]chan protocol.Command{}, fatal: make(chan error, 1), redactor: redactor, newConnection: NewConnection}
	a.manager, err = NewRuntimeManager(cfg, store, logger, RuntimeHooks{OnSession: a.onSession, OnEvent: a.onEvent, OnRequest: a.onRequest, OnError: a.report})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.ctx = ctx
	runtimeDone := make(chan error, 1)
	go func() { runtimeDone <- a.manager.Run(ctx) }()
	defer func() { cancel(); <-runtimeDone; a.group.Wait() }()
	select {
	case <-ctx.Done():
		return nil
	case <-a.manager.Ready():
	case err := <-a.fatal:
		return err
	}
	pending, err := a.store.PendingCommands()
	if err != nil {
		return err
	}
	// A worker restart changes every live runtime generation; recovered commands
	// are routed through the same stale-generation validation as new delivery.
	for _, record := range pending {
		if _, err := a.dispatch(ctx, record.Command); err != nil {
			return err
		}
	}
	conn, err := a.newConnection(a.cfg, a.store, a.log, a.manager.Snapshot, a.HandleCommand)
	if err != nil {
		return err
	}
	networkDone := make(chan error, 1)
	a.group.Add(1)
	go func() { defer a.group.Done(); a.statusLoop(ctx, conn) }()
	a.group.Go(func() { networkDone <- conn.Run(ctx) })
	select {
	case <-ctx.Done():
		return nil
	case err := <-a.fatal:
		return err
	case err := <-networkDone:
		return err
	}
}

func (a *Agent) report(err error) {
	if err != nil {
		select {
		case a.fatal <- err:
		default:
		}
	}
}

func (a *Agent) HandleCommand(ctx context.Context, c protocol.Command) (protocol.CommandAck, error) {
	received, err := a.store.Receive(c)
	if err != nil {
		return protocol.CommandAck{}, err
	}
	if !received.Accepted {
		ack := protocol.CommandAck{CommandID: c.ID, Status: "duplicate"}
		if received.Record.Result != nil {
			ack.Error = received.Record.Result.Error
		}
		return ack, nil
	}
	return a.dispatch(ctx, c)
}

func (a *Agent) dispatch(ctx context.Context, c protocol.Command) (protocol.CommandAck, error) {
	_, runtime, ok := a.manager.Client(c.RuntimeID)
	if !ok {
		for _, r := range a.manager.Snapshot() {
			if r.ID == c.RuntimeID {
				runtime = r
				break
			}
		}
	}
	if runtime.ID != "" && runtime.Generation != c.RuntimeGeneration {
		return a.reject(c, protocol.StaleRuntime, "Runtime generation no longer matches.")
	}
	if !ok {
		return a.reject(c, protocol.CodexUnavailable, "Runtime is not available.")
	}
	if runtime.Generation != c.RuntimeGeneration {
		return a.reject(c, protocol.StaleRuntime, "Runtime generation no longer matches.")
	}
	if time.Now().After(c.ExpiresAt) {
		return a.reject(c, protocol.CommandExpired, "Command has expired.")
	}
	if c.Operation == protocol.NewSession {
		a.mu.Lock()
		queue := a.creates[c.RuntimeID]
		if queue == nil {
			queue = make(chan protocol.Command, 20)
			a.creates[c.RuntimeID] = queue
			a.group.Add(1)
			go func() { defer a.group.Done(); a.createLoop(c.RuntimeID, queue) }()
		}
		a.mu.Unlock()
		select {
		case queue <- c:
			return protocol.CommandAck{CommandID: c.ID, Status: "accepted"}, nil
		default:
			return a.reject(c, protocol.SessionQueueFull, "Runtime creation queue is full.")
		}
	}
	a.mu.RLock()
	actor := a.sessions[c.SessionID]
	a.mu.RUnlock()
	if actor == nil {
		return a.reject(c, protocol.UnknownSession, "Session is not available.")
	}
	request := actorCommand{command: c, reply: make(chan commandReply, 1)}
	select {
	case actor.commands <- request:
	case <-ctx.Done():
		return protocol.CommandAck{}, ctx.Err()
	case <-a.ctx.Done():
		return protocol.CommandAck{}, a.ctx.Err()
	}
	select {
	case r := <-request.reply:
		return r.ack, r.err
	case <-ctx.Done():
		return protocol.CommandAck{}, ctx.Err()
	case <-a.ctx.Done():
		return protocol.CommandAck{}, a.ctx.Err()
	}
}

func (a *Agent) reject(c protocol.Command, code, message string) (protocol.CommandAck, error) {
	e := &protocol.Error{Code: code, Message: message}
	result := &protocol.Result{CommandID: c.ID, State: "failed", Error: e}
	_, err := a.record(c, CommandFailed, result, "command_failed")
	status := "rejected_invalid_state"
	switch code {
	case protocol.StaleRuntime:
		status = "rejected_stale_runtime"
	case protocol.UnknownSession:
		status = "rejected_unknown_session"
	case protocol.CommandExpired:
		status = "rejected_expired"
	}
	return protocol.CommandAck{CommandID: c.ID, Status: status, Error: e}, err
}

func (a *Agent) record(c protocol.Command, state CommandState, result *protocol.Result, kind string) (protocol.Event, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return protocol.Event{}, err
	}
	return a.store.RecordResult(c.ID, state, result, protocol.Event{RuntimeID: c.RuntimeID, RuntimeGeneration: c.RuntimeGeneration, SessionID: c.SessionID, Kind: kind, Data: data})
}

func (a *Agent) emit(runtime protocol.Runtime, sessionID, kind string, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = a.store.AppendEvent(protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: sessionID, Kind: kind, Data: body})
	return err
}

func (a *Agent) onSession(runtime protocol.Runtime, s protocol.Session) {
	if a.ctx.Err() != nil {
		return
	}
	a.mu.Lock()
	actor := a.sessions[s.ID]
	if actor == nil {
		actor = &sessionActor{agent: a, identityRuntime: runtime.ID, identityThread: s.ThreadID, runtime: runtime, session: s, commands: make(chan actorCommand), eventQueue: make(chan actorEvent, 128), requestQueue: make(chan actorRequest, 32), snapshots: make(chan actorSnapshot, 8), pending: map[string]pendingRequest{}}
		a.sessions[s.ID] = actor
		a.group.Add(1)
		go func() { defer a.group.Done(); actor.run() }()
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	processed := make(chan struct{})
	select {
	case actor.snapshots <- actorSnapshot{runtime: runtime, session: s, processed: processed}:
	case <-a.ctx.Done():
	}
	select {
	case <-processed:
	case <-a.ctx.Done():
	}
}

func (a *Agent) actorForThread(runtimeID, threadID string) *sessionActor {
	// Actor IDs and thread identities are immutable; avoid racing its state.
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, s := range a.sessions {
		if s.identityRuntime == runtimeID && s.identityThread == threadID {
			return s
		}
	}
	return nil
}

func (a *Agent) onEvent(runtime protocol.Runtime, event codexadapter.Event) {
	if event.Thread != nil && event.Thread.ID != "" && workspaceAllowed(event.Thread.CWD, a.cfg.AllowedWorkspaceRoots) {
		session := sessionFromThread(runtime, *event.Thread, true)
		if a.actorForThread(runtime.ID, event.Thread.ID) == nil {
			saved, err := a.store.UpsertSession(session)
			if err != nil {
				a.report(err)
				return
			}
			a.report(a.emit(runtime, saved.ID, "session_discovered", saved))
			a.onSession(runtime, saved)
		}
	}
	actor := a.actorForThread(runtime.ID, event.ThreadID)
	if actor == nil {
		return
	}
	// Generation travels with the event; delivery after restart is ignored.
	eventGeneration := runtime.Generation
	select {
	case actor.runtimeEvents() <- actorEvent{generation: eventGeneration, event: event}:
	case <-a.ctx.Done():
	}
}

func (a *Agent) onRequest(runtime protocol.Runtime, request codexadapter.Request) {
	actor := a.actorForThread(runtime.ID, request.ThreadID)
	if actor == nil {
		a.log.Warn("request for undiscovered session", "runtime_id", runtime.ID, "thread_id", request.ThreadID)
		return
	}
	select {
	case actor.runtimeRequests() <- actorRequest{generation: runtime.Generation, request: request}:
	case <-a.ctx.Done():
	}
}

func (a *Agent) createLoop(runtimeID string, queue <-chan protocol.Command) {
	for {
		select {
		case <-a.ctx.Done():
			return
		case c := <-queue:
			client, runtime, ok := a.manager.Client(runtimeID)
			if !ok || runtime.Generation != c.RuntimeGeneration {
				_, err := a.reject(c, protocol.StaleRuntime, "Runtime generation no longer matches.")
				a.report(err)
				continue
			}
			cwd, err := auth.CanonicalWorkspace(runtime.DefaultCWD, a.cfg.AllowedWorkspaceRoots)
			if err != nil {
				_, err = a.reject(c, protocol.InvalidWorkspace, "Workspace is not allowed.")
				a.report(err)
				continue
			}
			if time.Now().After(c.ExpiresAt) {
				_, err = a.reject(c, protocol.CommandExpired, "Command has expired.")
				a.report(err)
				continue
			}
			if err = a.store.SetCommandState(c.ID, CommandExecuting, nil); err != nil {
				a.report(err)
				continue
			}
			thread, err := client.StartThread(a.ctx, codexadapter.ThreadOptions{CWD: cwd, ApprovalPolicy: "on-request", Sandbox: "workspace-write"})
			if err != nil {
				a.report(a.executionError(c, err))
				continue
			}
			s, err := a.store.UpsertSession(protocol.Session{WorkerID: a.cfg.WorkerID, RuntimeID: runtimeID, ThreadID: thread.ID, Name: thread.Name, Preview: thread.Preview, CWD: cwd, State: "idle", Loaded: true})
			if err != nil {
				a.report(err)
				continue
			}
			if s.Name == "" {
				s.Name = "New session"
			}
			if err = a.emit(runtime, s.ID, "session_discovered", s); err != nil {
				a.report(err)
				continue
			}
			a.onSession(runtime, s)
			_, err = a.record(c, CommandCompleted, &protocol.Result{CommandID: c.ID, State: "completed", Session: &s}, "command_completed")
			a.report(err)
		}
	}
}

func (a *Agent) executionError(c protocol.Command, err error) error {
	var rpc *codexadapter.RPCError
	if errors.As(err, &rpc) || errors.Is(err, codexadapter.ErrStaleTurn) || errors.Is(err, codexadapter.ErrRequestNotPending) {
		code := protocol.CodexProtocolError
		if errors.Is(err, codexadapter.ErrRequestNotPending) {
			code = protocol.ApprovalNotPending
		}
		if errors.Is(err, codexadapter.ErrStaleTurn) {
			code = protocol.StaleTurn
		}
		_, saveErr := a.reject(c, code, "Codex rejected the operation.")
		return saveErr
	}
	result := &protocol.Result{CommandID: c.ID, State: "outcome_unknown", Error: &protocol.Error{Code: protocol.OutcomeUnknown, Message: "Codex response was not confirmed. Inspect the thread before retrying."}}
	_, saveErr := a.record(c, CommandOutcomeUnknown, result, "command_result_unknown")
	return saveErr
}

type actorCommand struct {
	command protocol.Command
	reply   chan commandReply
}
type commandReply struct {
	ack protocol.CommandAck
	err error
}
type actorSnapshot struct {
	runtime   protocol.Runtime
	session   protocol.Session
	processed chan struct{}
}
type actorEvent struct {
	generation uint64
	event      codexadapter.Event
}
type actorRequest struct {
	generation uint64
	request    codexadapter.Request
}
type pendingRequest struct {
	approval protocol.Approval
	request  codexadapter.Request
}

type sessionActor struct {
	agent                           *Agent
	identityRuntime, identityThread string
	runtime                         protocol.Runtime
	session                         protocol.Session
	commands                        chan actorCommand
	snapshots                       chan actorSnapshot
	eventQueue                      chan actorEvent
	requestQueue                    chan actorRequest
	queue                           []protocol.Command
	pending                         map[string]pendingRequest
	activeCommand                   *protocol.Command
	text                            strings.Builder
}

func (s *sessionActor) runtimeEvents() chan actorEvent     { return s.eventQueue }
func (s *sessionActor) runtimeRequests() chan actorRequest { return s.requestQueue }

func (s *sessionActor) run() {
	for {
		if s.session.ActiveTurnID == "" && (s.session.State == "idle" || s.session.State == "not_loaded" || s.session.State == "failed") && s.runtime.State == "running" && len(s.queue) > 0 {
			c := s.queue[0]
			s.queue = s.queue[1:]
			s.start(c)
			continue
		}
		select {
		case <-s.agent.ctx.Done():
			return
		case snapshot := <-s.snapshots:
			previous := s.session
			if snapshot.runtime.Generation < s.runtime.Generation {
				if snapshot.processed != nil {
					close(snapshot.processed)
				}
				continue
			}
			if snapshot.runtime.Generation > s.runtime.Generation || snapshot.runtime.State == "failed" || snapshot.runtime.State == "stopped" {
				s.runtime = snapshot.runtime
				s.session = snapshot.session
				s.pending = map[string]pendingRequest{}
				s.activeCommand = nil
				s.text.Reset()
				for _, c := range s.queue {
					_, err := s.agent.reject(c, protocol.StaleRuntime, "Runtime restarted before execution.")
					s.agent.report(err)
				}
				s.queue = nil
			} else {
				s.runtime = snapshot.runtime
				if s.session.ActiveTurnID == "" {
					s.session = snapshot.session
				} else {
					s.session.Name, s.session.Preview = snapshot.session.Name, snapshot.session.Preview
					s.session.CWD, s.session.GitBranch, s.session.GitRoot = snapshot.session.CWD, snapshot.session.GitBranch, snapshot.session.GitRoot
				}
			}
			s.save()
			current := s.session
			previous.UpdatedAt, current.UpdatedAt = time.Time{}, time.Time{}
			if previous != current {
				s.agent.report(s.agent.emit(s.runtime, s.session.ID, "session_state_changed", s.session))
			}
			if snapshot.processed != nil {
				close(snapshot.processed)
			}
		case request := <-s.commands:
			s.command(request)
		case event := <-s.eventQueue:
			if event.generation == s.runtime.Generation {
				s.event(event.event)
			}
		case request := <-s.requestQueue:
			if request.generation == s.runtime.Generation {
				s.request(request.request)
			}
		}
	}
}

func (s *sessionActor) command(req actorCommand) {
	c := req.command
	reject := func(code, message string) {
		ack, err := s.agent.reject(c, code, message)
		req.reply <- commandReply{ack, err}
	}
	if c.RuntimeID != s.runtime.ID || c.RuntimeGeneration != s.runtime.Generation {
		reject(protocol.StaleRuntime, "Runtime generation no longer matches.")
		return
	}
	if c.SessionID != s.session.ID || c.ThreadID != s.session.ThreadID {
		reject(protocol.UnknownSession, "Session target does not match.")
		return
	}
	if time.Now().After(c.ExpiresAt) {
		reject(protocol.CommandExpired, "Command has expired.")
		return
	}
	if c.Operation == protocol.StartTurn {
		if len(s.queue) >= s.agent.cfg.MaxQueuedTurns {
			reject(protocol.SessionQueueFull, "Session queue is full.")
			return
		}
		s.queue = append(s.queue, c)
		req.reply <- commandReply{ack: protocol.CommandAck{CommandID: c.ID, Status: "accepted"}}
		return
	}
	if (c.Operation == protocol.Steer || c.Operation == protocol.Interrupt) && (c.ExpectedTurnID == "" || c.ExpectedTurnID != s.session.ActiveTurnID) {
		reject(protocol.StaleTurn, "The active turn has changed.")
		return
	}
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != c.RuntimeGeneration {
		reject(protocol.StaleRuntime, "Runtime generation no longer matches.")
		return
	}
	if c.Operation == protocol.ApprovalResponse || c.Operation == protocol.InputResponse {
		p, ok := s.pending[c.Arguments.RequestID]
		if !ok || p.approval.ID != c.Arguments.ApprovalID || p.approval.ThreadID != c.ThreadID || (p.approval.TurnID != "" && p.approval.TurnID != s.session.ActiveTurnID) || (c.ExpectedTurnID != "" && c.ExpectedTurnID != p.approval.TurnID) {
			reject(protocol.ApprovalNotPending, "This request is no longer pending.")
			return
		}
	}
	if err := s.agent.store.SetCommandState(c.ID, CommandExecuting, nil); err != nil {
		req.reply <- commandReply{err: err}
		s.agent.report(err)
		return
	}
	req.reply <- commandReply{ack: protocol.CommandAck{CommandID: c.ID, Status: "accepted"}}
	var err error
	switch c.Operation {
	case protocol.Steer:
		_, err = client.Steer(s.agent.ctx, c.ThreadID, c.ExpectedTurnID, c.Arguments.Text)
	case protocol.Interrupt:
		err = client.Interrupt(s.agent.ctx, c.ThreadID, c.ExpectedTurnID)
	case protocol.ApprovalResponse:
		err = client.ReplyApproval(s.agent.ctx, c.Arguments.RequestID, codexadapter.ApprovalResponse{Decision: c.Arguments.Decision})
	case protocol.InputResponse:
		err = client.ReplyAnswers(s.agent.ctx, c.Arguments.RequestID, c.Arguments.Answers)
	default:
		err = fmt.Errorf("unsupported control operation")
	}
	if err != nil {
		if !definiteRPCFailure(err) {
			s.session.State = "unknown"
			s.save()
		}
		s.agent.report(s.agent.executionError(c, err))
		return
	}
	if c.Operation == protocol.ApprovalResponse || c.Operation == protocol.InputResponse {
		state := "approved"
		if c.Arguments.Decision == "decline" {
			state = "declined"
		}
		if c.Arguments.Decision == "cancel" {
			state = "cancelled"
		}
		s.resolve(c.Arguments.RequestID, state)
	}
	_, err = s.agent.record(c, CommandCompleted, &protocol.Result{CommandID: c.ID, TurnID: s.session.ActiveTurnID, State: "completed"}, "command_completed")
	s.agent.report(err)
}

func (s *sessionActor) start(c protocol.Command) {
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != c.RuntimeGeneration {
		_, err := s.agent.reject(c, protocol.StaleRuntime, "Runtime generation no longer matches.")
		s.agent.report(err)
		return
	}
	if time.Now().After(c.ExpiresAt) {
		_, err := s.agent.reject(c, protocol.CommandExpired, "Queued command has expired.")
		s.agent.report(err)
		return
	}
	cwd, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		_, err = s.agent.reject(c, protocol.InvalidWorkspace, "Session workspace is not allowed.")
		s.agent.report(err)
		return
	}
	if err = s.agent.store.SetCommandState(c.ID, CommandExecuting, nil); err != nil {
		s.agent.report(err)
		return
	}
	if !s.session.Loaded {
		_, err = client.ResumeThread(s.agent.ctx, s.session.ThreadID, codexadapter.ThreadOptions{CWD: cwd, ApprovalPolicy: "on-request", Sandbox: "workspace-write"})
		if err != nil {
			s.session.State = "failed"
			s.save()
			s.agent.report(s.agent.executionError(c, err))
			return
		}
		s.session.Loaded = true
	}
	turn, err := client.StartTurn(s.agent.ctx, s.session.ThreadID, c.Arguments.Text)
	if err != nil {
		if !definiteRPCFailure(err) {
			s.session.State = "unknown"
			s.save()
		}
		s.agent.report(s.agent.executionError(c, err))
		return
	}
	s.session.ActiveTurnID = turn.ID
	s.session.State = "running"
	s.activeCommand = &c
	s.text.Reset()
	s.save()
	_, err = s.agent.record(c, CommandCompleted, &protocol.Result{CommandID: c.ID, TurnID: turn.ID, State: "running"}, "turn_started")
	s.agent.report(err)
}

func (s *sessionActor) save() {
	saved, err := s.agent.store.UpsertSession(s.session)
	if err == nil {
		s.session = saved
	}
	s.agent.report(err)
}

func (s *sessionActor) event(event codexadapter.Event) {
	switch event.Kind {
	case "turn_started":
		if event.TurnID == "" {
			return
		}
		if s.session.ActiveTurnID != event.TurnID {
			s.text.Reset()
		}
		s.session.ActiveTurnID = event.TurnID
		s.session.State = "running"
		s.session.Loaded = true
		s.save()
		s.agent.report(s.agent.emit(s.runtime, s.session.ID, "turn_started", protocol.Result{TurnID: event.TurnID, State: "running"}))
	case "agent_message_delta":
		if event.TurnID == s.session.ActiveTurnID && s.text.Len() < 256<<10 {
			s.text.WriteString(event.Text)
		}
	case "final_agent_message":
		if event.TurnID == s.session.ActiveTurnID {
			s.text.Reset()
			s.text.WriteString(event.Text)
		}
		s.agent.report(s.agent.emit(s.runtime, s.session.ID, "final_agent_message", protocol.Result{TurnID: event.TurnID, Text: s.agent.redactor.Redact(event.Text)}))
	case "turn_completed", "turn_failed", "turn_interrupted":
		if event.TurnID == "" || event.TurnID != s.session.ActiveTurnID {
			return
		}
		kind := event.Kind
		state := "idle"
		if event.State == "failed" {
			kind = "turn_failed"
			state = "failed"
		}
		if event.State == "interrupted" {
			kind = "turn_interrupted"
		}
		result := protocol.Result{TurnID: event.TurnID, State: state, Text: s.agent.redactor.Redact(s.text.String())}
		if s.activeCommand != nil {
			result.CommandID = s.activeCommand.ID
		}
		s.agent.report(s.agent.emit(s.runtime, s.session.ID, kind, result))
		s.session.ActiveTurnID = ""
		s.session.State = state
		s.activeCommand = nil
		s.text.Reset()
		s.save()
		for key := range s.pending {
			s.resolve(key, "cleared")
		}
	case "server_request_resolved":
		s.resolve(event.RequestID, "cleared")
	case "thread_closed":
		s.session.Loaded = false
		s.session.State = "not_loaded"
		s.save()
	}
}

func (s *sessionActor) request(req codexadapter.Request) {
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != s.runtime.Generation || !client.RequestPending(req.RequestID) {
		return
	}
	if req.Kind != "approval_requested" && req.Kind != "input_requested" {
		s.agent.log.Warn("unsupported Codex request", "kind", req.Kind, "session_id", s.session.ID)
		return
	}
	if _, exists := s.pending[req.RequestID]; exists {
		return
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: req.RequestID, ThreadID: req.ThreadID, TurnID: req.TurnID, ItemID: req.ItemID, Type: req.ApprovalType, Summary: s.agent.redactor.Redact(req.Summary), Decisions: req.Decisions, State: "pending"}
	kind := "approval_requested"
	s.session.State = "waiting_approval"
	if req.Kind == "input_requested" {
		kind = "user_input_requested"
		approval.Type = "user_input"
		s.session.State = "waiting_input"
		for _, q := range req.Questions {
			p := protocol.Question{ID: q.ID, Header: q.Header, Prompt: q.Prompt, Secret: q.IsSecret}
			for _, o := range q.Choices {
				p.Options = append(p.Options, o.Label)
			}
			approval.Questions = append(approval.Questions, p)
		}
	}
	if req.TurnID != "" {
		s.session.ActiveTurnID = req.TurnID
	}
	s.pending[req.RequestID] = pendingRequest{approval, req}
	s.save()
	s.agent.report(s.agent.emit(s.runtime, s.session.ID, kind, approval))
}

func definiteRPCFailure(err error) bool {
	var rpc *codexadapter.RPCError
	return errors.As(err, &rpc) || errors.Is(err, codexadapter.ErrStaleTurn) || errors.Is(err, codexadapter.ErrRequestNotPending)
}

func (s *sessionActor) resolve(requestID, state string) {
	pending, ok := s.pending[requestID]
	if !ok {
		return
	}
	delete(s.pending, requestID)
	pending.approval.State = state
	s.agent.report(s.agent.emit(s.runtime, s.session.ID, "approval_resolved", pending.approval))
	if len(s.pending) == 0 && s.session.ActiveTurnID != "" {
		s.session.State = "running"
		s.save()
	}
}
