package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWebUIColdSessionSubscribesObserverBeforeFirstPrompt(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t, "private-value")
	defer cleanup()
	session := installColdSession(a, runtime, "cold-web-session")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
	if err := a.observeWebUISession(context.Background(), runtime, session); err != nil {
		t.Fatal(err)
	}
	client, _, _ := a.manager.Client(runtime.ID)
	if !a.manager.loadedThreadSubscribed(runtime, client, session.ThreadID) {
		t.Fatal("browser enabled before observer subscription")
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/resume" {
			var params map[string]any
			if json.Unmarshal(call.Params, &params) != nil || len(params) != 2 || params["threadId"] != session.ThreadID || params["excludeTurns"] != true {
				t.Fatalf("observer changed settings or loaded unbounded history: %s", call.Params)
			}
		}
		if call.Method == "turn/start" || call.Method == "turn/steer" {
			t.Fatal("observer submitted a prompt")
		}
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "first-web-turn"})
	input := codexadapter.Event{Kind: "user_message_completed", ThreadID: session.ThreadID, TurnID: "first-web-turn", ItemID: "first-web-prompt", Text: "hello private-value"}
	a.onEvent(runtime, input)
	a.onEvent(runtime, input)
	waitFor(t, func() bool {
		events, _ := a.store.OutboxAfter(0)
		for _, event := range events {
			if event.Kind == "user_message" {
				return true
			}
		}
		return false
	})
	// Synchronize through the actor snapshot queue after both notifications.
	a.onSession(runtime, session)
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind != "user_message" {
			continue
		}
		count++
		if strings.Contains(string(event.Data), "private-value") || !strings.Contains(string(event.Data), "[REDACTED]") {
			t.Fatalf("prompt not redacted: %s", event.Data)
		}
	}
	if count != 1 {
		t.Fatalf("first prompt mirrored %d times", count)
	}
}

func TestWebUIObserverCancellationBoundsAdmissionAndActorSnapshot(t *testing.T) {
	for _, stage := range []string{"update", "snapshot-admission", "snapshot-processing"} {
		t.Run(stage, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session, err := a.store.UpsertSession(protocol.Session{RuntimeID: runtime.ID, ThreadID: "cancel-observer", CWD: runtime.DefaultCWD, State: "not_loaded"})
			if err != nil {
				t.Fatal(err)
			}
			server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
			if stage == "update" {
				a.updateMu.Lock()
				defer a.updateMu.Unlock()
			} else {
				capacity := 0
				if stage == "snapshot-processing" {
					capacity = 1
				}
				// Model an actor busy in an earlier native RPC without starting
				// another goroutine that could itself hide a shutdown leak.
				a.sessions[session.ID] = &sessionActor{identityRuntime: runtime.ID, identityThread: session.ThreadID, snapshots: make(chan actorSnapshot, capacity)}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.observeWebUISession(ctx, runtime, session) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("observer ignored caller deadline: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("observer blocked after caller deadline")
			}
			client, _, _ := a.manager.Client(runtime.ID)
			if ready, _ := a.manager.webUIObserverState(runtime, client, session.ThreadID); ready {
				t.Fatal("canceled snapshot enabled the browser")
			}
			if stage == "update" && hasCall(server.Calls(), "thread/resume") {
				t.Fatal("canceled admission reached Codex")
			}
			if stage != "update" {
				if !a.updateMu.TryLock() {
					t.Fatal("canceled snapshot retained update admission")
				}
				a.updateMu.Unlock()
			}
		})
	}
}

func TestWebUILoadedObserverReusesOnlyAcknowledgedSubscription(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "loaded-observer", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, []string{session.ThreadID})
	client, _, _ := a.manager.Client(runtime.ID)
	if err := a.manager.discover(t.Context(), runtime, client); err != nil {
		t.Fatal(err)
	}
	before := countCall(server.Calls(), "thread/resume")
	if before != 1 {
		t.Fatalf("discovery subscriptions = %d", before)
	}
	for range 2 {
		if err := a.observeWebUISession(t.Context(), runtime, session); err != nil {
			t.Fatal(err)
		}
	}
	if countCall(server.Calls(), "thread/resume") != before {
		t.Fatal("already observed session repeated native resume")
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "thread_closed", ThreadID: session.ThreadID})
	if err := a.observeWebUISession(t.Context(), runtime, session); err != nil {
		t.Fatal(err)
	}
	if countCall(server.Calls(), "thread/resume") != before+1 {
		t.Fatal("closed subscription reused stale readiness")
	}
	a.manager.install(runtime, client)
	a.manager.mu.Lock()
	a.manager.runtimes[runtime.ID].subscriptions[session.ThreadID] = struct{}{}
	a.manager.mu.Unlock()
	if err := a.observeWebUISession(t.Context(), runtime, session); err != nil {
		t.Fatal(err)
	}
	if countCall(server.Calls(), "thread/resume") != before+2 {
		t.Fatal("in-flight discovery reservation treated as observer readiness")
	}
}

