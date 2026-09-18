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

func TestHubImageCapabilityProtectsOldWorkersAndTransfersImageBytes(t *testing.T) {
	for _, supported := range []bool{false, true} {
		name := "legacy worker"
		if supported {
			name = "image worker"
		}
		t.Run(name, func(t *testing.T) {
			store := testRegistry(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			token, err := auth.GenerateWorkerToken()
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.New()
			if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: id, Name: "worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
				t.Fatal(err)
			}
			hub := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 3*time.Second)
			server := httptest.NewTLSServer(hub)
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			conn.SetReadLimit(protocol.MaxFrameBytes)
			sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: id.String(), WorkerName: "worker", OS: "linux", Arch: "amd64", ProtocolMin: 1, ProtocolMax: 1, SupportsImageInput: supported})
			if frame, err := readEnvelope(ctx, conn); err != nil || frame.Type != "hello_ack" {
				t.Fatalf("hello: %v %v", frame.Type, err)
			}
			workers, err := store.ListWorkers(ctx)
			if err != nil || len(workers) != 1 {
				t.Fatalf("registered worker: %v", err)
			}
			var metadata struct {
				SupportsImageInput bool `json:"supports_image_input"`
			}
			if err := json.Unmarshal(workers[0].HeartbeatMetadata, &metadata); err != nil || metadata.SupportsImageInput != supported {
				t.Fatalf("image capability not persisted from hello: %v", err)
			}
			// An image larger than the previous 2 MiB transport cap exercises the
			// actual WSS command path rather than just protocol serialization.
			image := make([]byte, 2<<20)
			copy(image, "\x89PNG\r\n\x1a\n")
			command := protocol.Command{ID: uuid.NewString(), WorkerID: id.String(), RuntimeID: uuid.NewString(), RuntimeGeneration: 1,
				SessionID: uuid.NewString(), ThreadID: "thread", Operation: protocol.StartTurn,
				Arguments: protocol.Arguments{Text: "caption", Images: []protocol.Image{{MIMEType: "image/png", Data: image}}},
				CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
			if !supported {
				if err := hub.SendCommand(ctx, command); err == nil {
					t.Fatal("image command sent to a worker that would ignore its attachment")
				}
				return
			}
			sent := make(chan error, 1)
			go func() { sent <- hub.SendCommand(ctx, command) }()
			frame, err := readEnvelope(ctx, conn)
			if err != nil || frame.Type != "command" {
				t.Fatalf("image frame: %s %v", frame.Type, err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			received, err := protocol.Payload[protocol.Command](frame)
			if err != nil || received.ID != command.ID || len(received.Arguments.Images) != 1 || !bytes.Equal(received.Arguments.Images[0].Data, image) {
				t.Fatalf("image command content changed: %v", err)
			}
			sendFrame(t, ctx, conn, "heartbeat", protocol.Heartbeat{WorkerID: id.String(), SupportsImageInput: true})
			for {
				workers, err := store.ListWorkers(ctx)
				if err != nil || len(workers) != 1 {
					t.Fatalf("heartbeat worker: %v", err)
				}
				if bytes.Contains(workers[0].HeartbeatMetadata, []byte(`"worker_id"`)) {
					if err := json.Unmarshal(workers[0].HeartbeatMetadata, &metadata); err != nil || !metadata.SupportsImageInput {
						t.Fatalf("heartbeat lost image capability: %v", err)
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("heartbeat was not persisted")
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
	}
}
