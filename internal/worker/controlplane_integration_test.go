package worker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// This test runs the real registry, websocket hub, dispatcher, sender, worker
// Agent and bbolt outbox. The fixture is an app-server JSONL peer, so the only
// substituted edges are TLS trust and Telegram's external HTTP API.
func TestControlPlaneWorkerOutboxSurvivesGatewayRestartIntegration(t *testing.T) {
	store, registryProbe := controlPlaneRegistry(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	root := t.TempDir()
	workerID := uuid.New()
	token := "cwk_controlplane_test_token"
	hash := sha256.Sum256([]byte(token))
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "pipeline", OS: "linux", Arch: "amd64", TokenHash: hash[:]}); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(root, "worker.token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local, err := OpenStore(filepath.Join(root, "state", "worker.db"), workerID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	cfg := config.WorkerConfig{WorkerID: workerID.String(), Name: "pipeline", GatewayURL: "wss://pipeline.invalid/api/v1/workers/connect", TokenFile: tokenFile, StateFile: filepath.Join(root, "state", "worker.db"), AllowedWorkspaceRoots: []string{root}, Runtimes: []config.RuntimeProfile{{ID: "main", Name: "Main", CodexBinary: "/bin/true", WorkingDirectory: root, Autostart: true, RestartPolicy: "on-failure"}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := NewAgent(cfg, local, log)
	if err != nil {
		t.Fatal(err)
	}
	var fakeMu sync.Mutex
	var fake *codextest.Server
	agent.manager.start = func(run context.Context, _ codexadapter.Config) (*codexadapter.Client, error) {
		client, s, e := codextest.New(run)
		if e != nil {
			return nil, e
		}
		s.SetThreads([]map[string]any{{"id": "thread-a", "sessionId": "thread-a", "name": "Alpha", "preview": "one", "cwd": root, "status": "idle"}, {"id": "thread-b", "sessionId": "thread-b", "name": "Beta", "preview": "two", "cwd": root, "status": "idle"}, {"id": "thread-c", "sessionId": "thread-c", "name": "Gamma", "preview": "three", "cwd": root, "status": "idle"}}, []string{"thread-a", "thread-b", "thread-c"})
		fakeMu.Lock()
		fake = s
		fakeMu.Unlock()
		return client, nil
	}

	tg := &telegramRecorder{}
	gw := startControlGateway(t, store, tg, log)
	defer gw.close()
	slot := &gatewaySlot{}
	slot.set(gw)
	agent.newConnection = dialTestServer(slot)
	agentDone := make(chan error, 1)
	go func() { agentDone <- agent.Run(ctx) }()
	defer func() {
		stop()
		select {
		case err := <-agentDone:
			if err != nil {
				t.Errorf("agent stopped: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}()
	waitControl(t, func() bool { return len(agent.manager.Snapshot()) == 1 && len(gw.hub.ConnectedWorkers()) == 1 })
	var sessions []protocol.Session
	waitControl(t, func() bool {
		var e error
		sessions, e = store.SessionSnapshot(ctx)
		return e == nil && len(sessions) == 3
	})
	runtimeID := agent.manager.Snapshot()[0].ID

	// AT-03: /tgsessions traverses the actual webhook, Registry delivery and
	// sender renderer; the Telegram result contains every discovered thread.
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 1, "/tgsessions "+runtimeID, 0)
	waitControl(t, func() bool { return tg.hasText("Sessions", "Alpha", "Beta", "Gamma") })

	a, b := sessions[0], sessions[1]
	targetThread := a.ThreadID

	// Selection and a normal Telegram message travel through the actual webhook.
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 2, "/tgconnect "+a.ID, 0)
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 3, "perform an offline-safe turn", 0)
	waitControl(t, func() bool {
		fakeMu.Lock()
		defer fakeMu.Unlock()
		return fake != nil && countFixtureCalls(fake.Calls(), "turn/start") == 1
	})
	active := ""
	waitControl(t, func() bool {
		localSessions, e := local.ListSessions(runtimeID)
		if e != nil {
			return false
		}
		for _, session := range localSessions {
			if session.ThreadID == targetThread {
				active = session.ActiveTurnID
			}
		}
		return active != ""
	})

	fakeMu.Lock()
	f := fake
	fakeMu.Unlock()
	// Commentary and tools each keep one independently replaced message. Both
	// IDs must survive a restart, then remain until final delivery succeeds.
	progressTexts := []string{"Checking the gateway command routing now.", "The command routing is verified; checking delivery next.", "Delivery checks passed; preparing the final response."}
	emitCommentary := func(i int) {
		t.Helper()
		if err := f.Emit("item/completed", map[string]any{"threadId": targetThread, "turnId": active, "item": map[string]any{"id": "commentary-" + string(rune('a'+i)), "type": "agentMessage", "status": "completed", "phase": "commentary", "text": progressTexts[i]}}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			waitControl(t, func() bool { return tg.countText(progressTexts[i]) == 1 })
		} else {
			waitControl(t, func() bool { return tg.hasEditedText(progressTexts[i]) })
		}
	}
	emitCommentary(0)
	progressIDs := []int64{tg.messageID(progressTexts[0])}
	// Interleave both kinds after the restart to catch either one accidentally
	// replacing the other. Started tools stay monospace before they finish.
	toolCommands := []string{"printf 'tool one 🧪\\n'", "rg --files", "go version"}
	for i, command := range toolCommands {
		if i == 1 {
			gw.close()
			gw = startControlGateway(t, store, tg, log)
			slot.set(gw)
			defer gw.close()
			waitControl(t, func() bool { return len(gw.hub.ConnectedWorkers()) == 1 })
		}
		if err := f.Emit("item/started", map[string]any{"threadId": targetThread, "turnId": active, "item": map[string]any{"id": "tool-" + string(rune('a'+i)), "type": "commandExecution", "status": "inProgress", "command": command}}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			waitControl(t, func() bool { return tg.countText(command) == 1 })
			progressIDs = append(progressIDs, tg.messageID(command))
			// Restart after the first send is durably checkpointed. The
			// external-send-before-commit crash window is at-least-once;
			// this case checks recovery of an existing replacement target.
			waitControl(t, func() bool {
				var checkpointed bool
				err := registryProbe.QueryRowContext(ctx, `SELECT EXISTS(
					SELECT 1 FROM telegram_progress_messages
					WHERE bot_id='bot' AND chat_id=9 AND telegram_message_id=? AND status='pending'
				)`, progressIDs[len(progressIDs)-1]).Scan(&checkpointed)
				return err == nil && checkpointed
			})
		} else {
			waitControl(t, func() bool { return tg.hasEditedText(command) })
			emitCommentary(i)
		}
	}
	tg.assertCommentaryProgress(t, progressIDs[0], progressTexts)
	tg.assertToolProgress(t, progressIDs[len(progressIDs)-1], toolCommands)
	if len(progressIDs) != 2 || progressIDs[0] == progressIDs[1] {
		t.Fatalf("commentary and tools should have two independent messages: %v", progressIDs)
	}
	if currentActive(t, local, runtimeID, targetThread) != active {
		t.Fatal("temporary progress ended the running turn")
	}
	if tg.deletedCount() != 0 {
		t.Fatal("temporary progress was removed before the final answer")
	}
	if tg.countText("Queued for") != 0 {
		t.Fatal("normal prompt produced an unwanted queued acknowledgment")
	}
	// Bare /status reaches Codex while work is active; /tgstatus reads gateway
	// queues. Neither is submitted as a new model turn. Explicit controls may
	// acknowledge their command, so the prompt-only check runs before them.
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 9001, "/status", 0)
	waitControl(t, func() bool { return tg.hasText("Codex session", "Model:", "Reasoning:") })
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 9002, "/tgstatus", 0)
	waitControl(t, func() bool { return tg.hasText("Queued commands:") })

	// AT-04: changing the Telegram selection changes only the binding. It must
	// neither interrupt A nor issue another resume/start while A is running.
	beforeStart := countFixtureCalls(f.Calls(), "turn/start")
	beforeInterrupt := countFixtureCalls(f.Calls(), "turn/interrupt")
	beforeResume := countFixtureCalls(f.Calls(), "thread/resume")
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 4, "/tgconnect "+b.ID, 0)
	waitControl(t, func() bool { return tg.hasText("Connected to", "Beta") })
	time.Sleep(250 * time.Millisecond)
	fakeMu.Lock()
	if got := countFixtureCalls(f.Calls(), "turn/start"); got != beforeStart {
		fakeMu.Unlock()
		t.Fatalf("selection started another turn: got %d want %d", got, beforeStart)
	}
	if got := countFixtureCalls(f.Calls(), "turn/interrupt"); got != beforeInterrupt {
		fakeMu.Unlock()
		t.Fatalf("selection interrupted A: got %d want %d", got, beforeInterrupt)
	}
	if got := countFixtureCalls(f.Calls(), "thread/resume"); got != beforeResume {
		fakeMu.Unlock()
		t.Fatalf("selection resumed a thread: got %d want %d", got, beforeResume)
	}
	fakeMu.Unlock()
	if currentActive(t, local, runtimeID, targetThread) != active {
		t.Fatalf("selection changed A active turn: got %q want %q", currentActive(t, local, runtimeID, targetThread), active)
	}

	// AT-10: lose only the WSS transport. The server keeps running, the runtime
	// and active turn remain local, Registry marks the worker unreachable, then
	// the same worker reconnects and restores Registry liveness.
	slot.blockAndClose()
	waitControl(t, func() bool { return workerConnectivity(store, ctx, workerID) == "unreachable" })
	if currentActive(t, local, runtimeID, targetThread) != active || len(agent.manager.Snapshot()) != 1 {
		t.Fatal("WSS loss changed the local active turn or runtime")
	}
	slot.restore()
	waitControl(t, func() bool {
		return len(gw.hub.ConnectedWorkers()) == 1 && workerConnectivity(store, ctx, workerID) == "online"
	})
	waitControl(t, func() bool {
		reconciled, err := store.SessionSnapshot(ctx)
		return err == nil && len(reconciled) == 3 && currentActive(t, local, runtimeID, targetThread) == active
	})

	// AT-13: the approval button is persisted by the sender, then Codex clears
	// the exact server request before the click. The click produces only the
	// durable stale-button UI response and no App Server reply.
	connected := tg.countText("Connected to")
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 5, "/tgconnect "+a.ID, 0)
	waitControl(t, func() bool { return tg.countText("Connected to") > connected })
	if err := f.Request("item/commandExecution/requestApproval", 91, map[string]any{"threadId": targetThread, "turnId": active, "command": "go test ./...", "availableDecisions": []string{"accept", "decline"}}); err != nil {
		t.Fatal(err)
	}
	var staleCallback string
	waitControl(t, func() bool {
		staleCallback = tg.callbackFor("Approval required")
		return staleCallback != ""
	})
	responses := len(f.Responses())
	if err := f.Emit("serverRequest/resolved", map[string]any{"threadId": targetThread, "turnId": active, "requestId": 91}); err != nil {
		t.Fatal(err)
	}
	waitControl(t, func() bool {
		dashboard, err := store.AdminDashboardSnapshot(ctx)
		return err == nil && dashboard.PendingApprovals == 0
	})
	postCallback(t, gw.server.Client(), gw.server.URL, "secret", 6, staleCallback)
	waitControl(t, func() bool { return tg.hasText("This button is expired") })
	time.Sleep(250 * time.Millisecond)
	if got := len(f.Responses()); got != responses {
		t.Fatalf("stale approval reached Codex: responses got %d want %d", got, responses)
	}

	// Stop only the gateway. The live worker/runtime continues; a final event
	// emitted during the outage must remain in bbolt until reconnection.
	gw.close()
	if active == "" {
		t.Fatal("fixture did not return a turn id")
	}
	if err := f.Emit("item/completed", map[string]any{"threadId": targetThread, "turnId": active, "item": map[string]any{"id": "final-answer", "type": "agentMessage", "status": "completed", "phase": "final_answer", "text": "final emitted while gateway was offline"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Emit("turn/completed", map[string]any{"threadId": targetThread, "turnId": active, "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	waitControl(t, func() bool {
		events, e := local.OutboxAfter(0)
		if e != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "turn_completed" {
				return true
			}
		}
		return false
	})
	if tg.deletedCount() != 0 {
		t.Fatal("temporary progress was removed before the offline final answer was delivered")
	}

	gw = startControlGateway(t, store, tg, log)
	slot.set(gw)
	defer gw.close()
	waitControl(t, func() bool { return len(gw.hub.ConnectedWorkers()) == 1 })
	waitControl(t, func() bool { return tg.countText("final emitted while gateway was offline") == 1 })
	waitControl(t, func() bool { return tg.deletedCount() == len(progressIDs) })
	time.Sleep(500 * time.Millisecond)
	if got := tg.countText("final emitted while gateway was offline"); got != 1 {
		t.Fatalf("final delivery duplicated after replay: %d", got)
	}
	tg.assertProgressCleanup(t, progressIDs, "final emitted while gateway was offline")
	// Closing the fixture transport is the same failure signal an app-server
	// process exit provides. Supervisor restart advances generation but retains
	// the stable persisted session identities discovered from the new client.
	before := agent.manager.Snapshot()[0].Generation
	f.Close()
	waitControl(t, func() bool {
		return len(agent.manager.Snapshot()) == 1 && agent.manager.Snapshot()[0].Generation > before
	})
	waitControl(t, func() bool {
		recovered, e := local.ListSessions(agent.manager.Snapshot()[0].ID)
		return e == nil && len(recovered) == 3
	})

	// Readiness is a registry health check, not runtime supervision. A failed
	// readiness probe must not stop this already-running local runtime.
	bad := readinessFail{Store: store}
	r := httptest.NewRecorder()
	gateway.NewMux(bad, gw.hub).ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if r.Code != http.StatusServiceUnavailable || len(agent.manager.Snapshot()) != 1 {
		t.Fatalf("readiness/runtime isolation = %d runtimes=%d", r.Code, len(agent.manager.Snapshot()))
	}
}

type gatewayRun struct {
	hub    *gateway.Hub
	server *httptest.Server
	cancel context.CancelFunc
	done   sync.WaitGroup
}

func startControlGateway(t *testing.T, store *registry.Store, telegram gateway.TelegramAPI, log *slog.Logger) *gatewayRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hub := gateway.NewHub(store, log, time.Second, 5*time.Second)
	hub.EventHandler = store.IngestEvent
	hub.AckHandler = store.AcknowledgeCommand
	mux := gateway.NewMux(store, hub)
	mux.Handle("/api/v1/telegram/webhook", gateway.NewWebhook(store, config.GatewayConfig{AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{9}, CommandExpiry: time.Hour, Secrets: config.BotSecrets{BotName: "bot"}}, "secret", telegram, log))
	server := httptest.NewUnstartedServer(mux)
	server.StartTLS()
	run := &gatewayRun{hub: hub, server: server, cancel: cancel}
	run.done.Add(3)
	go func() { defer run.done.Done(); hub.Run(ctx) }()
	go func() { defer run.done.Done(); _ = gateway.NewDispatcher(store, hub, log).Run(ctx) }()
	go func() {
		defer run.done.Done()
		_ = gateway.NewSender(store, telegram, log, gateway.SenderOptions{BotID: "bot", OwnerID: 7}).Run(ctx)
	}()
	return run
}

