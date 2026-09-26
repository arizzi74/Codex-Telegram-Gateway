package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/telegramcommands"
)

type updateDispatchStore struct {
	request protocol.WorkerUpdateRequest
	trace   *[]string
	failed  string
}

func (s *updateDispatchStore) PendingCommandsForWorker(context.Context, uuid.UUID, int) ([]protocol.Command, error) {
	return nil, nil
}
func (s *updateDispatchStore) MarkDispatched(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (s *updateDispatchStore) ExpireCommands(context.Context) error                       { return nil }
func (s *updateDispatchStore) PendingWorkerUpdates(context.Context, uuid.UUID) ([]protocol.WorkerUpdateRequest, error) {
	return []protocol.WorkerUpdateRequest{s.request}, nil
}
func (s *updateDispatchStore) MarkWorkerUpdateDispatched(_ context.Context, worker, request uuid.UUID) error {
	if worker.String() != s.request.WorkerID || request.String() != s.request.RequestID {
		return errors.New("wrong worker update target")
	}
	*s.trace = append(*s.trace, "marked")
	return nil
}
func (s *updateDispatchStore) FailWorkerUpdate(_ context.Context, _, _ uuid.UUID, code string) error {
	s.failed = code
	return nil
}

type updateDispatchTransport struct {
	trace *[]string
	err   error
}

func (t *updateDispatchTransport) ConnectedWorkers() []uuid.UUID                       { return nil }
func (t *updateDispatchTransport) SendCommand(context.Context, protocol.Command) error { return nil }
func (t *updateDispatchTransport) SendWorkerUpdate(context.Context, protocol.WorkerUpdateRequest) error {
	*t.trace = append(*t.trace, "sent")
	return t.err
}

func TestDispatcherDurablyMarksWorkerUpdateBeforeSendAndPreservesRetries(t *testing.T) {
	for _, scenario := range []string{"sent", "offline", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			var trace []string
			worker := uuid.New()
			store := &updateDispatchStore{request: protocol.WorkerUpdateRequest{WorkerID: worker.String(), RequestID: uuid.NewString()}, trace: &trace}
			transport := &updateDispatchTransport{trace: &trace}
			if scenario == "offline" {
				transport.err = errors.New("connection lost")
			}
			if scenario == "unsupported" {
				transport.err = ErrWorkerUpdateUnsupported
			}
			err := NewDispatcher(store, transport, nil).dispatchWorker(t.Context(), worker)
			if (err != nil) != (scenario == "offline") || !reflect.DeepEqual(trace, []string{"marked", "sent"}) {
				t.Fatalf("update dispatch trace=%v error=%v", trace, err)
			}
			if (store.failed == "unsupported_worker") != (scenario == "unsupported") {
				t.Fatalf("retryable transport error was made terminal: %q", store.failed)
			}
		})
	}
}

func TestHubWorkerUpdateRequiresCapabilityAndCurrentCredentialLease(t *testing.T) {
	for _, supported := range []bool{true, false} {
		t.Run(map[bool]string{true: "supported", false: "old worker"}[supported], func(t *testing.T) {
			store := testRegistry(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			token, _ := auth.GenerateWorkerToken()
			worker := uuid.New()
			if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: worker, Name: "worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken(token)}); err != nil {
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
			sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: worker.String(), WorkerName: "worker", OS: "linux", Arch: "arm64", ProtocolMin: 1, ProtocolMax: 1, SupportsWorkerUpdate: supported})
			if frame, err := readEnvelope(ctx, conn); err != nil || frame.Type != "hello_ack" {
				t.Fatalf("handshake: %#v %v", frame, err)
			}
			request := protocol.WorkerUpdateRequest{WorkerID: worker.String(), RequestID: uuid.NewString()}
			err = hub.SendWorkerUpdate(ctx, request)
			if supported {
				if err != nil {
					t.Fatal(err)
				}
				frame, err := readEnvelope(ctx, conn)
				if err != nil || frame.Type != "worker_update_request" {
					t.Fatalf("worker-only frame: %#v %v", frame, err)
				}
				got, err := protocol.Payload[protocol.WorkerUpdateRequest](frame)
				if err != nil || got != request {
					t.Fatalf("update target changed: %#v %v", got, err)
				}
			} else if !errors.Is(err, ErrWorkerUpdateUnsupported) {
				t.Fatalf("old worker accepted maintenance: %v", err)
			}
			if err := store.RevokeWorker(ctx, worker); err != nil {
				t.Fatal(err)
			}
			if err := hub.SendWorkerUpdate(ctx, request); err == nil || errors.Is(err, ErrWorkerUpdateUnsupported) {
				t.Fatalf("revoked lease not checked first: %v", err)
			}
		})
	}
}

