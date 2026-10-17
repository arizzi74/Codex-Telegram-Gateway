package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestHubFileCapabilityAndBinaryTransfer(t *testing.T) {
	store := testRegistry(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	token, err := auth.GenerateWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: id, Name: "worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 30*time.Second)
	acks := make(chan protocol.CommandAck, 3)
	hub.AckHandler = func(_ context.Context, _, _ uuid.UUID, ack protocol.CommandAck) error { acks <- ack; return nil }
	server := httptest.NewTLSServer(hub)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)
	sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: id.String(), WorkerName: "worker", OS: "linux", Arch: "amd64", ProtocolMin: 1, ProtocolMax: 1})
	if frame, err := readEnvelope(ctx, conn); err != nil || frame.Type != "hello_ack" {
		t.Fatalf("hello: %s %v", frame.Type, err)
	}
	// The maximum upload must fit the real websocket envelope unchanged.
	data := make([]byte, protocol.MaxFileBytes)
	for i := range data {
		data[i] = byte(i % 256)
	}
	command := protocol.Command{ID: uuid.NewString(), WorkerID: id.String(), RuntimeID: uuid.NewString(), RuntimeGeneration: 1, SessionID: uuid.NewString(), ThreadID: "thread", Operation: protocol.StoreFiles, Arguments: protocol.Arguments{CWD: "/work", Files: []protocol.File{{Name: "binary.dat", Data: data}}}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	assertRejected := func() {
		t.Helper()
		if err := hub.SendCommand(ctx, command); err != nil {
			t.Fatal(err)
		}
		select {
		case ack := <-acks:
			if ack.Status != "rejected" || ack.Error == nil || ack.Error.Code != protocol.UnsupportedOperation || !strings.Contains(ack.Error.Message, "Update the worker") {
				t.Fatalf("missing terminal upgrade error: %#v", ack)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertRejected()
	setCapability := func(supported bool) {
		t.Helper()
		sendFrame(t, ctx, conn, "heartbeat", protocol.Heartbeat{WorkerID: id.String(), SupportsFileInput: supported})
		for {
			workers, err := store.ListWorkers(ctx)
			if err != nil || len(workers) != 1 {
				t.Fatalf("worker metadata: %v", err)
			}
			var metadata struct {
				SupportsFileInput bool `json:"supports_file_input"`
			}
			if err := json.Unmarshal(workers[0].HeartbeatMetadata, &metadata); err != nil {
				t.Fatal(err)
			}
			hub.mu.RLock()
			peerSupported := hub.peers[id.String()] != nil && hub.peers[id.String()].supportsFileInput
			hub.mu.RUnlock()
			if metadata.SupportsFileInput == supported && peerSupported == supported {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
	setCapability(true)
	sent := make(chan error, 1)
	go func() { sent <- hub.SendCommand(ctx, command) }()
	frame, err := readEnvelope(ctx, conn)
	if err != nil || frame.Type != "command" {
		t.Fatalf("file frame: %s %v", frame.Type, err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	received, err := protocol.Payload[protocol.Command](frame)
	if err != nil || received.Operation != protocol.StoreFiles || len(received.Arguments.Files) != 1 || !bytes.Equal(received.Arguments.Files[0].Data, data) {
		t.Fatalf("file transport corrupted bytes: %v", err)
	}
	setCapability(false)
	assertRejected()
}
