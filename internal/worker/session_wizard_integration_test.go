package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Exercise the real Telegram webhook, durable wizard, worker transport,
// filesystem operations and rendered buttons together. Only Telegram's HTTP
// edge and the Codex JSONL server are fixtures; all databases are isolated.
func TestControlPlaneGuidedSessionCreationAndDeletionIntegration(t *testing.T) {
	registryStore, probe := controlPlaneRegistry(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	workspace, stateRoot := t.TempDir(), t.TempDir()
	parent := filepath.Join(workspace, "Projects")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.New()
	token := "cwk_isolated_session_wizard_worker"
	hash := sha256.Sum256([]byte(token))
	if _, err := registryStore.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "wizard-worker", OS: "linux", Arch: "arm64", TokenHash: hash[:]}); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(stateRoot, "worker.token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateRoot, "state", "worker.db")
	local, err := OpenStore(statePath, workerID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	cfg := config.WorkerConfig{
		WorkerID: workerID.String(), Name: "wizard-worker", GatewayURL: "wss://gateway.example.com/tgapi/v1/workers/connect", TokenFile: tokenPath, StateFile: statePath,
		AllowedWorkspaceRoots: []string{workspace},
		Runtimes:              []config.RuntimeProfile{{ID: "main", Name: "Main", CodexBinary: "/bin/true", WorkingDirectory: workspace, Autostart: true, RestartPolicy: "on-failure"}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := NewAgent(cfg, local, logger)
	if err != nil {
		t.Fatal(err)
	}
	var fakeMu sync.Mutex
	var fake *codextest.Server
	agent.manager.start = func(run context.Context, _ codexadapter.Config) (*codexadapter.Client, error) {
		client, server, err := codextest.New(run)
		if err == nil {
			fakeMu.Lock()
			fake = server
			fakeMu.Unlock()
		}
		return client, err
	}
	tg := &telegramRecorder{}
	t.Cleanup(func() {
		if t.Failed() {
			tg.mu.Lock()
			defer tg.mu.Unlock()
			for _, message := range tg.messages {
				t.Logf("Telegram: %s", message.Text)
			}
		}
	})
	gw := startControlGateway(t, registryStore, tg, logger)
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
	runtimeID := agent.manager.Snapshot()[0].ID
	fakeMu.Lock()
	f := fake
	fakeMu.Unlock()
	if f == nil {
		t.Fatal("Codex fixture was not started")
	}
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 100, "/tgnew "+runtimeID, 0)
	waitControl(t, func() bool { return tg.hasText("What would you like to name the new session?") })
	const sessionName = "Guided integration session"
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 101, sessionName, 0)
	waitControl(t, func() bool { return tg.hasText("Choose the parent folder:", workspace, "Projects") })
	open := wizardButton(tg, "Choose the parent folder:\n"+workspace+"\n", "Open 1")
	if open == "" {
		t.Fatal("folder listing did not contain a navigation button")
	}
	postCallback(t, gw.server.Client(), gw.server.URL, "secret", 102, open)
	waitControl(t, func() bool { return wizardButton(tg, "Choose the parent folder:\n"+parent+"\n", "Create here") != "" })
	create := wizardButton(tg, "Choose the parent folder:\n"+parent+"\n", "Create here")
	postCallback(t, gw.server.Client(), gw.server.URL, "secret", 103, create)
	var created protocol.Session
	waitControl(t, func() bool {
		sessions, err := registryStore.SessionSnapshot(ctx)
		if err != nil {
			return false
		}
		for _, session := range sessions {
			if session.Name == sessionName {
				created = session
				return true
			}
		}
		return false
	})
	child := filepath.Join(parent, "Guided_integration_session")
	if created.CWD != child {
		t.Fatalf("created CWD = %q, want %q", created.CWD, child)
	}
	waitControl(t, func() bool {
		var selected string
		err := probe.QueryRowContext(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND user_id=7 AND chat_id=9 AND message_thread_id=0`).Scan(&selected)
		return err == nil && selected == created.ID
	})
	if hasCall(f.Calls(), "turn/start") {
		t.Fatal("a wizard reply was submitted to Codex as a prompt")
	}
	var renamed bool
	for _, call := range f.Calls() {
		if call.Method == "thread/name/set" {
			var params struct {
				ThreadID string `json:"threadId"`
				Name     string `json:"name"`
			}
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			renamed = params.ThreadID == created.ThreadID && params.Name == sessionName
		}
	}
	if !renamed || countCall(f.Calls(), "thread/start") != 1 {
		t.Fatalf("named creation calls = %#v", f.Calls())
	}
	marker := filepath.Join(child, "keep.txt")
	if err := os.WriteFile(marker, []byte("project files survive session deletion"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The stateless JSONL fixture needs this saved snapshot for the worker's
	// fresh thread/read idle/CWD check immediately before deletion.
	f.SetThreads([]map[string]any{{"id": created.ThreadID, "name": sessionName, "cwd": child, "status": "idle"}}, []string{created.ThreadID})
	postTelegram(t, gw.server.Client(), gw.server.URL, "secret", 104, "/tgdeletesession "+runtimeID, 0)
	waitControl(t, func() bool { return wizardButton(tg, "Delete a session ·", "Delete 1") != "" })
	postCallback(t, gw.server.Client(), gw.server.URL, "secret", 105, wizardButton(tg, "Delete a session ·", "Delete 1"))
	waitControl(t, func() bool {
		return wizardButton(tg, "Delete Codex session \""+sessionName+"\"?", "Delete session") != ""
	})
	postCallback(t, gw.server.Client(), gw.server.URL, "secret", 106, wizardButton(tg, "Delete Codex session \""+sessionName+"\"?", "Delete session"))
	waitControl(t, func() bool {
		return tg.hasText("Deleted Codex session:", sessionName, "Working directory and files kept:")
	})
	if countCall(f.Calls(), "thread/delete") != 1 || hasCall(f.Calls(), "thread/archive") || hasCall(f.Calls(), "thread/resume") {
		t.Fatalf("unexpected delete calls = %#v", f.Calls())
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "project files survive session deletion" {
		t.Fatalf("project marker after deletion = %q, %v", content, err)
	}
	var selectedCount int
	if err := probe.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_bindings WHERE session_id=?`, created.ID).Scan(&selectedCount); err != nil || selectedCount != 0 {
		t.Fatalf("deleted session still selected: count=%d, %v", selectedCount, err)
	}
	sessions, err := local.ListSessions(runtimeID)
	if err != nil || len(sessions) != 1 || !sessions[0].Deleted || !sessions[0].Archived {
		t.Fatalf("local deletion tombstone = %#v, %v", sessions, err)
	}
}

func wizardButton(tg *telegramRecorder, messagePart, label string) string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	for i := len(tg.messages) - 1; i >= 0; i-- {
		message := tg.messages[i]
		if !strings.Contains(message.Text, messagePart) || message.Keyboard == nil {
			continue
		}
		for _, row := range message.Keyboard.Rows {
			for _, button := range row {
				if button.Text == label {
					return button.Data
				}
			}
		}
	}
	return ""
}
