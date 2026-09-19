package gateway

import (
	"context"
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

func TestHubSessionCapabilitiesRejectOldWorkersWithoutDisconnecting(t *testing.T) {
	for _, tc := range []struct {
		name, version        string
		workspaces, deletion bool
	}{
		{name: "legacy", version: "0.5.17"},
		{name: "released without flags", version: "0.5.19", workspaces: true, deletion: true},
		{name: "prefixed release without flags", version: "v0.5.19", workspaces: true, deletion: true},
		{name: "new worker", version: "dev", workspaces: true, deletion: true},
		{name: "workspace only", version: "dev", workspaces: true},
		{name: "deletion only", version: "dev", deletion: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			store := testRegistry(t)
			token, err := auth.GenerateWorkerToken()
			if err != nil {
				t.Fatal(err)
			}
			workerID := uuid.New()
			if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: workerID, Name: "worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
				t.Fatal(err)
			}
			hub := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 15*time.Second)
			var rejected []string
			hub.AckHandler = func(_ context.Context, worker, connection uuid.UUID, ack protocol.CommandAck) error {
				if worker != workerID || connection == uuid.Nil || ack.Status != "rejected" || ack.Error == nil || ack.Error.Code != protocol.UnsupportedOperation {
					t.Errorf("capability rejection: %s %s %#v", worker, connection, ack)
				}
				rejected = append(rejected, ack.CommandID)
				return nil
			}
			server := httptest.NewTLSServer(hub)
			defer server.Close()
			conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https"), &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			hello := protocol.Hello{WorkerID: workerID.String(), WorkerName: "worker", OS: "linux", Arch: "amd64", WorkerVersion: tc.version, ProtocolMin: 1, ProtocolMax: 1}
			if tc.version == "dev" {
				hello.SupportsSessionWorkspaces, hello.SupportsSessionDeletion = tc.workspaces, tc.deletion
			}
			sendFrame(t, ctx, conn, "hello", hello)
			if frame, err := readEnvelope(ctx, conn); err != nil || frame.Type != "hello_ack" {
				t.Fatalf("hello acknowledgement: %s %v", frame.Type, err)
			}
			for _, operation := range []protocol.Operation{protocol.BrowseWorkspace, protocol.DeleteSession, protocol.NewSession, protocol.StartTurn} {
				command := protocol.Command{ID: uuid.NewString(), WorkerID: workerID.String(), RuntimeID: uuid.NewString(), RuntimeGeneration: 1,
					Operation: operation, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
				supported := true
				switch operation {
				case protocol.BrowseWorkspace:
					command.Arguments.Workspace = &protocol.WorkspaceRequest{}
					supported = tc.workspaces
				case protocol.NewSession:
					command.Arguments = protocol.Arguments{CWD: "/work", SessionName: "Project name", CreateDirectory: true}
					supported = tc.workspaces
				case protocol.DeleteSession, protocol.StartTurn:
					command.SessionID, command.ThreadID = uuid.NewString(), "thread"
					if operation == protocol.StartTurn {
						command.Arguments.Text = "still connected"
					} else {
						supported = tc.deletion
					}
				}
				if err := hub.SendCommand(ctx, command); err != nil {
					t.Fatal(err)
				}
				if !supported {
					if len(rejected) == 0 || rejected[len(rejected)-1] != command.ID {
						t.Fatal("unsupported operation did not use durable rejection handler")
					}
					continue
				}
				frame, err := readEnvelope(ctx, conn)
				if err != nil || frame.Type != "command" {
					t.Fatalf("supported command: %s %v", frame.Type, err)
				}
				received, err := protocol.Payload[protocol.Command](frame)
				if err != nil || received.ID != command.ID {
					t.Fatalf("wire received unsupported or incorrect command: %#v %v", received, err)
				}
			}
		})
	}
}