func TestWebUIObserverWaitDoesNotHoldRelayMutex(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "waiting-observer", "")
	runtime.LocalSocket = "/unused-observer-test.sock"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := testWebUIRelay()
	r.ctx, r.runtime, r.session = ctx, runtime, session
	entered := make(chan struct{})
	r.pool = &webUIRelays{c: &Connection{cfg: a.cfg, store: a.store, snapshot: func() []protocol.Runtime { return []protocol.Runtime{runtime} }}}
	r.pool.c.observeWebUI = func(ctx context.Context, _ protocol.Runtime, _ protocol.Session) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- r.forwardInput(nil, []byte(`{"id":1,"method":"thread/resume","params":{}}`)) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("observer admission failed before callback: %v", err)
	case <-time.After(time.Second):
		t.Fatal("observer admission did not start")
	}
	if !r.mu.TryLock() {
		t.Fatal("observer blocked the relay mutex")
	}
	r.mu.Unlock()
	if _, err := r.clientMessage([]byte(`{"id":2,"method":"turn/start","params":{"input":[{"type":"text","text":"hello"}]}}`)); err == nil {
		t.Fatal("prompt admitted before durable observer")
	}
	if _, err := r.clientMessage([]byte(`{"id":3,"method":"thread/resume","params":{}}`)); err == nil {
		t.Fatal("overlapping resume admitted")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("disconnect did not cancel observer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnected observer did not stop")
	}
	if r.observerReady || r.resumed {
		t.Fatal("disconnected observer enabled browser input")
	}
}

func TestWebUIDiscoveryCannotConfirmSubscriptionClosedDuringActorSnapshot(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "closing-observer", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, []string{session.ThreadID})
	client, _, _ := a.manager.Client(runtime.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	a.manager.hooks.OnSession = func(runtime protocol.Runtime, session protocol.Session) {
		close(entered)
		<-release
		a.onSession(runtime, session)
	}
	done := make(chan error, 1)
	go func() { done <- a.manager.discover(t.Context(), runtime, client) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("discovery did not reach actor snapshot")
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "thread_closed", ThreadID: session.ThreadID})
	// A new discovery may already have reserved this ID while the older
	// acknowledged subscription is still waiting for its actor snapshot.
	a.manager.mu.Lock()
	a.manager.runtimes[runtime.ID].subscriptions[session.ThreadID] = struct{}{}
	a.manager.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ready, _ := a.manager.webUIObserverState(runtime, client, session.ThreadID); ready {
		t.Fatal("older discovery callback confirmed the replacement reservation")
	}
	if err := a.observeWebUISession(t.Context(), runtime, session); err != nil {
		t.Fatal(err)
	}
	if countCall(server.Calls(), "thread/resume") != 2 {
		t.Fatal("browser did not await a fresh subscription acknowledgment")
	}
}

func TestWebUIResumeFailsClosedWhenObserverCannotSubscribe(t *testing.T) {
	r := testWebUIRelay()
	r.ctx = context.Background()
	r.runtime = protocol.Runtime{ID: "runtime", Generation: 1}
	r.session.ID, r.session.RuntimeID = "session", r.runtime.ID
	r.session.CWD = t.TempDir()
	r.pool = &webUIRelays{c: &Connection{}}
	r.pool.c.cfg.AllowedWorkspaceRoots = []string{r.session.CWD}
	r.pool.c.observeWebUI = func(ctx context.Context, runtime protocol.Runtime, session protocol.Session) error {
		if _, bounded := ctx.Deadline(); !bounded || runtime.ID != r.runtime.ID || session.ThreadID != r.session.ThreadID {
			t.Fatal("observer context or scope missing")
		}
		return errors.New("observer disconnected")
	}
	if _, err := r.clientMessage([]byte(`{"id":1,"method":"thread/resume","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(map[string]any{"id": 1, "result": map[string]any{"thread": map[string]any{"id": r.session.ThreadID, "cwd": r.session.CWD}}})
	if data, err := r.serverMessage(response); err == nil || len(data) != 0 || r.resumed {
		t.Fatalf("browser became usable without observer: %s, %v", data, err)
	}
	if _, err := r.clientMessage([]byte(`{"id":2,"method":"turn/start","params":{"input":[{"type":"text","text":"hello"}]}}`)); err == nil {
		t.Fatal("prompt accepted after observer failed")
	}
}

func TestWebUIObserverRejectsStaleOrDisallowedSession(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installColdSession(a, runtime, "cold-web-session")
	stale := runtime
	stale.Generation++
	if err := a.observeWebUISession(context.Background(), stale, session); err == nil {
		t.Fatal("stale runtime subscribed")
	}
	session.CWD = t.TempDir()
	if err := a.observeWebUISession(context.Background(), runtime, session); err == nil {
		t.Fatal("disallowed session subscribed")
	}
	if hasCall(server.Calls(), "thread/resume") {
		t.Fatal("invalid observer target reached native API")
	}
}

func TestWebUIObserverPreservesExistingTurn(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installColdSession(a, runtime, "running-web-session")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "active", "turns": []map[string]any{{"id": "live-turn", "status": "inProgress"}}}}, nil)
	if err := a.observeWebUISession(context.Background(), runtime, session); err != nil {
		t.Fatal(err)
	}
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil || len(sessions) != 1 || sessions[0].ActiveTurnID != "live-turn" || sessions[0].State != "running" {
		t.Fatalf("observer lost current turn: %+v %v", sessions, err)
	}
	for _, call := range server.Calls() {
		if strings.HasPrefix(call.Method, "turn/") {
			t.Fatalf("observer mutated running turn: %s", call.Method)
		}
	}
}
