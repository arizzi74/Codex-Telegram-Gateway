package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

type inboundControlTelegram struct {
	*telegramRecorder
	data    []byte
	started chan struct{}
	release chan struct{}
}

func (tg *inboundControlTelegram) DownloadFile(ctx context.Context, id string) (protocol.File, error) {
	if id == "blocked" {
		close(tg.started)
		select {
		case <-tg.release:
		case <-ctx.Done():
			return protocol.File{}, ctx.Err()
		}
	}
	return protocol.File{Name: "untrusted-download-name", Data: bytes.Clone(tg.data)}, nil
}

// Exercise the real webhook, immutable command, authenticated WebSocket,
// worker filesystem, result outbox and Telegram sender. Only Telegram's network
// and Codex JSONL peer are substituted; no external messages or files are used.
func TestControlPlaneInboundFilesIntegration(t *testing.T) {
	store, probe := controlPlaneRegistry(t)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	workspace, stateRoot := t.TempDir(), t.TempDir()
	alpha, beta := filepath.Join(workspace, "Alpha"), filepath.Join(workspace, "Beta")
	for _, path := range []string{alpha, beta} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workerID := uuid.New()
	token := "cwk_inbound_controlplane_fixture"
	hash := sha256.Sum256([]byte(token))
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "files", OS: "linux", Arch: "amd64", TokenHash: hash[:]}); err != nil {
		t.Fatal(err)
	}
	tokenPath, statePath := filepath.Join(stateRoot, "worker.token"), filepath.Join(stateRoot, "state.db")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := OpenStore(statePath, workerID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	cfg := config.WorkerConfig{WorkerID: workerID.String(), Name: "files", GatewayURL: "wss://gateway.invalid/tgw/api/v1/workers/connect", TokenFile: tokenPath, StateFile: statePath,
		AllowedWorkspaceRoots: []string{workspace}, Runtimes: []config.RuntimeProfile{{ID: "main", Name: "Main", CodexBinary: "/bin/true", WorkingDirectory: workspace, Autostart: true}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	agent, err := NewAgent(cfg, local, logger)
	if err != nil {
		t.Fatal(err)
	}
	var peerMu sync.Mutex
	var peer *codextest.Server
	agent.manager.start = func(run context.Context, _ codexadapter.Config) (*codexadapter.Client, error) {
		client, fake, err := codextest.New(run)
		if err == nil {
			fake.SetThreads([]map[string]any{{"id": "thread-alpha", "name": "Alpha", "cwd": alpha, "status": "idle"}, {"id": "thread-beta", "name": "Beta", "cwd": beta, "status": "idle"}}, nil)
			peerMu.Lock()
			peer = fake
			peerMu.Unlock()
		}
		return client, err
	}
	// Secret-shaped content must survive the binary command transport unchanged.
	data := []byte("raw document\x00\xff\n" + "cwk_" + strings.Repeat("a", 64) + "\nsk-" + strings.Repeat("b", 32))
	tg := &inboundControlTelegram{telegramRecorder: &telegramRecorder{}, data: data, started: make(chan struct{}), release: make(chan struct{})}
	gw := startControlGateway(t, store, tg, logger)
	defer gw.close()
	var unblock sync.Once
	defer unblock.Do(func() { close(tg.release) })
	slot := &gatewaySlot{}
	slot.set(gw)
	agent.newConnection = dialTestServer(slot)
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent stopped: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	}()
	waitControl(t, func() bool {
		sessions, err := store.SessionSnapshot(ctx)
		return err == nil && len(sessions) == 2 && len(gw.hub.ConnectedWorkers()) == 1
	})
	peerMu.Lock()
	fake := peer
	peerMu.Unlock()
	if fake == nil {
		t.Fatal("missing Codex fixture")
	}
	runtimeID := agent.manager.Snapshot()[0].ID
	selectControlSession(t, gw, tg.telegramRecorder, 10, runtimeID, 1)
	if _, err := os.Stat(filepath.Join(alpha, "inbound")); !os.IsNotExist(err) {
		t.Fatalf("inbound existed before upload: %v", err)
	}
	postDocument := func(id int64, fileID, caption string) error {
		body, _ := json.Marshal(map[string]any{"update_id": id, "message": map[string]any{"message_id": id, "from": map[string]any{"id": 7}, "chat": map[string]any{"id": 9}, "caption": caption,
			"document": map[string]any{"file_id": fileID, "file_name": "report.txt", "file_size": len(data)}}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.server.URL+"/tgw/api/v1/telegram/webhook", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
		response, err := gw.server.Client().Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("document update %d: HTTP %d", id, response.StatusCode)
		}
		return nil
	}
	savedPath := func(updateID int64) string {
		t.Helper()
		var commandID string
		waitControl(t, func() bool {
			err := probe.QueryRowContext(ctx, `SELECT command_id FROM commands WHERE telegram_update_id=?`, updateID).Scan(&commandID)
			return err == nil && commandID != ""
		})
		path := filepath.Join(alpha, "inbound", inboundFilename("report.txt", commandID))
		waitControl(t, func() bool {
			contents, err := os.ReadFile(path)
			return err == nil && bytes.Equal(contents, data)
		})
		return path
	}
	beforeResume := countCall(fake.Calls(), "thread/resume")
	tg.mu.Lock()
	beforeMessages := len(tg.messages)
	tg.mu.Unlock()
	if err := postDocument(20, "first", ""); err != nil {
		t.Fatal(err)
	}
	first := savedPath(20)
	waitControl(t, func() bool { return tg.hasText("File saved in " + first) })
	tg.mu.Lock()
	newMessages := len(tg.messages) - beforeMessages
	tg.mu.Unlock()
	if newMessages != 1 {
		t.Fatalf("file-only upload sent %d messages, want only the saved-path confirmation", newMessages)
	}
	if countCall(fake.Calls(), "turn/start") != 0 || countCall(fake.Calls(), "turn/steer") != 0 || countCall(fake.Calls(), "thread/resume") != beforeResume {
		t.Fatal("uncaptioned upload contacted Codex")
	}
	if err := postDocument(20, "first", ""); err != nil {
		t.Fatal(err)
	}
	if tg.countText("File saved in "+first) != 1 {
		t.Fatal("Telegram retry duplicated the saved-file confirmation")
	}
	if err := postDocument(21, "second", "Summarize this report"); err != nil {
		t.Fatal(err)
	}
	second := savedPath(21)
	waitControl(t, func() bool { return countCall(fake.Calls(), "turn/start") == 1 })
	assertInput := func(method, text, path string) {
		t.Helper()
		for _, call := range fake.Calls() {
			if call.Method != method {
				continue
			}
			var params struct {
				Input []struct{ Type, Text string } `json:"input"`
			}
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			if len(params.Input) != 1 || params.Input[0].Type != "text" || params.Input[0].Text != text+"\nFile: "+path {
				t.Fatalf("%s did not receive caption and saved path: %s", method, call.Params)
			}
			return
		}
		t.Fatalf("missing %s", method)
	}
	assertInput("turn/start", "Summarize this report", second)
	waitControl(t, func() bool { return currentActive(t, local, runtimeID, "thread-alpha") != "" })
	active := currentActive(t, local, runtimeID, "thread-alpha")
	if err := postDocument(22, "third", ""); err != nil {
		t.Fatal(err)
	}
	third := savedPath(22)
	waitControl(t, func() bool { return tg.hasText("File saved in " + third) })
	if countCall(fake.Calls(), "turn/start") != 1 || countCall(fake.Calls(), "turn/steer") != 0 || currentActive(t, local, runtimeID, "thread-alpha") != active {
		t.Fatal("file-only upload disturbed an active turn")
	}
	if err := postDocument(23, "fourth", "Also inspect this"); err != nil {
		t.Fatal(err)
	}
	fourth := savedPath(23)
	waitControl(t, func() bool { return countCall(fake.Calls(), "turn/steer") == 1 })
	assertInput("turn/steer", "Also inspect this", fourth)
	response := make(chan error, 1)
	go func() { response <- postDocument(24, "blocked", "") }()
	select {
	case <-tg.started:
	case <-time.After(5 * time.Second):
		t.Fatal("document download did not start")
	}
	selectControlSession(t, gw, tg.telegramRecorder, 30, runtimeID, 2)
	unblock.Do(func() { close(tg.release) })
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	fifth := savedPath(24)
	waitControl(t, func() bool { return tg.hasText("File saved in " + fifth) })
	if _, err := os.Stat(filepath.Join(beta, "inbound")); !os.IsNotExist(err) {
		t.Fatalf("session switch redirected upload to Beta: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(alpha, "inbound"))
	if err != nil || len(entries) != 5 {
		t.Fatalf("saved files = %d, want five distinct uploads; err=%v", len(entries), err)
	}
	if tg.countText("File saved in "+first) != 1 {
		t.Fatal("Telegram retry later duplicated the saved-file confirmation")
	}
}