type gatewaySlot struct {
	mu        sync.RWMutex
	run       *gatewayRun
	available bool
	conn      *websocket.Conn
}

func (s *gatewaySlot) set(run *gatewayRun) {
	s.mu.Lock()
	s.run, s.available = run, true
	s.mu.Unlock()
}
func (s *gatewaySlot) current() (*gatewayRun, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.run, s.available
}
func (s *gatewaySlot) remember(conn *websocket.Conn) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
}
func (s *gatewaySlot) blockAndClose() {
	s.mu.Lock()
	s.available = false
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		conn.CloseNow()
	}
}
func (s *gatewaySlot) restore() { s.mu.Lock(); s.available = true; s.mu.Unlock() }

func (g *gatewayRun) close() {
	if g.server == nil {
		return
	}
	g.cancel()
	g.server.Close()
	g.done.Wait()
}
func dialTestServer(slot *gatewaySlot) func(config.WorkerConfig, *Store, *slog.Logger, func() []protocol.Runtime, func(context.Context, protocol.Command) (protocol.CommandAck, error)) (*Connection, error) {
	return func(cfg config.WorkerConfig, local *Store, log *slog.Logger, snapshot func() []protocol.Runtime, command func(context.Context, protocol.Command) (protocol.CommandAck, error)) (*Connection, error) {
		c, e := NewConnection(cfg, local, log, snapshot, command)
		if e != nil {
			return nil, e
		}
		c.dial = func(ctx context.Context, _ string, options *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
			current, available := slot.current()
			if !available || current == nil || current.server == nil {
				return nil, nil, errors.New("test gateway is stopped")
			}
			client := current.server.Client()
			copy := *options
			copy.HTTPClient = client
			conn, response, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(current.server.URL, "https")+"/api/v1/workers/connect", &copy)
			if err == nil {
				slot.remember(conn)
			}
			return conn, response, err
		}
		return c, nil
	}
}

