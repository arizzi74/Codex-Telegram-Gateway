package worker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type sessionModeControl struct {
	ctx      context.Context
	registry *registry.Store
	probe    *sql.DB
	local    *Store
	gw       *gatewayRun
	telegram *telegramRecorder
	codex    *codextest.Server
	runtime  string
	sessions map[string]protocol.Session
}

func newSessionModeControl(t *testing.T) sessionModeControl {
	t.Helper()
	registryStore, probe := controlPlaneRegistry(t)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	root := t.TempDir()
	// The worker's private Unix socket must fit the platform's sockaddr limit;
	// Go's test-name-prefixed TempDir path is too long for this test name.
	stateRoot, err := os.MkdirTemp("", "tg-session-modes-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })
	workerID := uuid.New()
	token := "cwk_session_modes_test"
	hash := sha256.Sum256([]byte(token))
	if _, err := registryStore.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "sessions-worker", OS: "linux", Arch: "arm64", TokenHash: hash[:]}); err != nil {
		t.Fatal(err)
	}
	tokenFile, stateFile := filepath.Join(stateRoot, "worker.token"), filepath.Join(stateRoot, "state", "worker.db")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := OpenStore(stateFile, workerID.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.WorkerConfig{WorkerID: workerID.String(), Name: "sessions-worker", GatewayURL: "wss://gateway.example.com/tgapi/v1/workers/connect", TokenFile: tokenFile, StateFile: stateFile, AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "main", Name: "Main", CodexBinary: "/bin/true", WorkingDirectory: root, Autostart: true, RestartPolicy: "on-failure"}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := NewAgent(cfg, local, log)
	if err != nil {
		t.Fatal(err)
	}
	fixture := make(chan *codextest.Server, 2)
	agent.manager.start = func(run context.Context, _ codexadapter.Config) (*codexadapter.Client, error) {
		client, server, err := codextest.New(run)
		if err == nil {
			server.SetThreads([]map[string]any{
				{"id": "thread-alpha", "name": "Alpha complete session name", "cwd": root, "status": "idle"},
				{"id": "thread-beta", "name": "Beta complete session name", "cwd": root, "status": "idle"},
			}, []string{"thread-alpha", "thread-beta"})
			fixture <- server
		}
		return client, err
	}
	telegram := &telegramRecorder{}
	gw := startControlGateway(t, registryStore, telegram, log)
	slot := &gatewaySlot{}
	slot.set(gw)
	agent.newConnection = dialTestServer(slot)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-agentDone:
			if err != nil {
				t.Errorf("agent stopped: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
		gw.close()
		_ = local.Close()
		if t.Failed() {
			telegram.mu.Lock()
			defer telegram.mu.Unlock()
			for _, action := range telegram.actions {
				t.Logf("Telegram %s %d: %s", action.kind, action.messageID, action.text)
			}
		}
	})
	waitControl(t, func() bool { return len(agent.manager.Snapshot()) == 1 && len(gw.hub.ConnectedWorkers()) == 1 })
	env := sessionModeControl{ctx: ctx, registry: registryStore, probe: probe, local: local, gw: gw, telegram: telegram, runtime: agent.manager.Snapshot()[0].ID, sessions: make(map[string]protocol.Session)}
	select {
	case env.codex = <-fixture:
	case <-time.After(time.Second):
		t.Fatal("Codex fixture unavailable")
	}
	waitControl(t, func() bool {
		sessions, err := registryStore.SessionSnapshot(ctx)
		for _, session := range sessions {
			env.sessions[session.ThreadID] = session
		}
		return err == nil && len(env.sessions) == 2
	})
	return env
}

func (e sessionModeControl) prompt(t *testing.T, update int64, text string) {
	t.Helper()
	postTelegram(t, e.gw.server.Client(), e.gw.server.URL, "secret", update, text, 0)
}

func (e sessionModeControl) commentary(t *testing.T, thread, turn, text string) {
	t.Helper()
	if err := e.codex.Emit("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": uuid.NewString(), "type": "agentMessage", "status": "completed", "phase": "commentary", "text": text}}); err != nil {
		t.Fatal(err)
	}
}

