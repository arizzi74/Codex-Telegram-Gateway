package worker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestConnectionAdvertisesImageInputThroughoutConnection(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	observed := make(chan struct{}, 1)
	server := newWorkerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		helloEnvelope, err := readWorkerEnvelope(r.Context(), conn)
		if err != nil {
			t.Error(err)
			return
		}
		hello, err := protocol.Payload[protocol.Hello](helloEnvelope)
		if err != nil || helloEnvelope.Type != "hello" || !hello.SupportsImageInput {
			t.Errorf("hello did not advertise image input: %#v, %v", hello, err)
			return
		}
		if err := serverEnvelope(r.Context(), conn, "hello_ack", protocol.HelloAck{ConnectionID: uuid.NewString(), HeartbeatIntervalSeconds: 1}); err != nil {
			t.Error(err)
			return
		}
		heartbeatEnvelope, err := readWorkerEnvelope(r.Context(), conn)
		if err != nil {
			t.Error(err)
			return
		}
		heartbeat, err := protocol.Payload[protocol.Heartbeat](heartbeatEnvelope)
		if err != nil || heartbeatEnvelope.Type != "heartbeat" || !heartbeat.SupportsImageInput {
			t.Errorf("heartbeat lost image input capability: %#v, %v", heartbeat, err)
			return
		}
		observed <- struct{}{}
	}))
	defer server.Close()
	c := testConnection(t, cfg, store, server.URL, server.Client(), func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		return protocol.CommandAck{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.connect(ctx)
	select {
	case <-observed:
	default:
		t.Fatal("connection did not advertise image support in hello and heartbeat")
	}
}

func TestConnectionReplaysOutboxAfterReconnectAndAcknowledges(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	const eventCount = 16
	var eventsAdded []protocol.Event
	for range eventCount {
		event, err := store.AppendEvent(protocol.Event{Kind: "runtime_started"})
		if err != nil {
			t.Fatal(err)
		}
		eventsAdded = append(eventsAdded, event)
	}
	var connections atomic.Int32
	server := newWorkerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, err = readWorkerEnvelope(ctx, conn); err != nil {
			t.Error(err)
			return
		}
		count := connections.Add(1)
		if err = serverEnvelope(ctx, conn, "hello_ack", protocol.HelloAck{ConnectionID: uuid.NewString(), HeartbeatIntervalSeconds: 60, ResumeFromEventSeq: 0}); err != nil {
			t.Error(err)
			return
		}
		reads := eventCount
		if count == 1 {
			reads = 1
		} // Drop the first connection before acknowledging.
		for i := range reads {
			got, err := readWorkerEnvelope(ctx, conn)
			if err != nil {
				t.Error(err)
				return
			}
			if got.Type != "worker_event" {
				t.Errorf("got %s", got.Type)
				return
			}
			persisted, err := protocol.Payload[protocol.Event](got)
			if err != nil || persisted.Seq != eventsAdded[i].Seq {
				t.Errorf("event %#v: %v", persisted, err)
				return
			}
			if count > 1 {
				if err = serverEnvelope(ctx, conn, "event_ack", protocol.EventAck{Seq: persisted.Seq}); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}))
	defer server.Close()
	c := testConnection(t, cfg, store, server.URL, server.Client(), func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		return protocol.CommandAck{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.connect(ctx)
	_ = c.connect(ctx)
	if connections.Load() != 2 {
		t.Fatalf("connections = %d", connections.Load())
	}
	events, err := store.OutboxAfter(0)
	if err != nil || len(events) != 0 {
		t.Fatalf("acknowledged event remained in outbox: %#v, %v", events, err)
	}
}

func TestConnectionDispatchesCommandWithoutTransportDeduplication(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	command := testCommand(workerID)
	called := make(chan struct{}, 1)
	server := newWorkerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, err = readWorkerEnvelope(ctx, conn); err != nil {
			t.Error(err)
			return
		}
		if err = serverEnvelope(ctx, conn, "hello_ack", protocol.HelloAck{ConnectionID: uuid.NewString(), HeartbeatIntervalSeconds: 60}); err != nil {
			t.Error(err)
			return
		}
		if err = serverEnvelope(ctx, conn, "command", command); err != nil {
			t.Error(err)
			return
		}
		ackEnvelope, err := readWorkerEnvelope(ctx, conn)
		if err != nil {
			t.Error(err)
			return
		}
		ack, err := protocol.Payload[protocol.CommandAck](ackEnvelope)
		if err != nil || ackEnvelope.Type != "command_ack" || ack.CommandID != command.ID || ack.Status != "accepted" {
			t.Errorf("ack %#v: %v", ack, err)
		}
	}))
	defer server.Close()
	c := testConnection(t, cfg, store, server.URL, server.Client(), func(_ context.Context, got protocol.Command) (protocol.CommandAck, error) {
		if got.ID != command.ID {
			t.Errorf("wrong command")
		}
		called <- struct{}{}
		return protocol.CommandAck{CommandID: got.ID, Status: "accepted"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.connect(ctx)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("handler was not called")
	}
	if _, found, err := store.LoadCommand(command.ID); err != nil || found {
		t.Fatalf("transport duplicated handler ledger work: found=%t err=%v", found, err)
	}
}

func TestConnectionReceivesCommandsInFIFOOrder(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	first, second := testCommand(workerID), testCommand(workerID)
	var mu sync.Mutex
	var received []string
	server := newWorkerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, err = readWorkerEnvelope(ctx, conn); err != nil {
			t.Error(err)
			return
		}
		if err = serverEnvelope(ctx, conn, "hello_ack", protocol.HelloAck{ConnectionID: uuid.NewString(), HeartbeatIntervalSeconds: 60}); err != nil {
			t.Error(err)
			return
		}
		for _, command := range []protocol.Command{first, second} {
			if err = serverEnvelope(ctx, conn, "command", command); err != nil {
				t.Error(err)
				return
			}
		}
		for _, expected := range []string{first.ID, second.ID} {
			envelope, err := readWorkerEnvelope(ctx, conn)
			if err != nil {
				t.Error(err)
				return
			}
			ack, err := protocol.Payload[protocol.CommandAck](envelope)
			if err != nil || ack.CommandID != expected {
				t.Errorf("ack order: %#v %v", ack, err)
				return
			}
		}
	}))
	defer server.Close()
	c := testConnection(t, cfg, store, server.URL, server.Client(), func(_ context.Context, command protocol.Command) (protocol.CommandAck, error) {
		mu.Lock()
		received = append(received, command.ID)
		mu.Unlock()
		return protocol.CommandAck{CommandID: command.ID, Status: "accepted"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.connect(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || received[0] != first.ID || received[1] != second.ID {
		t.Fatalf("command handler order: %v", received)
	}
}

func TestConnectionRejectsGatewayWatermarkRollback(t *testing.T) {
	workerID := uuid.NewString()
	store, cfg := testConnectionStore(t, workerID)
	defer store.Close()
	if _, err := store.AppendEvent(protocol.Event{Kind: "turn_completed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AckThrough(1); err != nil {
		t.Fatal(err)
	}
	server := newWorkerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, err = readWorkerEnvelope(r.Context(), conn); err != nil {
			t.Error(err)
			return
		}
		if err = serverEnvelope(r.Context(), conn, "hello_ack", protocol.HelloAck{ConnectionID: uuid.NewString(), HeartbeatIntervalSeconds: 60, ResumeFromEventSeq: 0}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := testConnection(t, cfg, store, server.URL, server.Client(), func(context.Context, protocol.Command) (protocol.CommandAck, error) {
		return protocol.CommandAck{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.connect(ctx); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("watermark rollback was accepted: %v", err)
	}
}

func testConnectionStore(t *testing.T, workerID string) (*Store, config.WorkerConfig) {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("cwk_test_token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"), workerID)
	if err != nil {
		t.Fatal(err)
	}
	return store, config.WorkerConfig{WorkerID: workerID, Name: "test-worker", GatewayURL: "", TokenFile: token}
}

func newWorkerTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local socket unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.StartTLS()
	return server
}

func testConnection(t *testing.T, cfg config.WorkerConfig, store *Store, serverURL string, client *http.Client, handler func(context.Context, protocol.Command) (protocol.CommandAck, error)) *Connection {
	t.Helper()
	// httptest serves HTTPS; websocket.Dial must use WSS and its trusted test client.
	cfg.GatewayURL = "wss://" + strings.TrimPrefix(strings.TrimPrefix(serverURL, "https://"), "http://")
	c, err := NewConnection(cfg, store, nil, nil, handler)
	if err != nil {
		t.Fatal(err)
	}
	c.dial = func(ctx context.Context, url string, options *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		copy := *options
		copy.HTTPClient = client
		return websocket.Dial(ctx, url, &copy)
	}
	return c
}

func serverEnvelope(ctx context.Context, conn *websocket.Conn, kind string, payload any) error {
	e, err := protocol.NewEnvelope(kind, payload)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