type telegramRecorder struct {
	mu       sync.Mutex
	messages []gateway.SendMessage
	actions  []telegramRecordedAction
	next     int64
}

type telegramRecordedAction struct {
	kind      string
	chatID    int64
	messageID int64
	text      string
	entities  []gateway.TelegramEntity
}

func (t *telegramRecorder) Send(_ context.Context, m gateway.SendMessage) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	t.messages = append(t.messages, m)
	t.actions = append(t.actions, telegramRecordedAction{kind: "send", chatID: m.ChatID, messageID: t.next, text: m.Text, entities: append([]gateway.TelegramEntity(nil), m.Entities...)})
	return t.next, nil
}

func (t *telegramRecorder) EditFormatted(_ context.Context, messageID int64, m gateway.SendMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.actions = append(t.actions, telegramRecordedAction{kind: "edit", chatID: m.ChatID, messageID: messageID, text: m.Text, entities: append([]gateway.TelegramEntity(nil), m.Entities...)})
	return nil
}

func (t *telegramRecorder) hasEditedText(text string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, action := range t.actions {
		if action.kind == "edit" && strings.Contains(action.text, text) {
			return true
		}
	}
	return false
}

func (t *telegramRecorder) assertToolProgress(test *testing.T, messageID int64, commands []string) {
	test.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	if messageID == 0 {
		test.Fatal("tool message ID was not recorded")
	}
	seen := 0
	for _, action := range t.actions {
		if action.messageID != messageID || action.kind == "delete" {
			continue
		}
		if seen >= len(commands) {
			test.Fatalf("unexpected extra tool update: %q", action.text)
		}
		wantKind := "edit"
		if seen == 0 {
			wantKind = "send"
		}
		command := commands[seen]
		commandStart := strings.Index(action.text, command)
		if action.kind != wantKind || action.chatID != 9 || commandStart < 0 {
			test.Fatalf("tool update %d = kind %s, chat %d, text %q; want %s with %q", seen, action.kind, action.chatID, action.text, wantKind, command)
		}
		commandOffset := len(utf16.Encode([]rune(action.text[:commandStart])))
		commandEnd := commandOffset + len(utf16.Encode([]rune(command)))
		textLength := len(utf16.Encode([]rune(action.text)))
		monospace := false
		for _, entity := range action.entities {
			if entity.Type == "pre" && entity.Offset >= 0 && entity.Length > 0 && entity.Offset <= commandOffset && entity.Offset+entity.Length >= commandEnd && entity.Offset+entity.Length <= textLength {
				monospace = true
			}
		}
		if !monospace {
			test.Fatalf("tool update %d lacks a valid monospace entity covering the command: %#v", seen, action.entities)
		}
		seen++
	}
	if seen != len(commands) {
		test.Fatalf("tool updates = %d, want %d", seen, len(commands))
	}
	for _, command := range commands[1:] {
		for _, action := range t.actions {
			if action.kind == "send" && strings.Contains(action.text, command) {
				test.Fatalf("subsequent tool sent another message instead of replacing %d: %q", messageID, action.text)
			}
		}
	}
}