func TestWorkerUpdateCommandAndFrozenResultRendering(t *testing.T) {
	if action, target, text, ignored := parseTelegramText("/tgupdateworkers@mybot", "mybot"); action != "update_workers" || target != "" || text != "" || ignored {
		t.Fatal("missing worker update command")
	}
	if _, _, _, ignored := parseTelegramText("/tgupdateworkers@otherbot", "mybot"); !ignored {
		t.Fatal("foreign bot command accepted")
	}
	found := false
	for _, command := range telegramcommands.Commands() {
		if command.Command == "tgupdateworkers" {
			found = true
		}
	}
	if !found || !strings.Contains(helpText(), "/tgupdateworkers") {
		t.Fatal("update command missing from menu/help")
	}
	sender := &Sender{}
	response := registry.AcceptResult{View: "worker_updates", WorkerUpdates: []registry.WorkerUpdateStatus{{Name: "Build worker", State: "queued"}, {Name: "Offline worker", State: "already_queued"}}}
	text := sender.renderWorkerUpdates(response)
	for _, want := range []string{"Build worker", "Offline worker", "Already queued", "Offline workers", "all its turns"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	response.View = "worker_update_result"
	response.WorkerUpdates = []registry.WorkerUpdateStatus{{Name: "Build worker", State: "up_to_date", Version: "0.5.29"}}
	if text := sender.renderWorkerUpdates(response); !strings.Contains(text, "No worker restart") || strings.Contains(text, "Updated and restarted") {
		t.Fatalf("misleading no-update result: %s", text)
	}
	response.WorkerUpdates[0].State, response.WorkerUpdates[0].ErrorCode = "failed", "unsupported_worker"
	if text := sender.renderWorkerUpdates(response); !strings.Contains(text, "codex-telegramgw update worker") {
		t.Fatalf("missing bootstrap guidance: %s", text)
	}
}

func TestWorkerUpdateRenderingSeparatesCodexCheckFromWorkerSuccess(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{"private-profile"}, "[REDACTED]")
	if err != nil {
		t.Fatal(err)
	}
	sender := &Sender{}
	sender.options.Redactor = redactor
	response := registry.AcceptResult{View: "worker_update_result", WorkerUpdates: []registry.WorkerUpdateStatus{{
		Name: "Worker", State: "completed", Version: "1.1.0",
		Codex: &protocol.CodexUpdateReport{State: "failed", ErrorCode: "check_failed", Profiles: []protocol.CodexRuntimeVersion{{ProfileID: "private-profile", InstalledVersion: "0.157.1", RunningVersion: "0.157.0", Support: "external"}}},
	}}}
	text := sender.renderWorkerUpdates(response)
	for _, want := range []string{"Updated and restarted · 1.1.0", "version check failed", "installed 0.157.1", "running 0.157.0", "externally managed; not updated", "[REDACTED]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "private-profile") || strings.Contains(text, "Update request failed") {
		t.Fatalf("Codex failure erased worker success or leaked profile: %s", text)
	}
	response.WorkerUpdates[0].State = "up_to_date"
	response.WorkerUpdates[0].Codex.State, response.WorkerUpdates[0].Codex.ErrorCode = "completed", ""
	text = sender.renderWorkerUpdates(response)
	if strings.Contains(strings.ToLower(text), "no worker restart") || !strings.Contains(text, "Worker binary up to date") || !strings.Contains(text, "Codex runtime: updated") {
		t.Fatalf("runtime installation falsely claims no restart: %s", text)
	}
	response.WorkerUpdates[0].Codex = nil
	if text := sender.renderWorkerUpdates(response); !strings.Contains(text, "check not reported") {
		t.Fatalf("older worker falsely promises runtime check: %s", text)
	}
}