func (e sessionModeControl) assertBinding(t *testing.T, thread string) {
	t.Helper()
	var selected string
	if err := e.probe.QueryRowContext(e.ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=7 AND chat_id=9 AND message_thread_id=0`).Scan(&selected); err != nil || selected != e.sessions[thread].ID {
		t.Fatalf("selection changed: selected=%q want=%q err=%v", selected, e.sessions[thread].ID, err)
	}
}

// Follow both already-running sessions through the actual webhook, worker
// transport and sender. The user's active selection is independent of replies
// to background questions and explicitly addressed multisession prompts.
func TestControlPlaneSessionFocusQuestionsAndMultisessionIntegration(t *testing.T) {
	e := newSessionModeControl(t)
	a, b := e.sessions["thread-alpha"], e.sessions["thread-beta"]
	selectControlSession(t, e.gw, e.telegram, 100, e.runtime, 1)
	e.prompt(t, 102, "Start Alpha work")
	var turnA, turnB string
	waitControl(t, func() bool { turnA = currentActive(t, e.local, e.runtime, a.ThreadID); return turnA != "" })
	e.commentary(t, a.ThreadID, turnA, "Alpha initial progress")
	waitControl(t, func() bool { return e.telegram.countText("Alpha initial progress") == 1 })
	firstA := e.telegram.latestMessageID("Alpha initial progress")
	selectControlSession(t, e.gw, e.telegram, 103, e.runtime, 2)
	waitControl(t, func() bool { return e.telegram.wasDeleted(firstA) })
	waitControl(t, func() bool {
		targets, err := e.registry.ListTelegramTypingTargets(e.ctx, 10)
		return err == nil && len(targets) == 0
	})
	e.prompt(t, 105, "Start Beta work")
	waitControl(t, func() bool { turnB = currentActive(t, e.local, e.runtime, b.ThreadID); return turnB != "" })
	e.commentary(t, b.ThreadID, turnB, "Beta latest progress")
	waitControl(t, func() bool { return e.telegram.countText("Beta latest progress") == 1 })
	firstB := e.telegram.latestMessageID("Beta latest progress")
	e.commentary(t, a.ThreadID, turnA, "Alpha latest background progress")
	// The subsequent sessions command is queued after this event is durably
	// ingested, so the following selection can restore its latest commentary.
	waitControl(t, func() bool {
		var seen bool
		err := e.probe.QueryRowContext(e.ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE session_id=? AND kind='agent_progress_message' AND json_extract(payload,'$.text')='Alpha latest background progress')`, a.ID).Scan(&seen)
		return err == nil && seen
	})
	if e.telegram.countText("Alpha latest background progress") != 0 {
		t.Fatal("hidden Alpha progress reached selected Beta")
	}
	selectControlSession(t, e.gw, e.telegram, 106, e.runtime, 1)
	waitControl(t, func() bool {
		return e.telegram.wasDeleted(firstB) && e.telegram.countText("Alpha latest background progress") == 1
	})
	secondA := e.telegram.latestMessageID("Alpha latest background progress")
	selectControlSession(t, e.gw, e.telegram, 108, e.runtime, 2)
	waitControl(t, func() bool {
		return e.telegram.wasDeleted(secondA) && e.telegram.countText("Beta latest progress") == 2
	})

	if err := e.codex.Request("item/tool/requestUserInput", 91, map[string]any{"threadId": a.ThreadID, "turnId": turnA, "itemId": "question-alpha", "isBlocking": true, "questions": []map[string]any{{"id": "choice", "header": "Choice", "question": "Alpha needs an answer while Beta stays selected"}}}); err != nil {
		t.Fatal(err)
	}
	waitControl(t, func() bool { return e.telegram.hasText(a.Name, "Alpha needs an answer") })
	questionID := e.telegram.latestMessageID("Alpha needs an answer")
	postTelegram(t, e.gw.server.Client(), e.gw.server.URL, "secret", 110, "Continue Alpha safely", questionID)
	waitControl(t, func() bool {
		for _, response := range e.codex.Responses() {
			if string(response.ID) == "91" && strings.Contains(string(response.Result), "Continue Alpha safely") {
				return true
			}
		}
		return false
	})
	e.assertBinding(t, b.ThreadID)
	if err := e.codex.Emit("item/completed", map[string]any{"threadId": a.ThreadID, "turnId": turnA, "item": map[string]any{"id": "alpha-final", "type": "agentMessage", "status": "completed", "phase": "final_answer", "text": "Alpha's hidden final response"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.codex.Emit("turn/completed", map[string]any{"threadId": a.ThreadID, "turnId": turnA, "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	waitControl(t, func() bool {
		var seen bool
		err := e.probe.QueryRowContext(e.ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE session_id=? AND kind='turn_completed')`, a.ID).Scan(&seen)
		return err == nil && seen
	})
	// Send a normal UI command as a delivery barrier after Alpha's terminal
	// event. No hidden turn completion may slip through before this response.
	statusBefore := e.telegram.countText("Queued commands:")
	e.prompt(t, 111, "/tgstatus")
	waitControl(t, func() bool { return e.telegram.countText("Queued commands:") > statusBefore })
	if e.telegram.hasText("✅", a.Name, "Turn completed.") || e.telegram.countText("Alpha's hidden final response") != 0 {
		t.Fatal("Alpha completion leaked into Beta's conversation")
	}

	e.prompt(t, 112, "/tgmultisession on")
	waitControl(t, func() bool { return e.telegram.hasText("Multisession mode is on") })
	aliases, err := e.registry.ListTelegramSessionAliases(e.ctx, "bot", 7, 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	var aliasA string
	for _, alias := range aliases {
		if alias.SessionID == a.ID {
			aliasA = alias.Alias
		}
	}
	if aliasA == "" {
		t.Fatal("Alpha has no session command")
	}
	e.prompt(t, 113, "/-"+strings.TrimPrefix(aliasA, "_")+" Start the next Alpha turn")
	waitControl(t, func() bool { turnA = currentActive(t, e.local, e.runtime, a.ThreadID); return turnA != "" })
	e.assertBinding(t, b.ThreadID)
	var addressed bool
	for _, call := range e.codex.Calls() {
		if call.Method == "turn/start" {
			var params struct {
				ThreadID string          `json:"threadId"`
				Input    json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(call.Params, &params)
			addressed = addressed || (params.ThreadID == a.ThreadID && strings.Contains(string(params.Input), "Start the next Alpha turn"))
		}
	}
	if !addressed {
		t.Fatal("explicit session command did not start Alpha's turn")
	}
	e.commentary(t, a.ThreadID, turnA, "Alpha multisession update")
	e.commentary(t, b.ThreadID, turnB, "Beta multisession update")
	waitControl(t, func() bool {
		return e.telegram.hasVisibleText(a.Name, "Alpha multisession update") && e.telegram.hasVisibleText(b.Name, "Beta multisession update")
	})
	for _, sample := range []struct{ name, text string }{{a.Name, "Alpha multisession update"}, {b.Name, "Beta multisession update"}} {
		e.telegram.mu.Lock()
		var matched bool
		for _, action := range e.telegram.actions {
			if strings.Contains(action.text, sample.text) {
				line := strings.SplitN(action.text, "\n", 2)[0]
				matched = strings.HasSuffix(line, " "+sample.name) && len(action.entities) > 0 && action.entities[0].Type == "bold"
			}
		}
		e.telegram.mu.Unlock()
		if !matched {
			t.Fatalf("multisession update lacks its full session header: %s", sample.text)
		}
	}
	e.assertBinding(t, b.ThreadID)
	e.prompt(t, 114, "/tgmultisession off")
	waitControl(t, func() bool { return e.telegram.hasText("Multisession mode is off") })
	waitControl(t, func() bool {
		return !e.telegram.hasVisibleText("Alpha multisession update") && e.telegram.hasVisibleText("Beta multisession update")
	})
	e.prompt(t, 115, "/tgdisconnect")
	waitControl(t, func() bool {
		targets, err := e.registry.ListTelegramTypingTargets(e.ctx, 10)
		return err == nil && len(targets) == 0 && !e.telegram.hasVisibleText("Beta multisession update")
	})
	connected := e.telegram.countText("Connected to")
	e.prompt(t, 116, "/-"+strings.TrimPrefix(aliasA, "_"))
	waitControl(t, func() bool { return e.telegram.countText("Connected to") > connected })
	e.assertBinding(t, a.ThreadID)
}

func (t *telegramRecorder) hasVisibleText(parts ...string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	visible := make(map[int64]string)
	for _, action := range t.actions {
		if action.kind == "delete" {
			delete(visible, action.messageID)
		} else if action.kind == "send" || action.kind == "edit" {
			visible[action.messageID] = action.text
		}
	}
	for _, text := range visible {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(text, part)
		}
		if matched {
			return true
		}
	}
	return false
}