func (t *telegramRecorder) assertCommentaryProgress(test *testing.T, messageID int64, messages []string) {
	test.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	if messageID == 0 {
		test.Fatal("commentary message ID was not recorded")
	}
	seen := 0
	for _, action := range t.actions {
		if action.messageID != messageID || action.kind == "delete" {
			continue
		}
		if seen >= len(messages) {
			test.Fatalf("unexpected extra commentary update: %q", action.text)
		}
		wantKind := "edit"
		if seen == 0 {
			wantKind = "send"
		}
		if action.kind != wantKind || action.chatID != 9 || !strings.HasPrefix(action.text, "⏳ ") || !strings.Contains(action.text, messages[seen]) {
			test.Fatalf("commentary update %d = kind %s, chat %d, text %q; want %s with %q", seen, action.kind, action.chatID, action.text, wantKind, messages[seen])
		}
		seen++
	}
	if seen != len(messages) {
		test.Fatalf("commentary updates = %d, want %d", seen, len(messages))
	}
	for _, message := range messages[1:] {
		for _, action := range t.actions {
			if action.kind == "send" && strings.Contains(action.text, message) {
				test.Fatalf("subsequent commentary sent another message instead of replacing %d: %q", messageID, action.text)
			}
		}
	}
}

func (t *telegramRecorder) DeleteMessage(_ context.Context, chatID, messageID int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.actions = append(t.actions, telegramRecordedAction{kind: "delete", chatID: chatID, messageID: messageID})
	return nil
}

func (t *telegramRecorder) messageID(text string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, action := range t.actions {
		if action.kind == "send" && strings.Contains(action.text, text) {
			return action.messageID
		}
	}
	return 0
}

func (t *telegramRecorder) deletedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, action := range t.actions {
		if action.kind == "delete" {
			count++
		}
	}
	return count
}

func (t *telegramRecorder) assertProgressCleanup(test *testing.T, progressIDs []int64, finalText string) {
	test.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	expected := make(map[int64]bool, len(progressIDs))
	for _, id := range progressIDs {
		if id == 0 {
			test.Fatal("temporary progress message ID was not recorded")
		}
		expected[id] = true
	}
	finalDelivered := false
	for _, action := range t.actions {
		if action.kind == "send" && strings.Contains(action.text, finalText) {
			finalDelivered = true
		}
		if action.kind != "delete" {
			continue
		}
		if !finalDelivered {
			test.Fatalf("message %d deleted before the final answer was sent", action.messageID)
		}
		if action.chatID != 9 || !expected[action.messageID] {
			test.Fatalf("unexpected or duplicate message deletion: chat=%d message=%d", action.chatID, action.messageID)
		}
		delete(expected, action.messageID)
	}
	if len(expected) != 0 {
		test.Fatalf("temporary progress messages were not deleted after final delivery: %v", expected)
	}
}
func (t *telegramRecorder) Edit(context.Context, int64, int64, string, *gateway.TelegramKeyboard) error {
	return nil
}
func (t *telegramRecorder) AnswerCallback(context.Context, string, string) error { return nil }
func (t *telegramRecorder) countText(v string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, m := range t.messages {
		if strings.Contains(m.Text, v) {
			n++
		}
	}
	return n
}
func (t *telegramRecorder) hasText(parts ...string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range t.messages {
		matched := true
		for _, part := range parts {
			if !strings.Contains(m.Text, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
func (t *telegramRecorder) callbackFor(messagePart string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range t.messages {
		if !strings.Contains(m.Text, messagePart) || m.Keyboard == nil {
			continue
		}
		for _, row := range m.Keyboard.Rows {
			for _, button := range row {
				if button.Data != "" {
					return button.Data
				}
			}
		}
	}
	return ""
}

type readinessFail struct{ *registry.Store }

func (readinessFail) Ping(context.Context) error { return errors.New("database unavailable") }

func controlPlaneRegistry(t *testing.T) (*registry.Store, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "gateway.db")
	store, err := registry.Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("open isolated SQLite store: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate isolated SQLite store: %v", err)
	}
	probe, err := sql.Open("sqlite", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatalf("open isolated registry probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	return store, probe
}
func postTelegram(t *testing.T, c *http.Client, base, secret string, id int64, text string, reply int64) {
	t.Helper()
	message := map[string]any{"message_id": id, "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 9}, "text": text}
	if reply > 0 {
		message["reply_to_message"] = map[string]any{"message_id": reply, "chat": map[string]any{"id": 9}}
	}
	body, _ := json.Marshal(map[string]any{"update_id": id, "message": message})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/telegram/webhook", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("telegram update %d status %d", id, res.StatusCode)
	}
}
func postCallback(t *testing.T, c *http.Client, base, secret string, id int64, data string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"update_id": id, "callback_query": map[string]any{"id": "callback-" + string(rune('0'+id)), "from": map[string]any{"id": 7}, "data": data, "message": map[string]any{"message_id": id, "chat": map[string]any{"id": 9}}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/telegram/webhook", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("Telegram callback %d status %d", id, res.StatusCode)
	}
}
func currentActive(t *testing.T, local *Store, runtimeID, threadID string) string {
	t.Helper()
	sessions, err := local.ListSessions(runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ThreadID == threadID {
			return session.ActiveTurnID
		}
	}
	return ""
}
func workerConnectivity(store *registry.Store, ctx context.Context, id uuid.UUID) string {
	workers, err := store.ListWorkers(ctx)
	if err != nil {
		return ""
	}
	for _, worker := range workers {
		if worker.ID == id {
			return worker.Connectivity
		}
	}
	return ""
}
func waitControl(t *testing.T, ok func() bool) {
	t.Helper()
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("control plane condition timed out")
}
func countFixtureCalls(calls []codextest.Call, method string) int {
	n := 0
	for _, c := range calls {
		if c.Method == method {
			n++
		}
	}
	return n
}
