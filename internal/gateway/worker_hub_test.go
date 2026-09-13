package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/jackc/pgx/v5"
)

func testRegistry(t *testing.T) *registry.Store {
	t.Helper()
	database := os.Getenv("TEST_DATABASE_URL")
	if database == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL acceptance tests")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	schema := "hub_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		base.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close(context.Background())
	})
	sep := "?"
	if strings.Contains(database, "?") {
		sep = "&"
	}
	store, err := registry.Open(ctx, database+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAT01RegistrationAT02CredentialRejection(t *testing.T) {
	store := testRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	token, _ := auth.GenerateWorkerToken()
	id := uuid.New()
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: id, Name: "enrolled", OS: "unknown", Arch: "unknown", TokenHash: auth.HashWorkerToken(token)}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 3*time.Second)
	server := httptest.NewTLSServer(NewMux(store, hub))
	defer server.Close()
	url := "wss" + strings.TrimPrefix(server.URL, "https") + "/api/v1/workers/connect"
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer invalid"}}})
	if err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("invalid token: %v %v", resp, err)
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: id.String(), WorkerName: "machine", Hostname: "host", OS: "linux", Arch: "arm64", WorkerVersion: "test", ProtocolMin: 1, ProtocolMax: 1})
	frame, err := readEnvelope(ctx, conn)
	if err != nil || frame.Type != "hello_ack" {
		t.Fatalf("hello: %v %v", frame, err)
	}
	workers, err := store.ListWorkers(ctx)
	if err != nil || len(workers) != 1 {
		t.Fatal("missing enrolled worker", err)
	}
	w := workers[0]
	if w.Connectivity != "online" || w.OS != "linux" || w.Arch != "arm64" || w.Version != "test" || w.Hostname != "host" {
		t.Fatalf("registration metadata incorrect: %+v", w)
	}
	if err = store.RevokeWorker(ctx, id); err != nil {
		t.Fatal(err)
	}
	sendFrame(t, ctx, conn, "heartbeat", protocol.Heartbeat{WorkerID: id.String()})
	if _, err = readEnvelope(ctx, conn); err == nil {
		t.Fatal("revoked connection accepted")
	}
}

func TestWorkerHelloCannotClaimAnotherIdentity(t *testing.T) {
	store := testRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token, _ := auth.GenerateWorkerToken()
	id := uuid.New()
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: id, Name: "enrolled", OS: "unknown", Arch: "unknown", TokenHash: auth.HashWorkerToken(token)}); err != nil {
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
	sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: uuid.NewString(), WorkerName: "impostor", OS: "linux", Arch: "arm64", ProtocolMin: 1, ProtocolMax: 1})
	if _, err = readEnvelope(ctx, conn); err == nil {
		t.Fatal("foreign worker identity accepted")
	}
	workers, _ := store.ListWorkers(ctx)
	if workers[0].Connectivity != "offline" {
		t.Fatal("unauthorized metadata was registered")
	}
}

func TestFailureLimiterWindow(t *testing.T) {
	l := &failureLimiter{entries: map[string]failureWindow{}}
	now := time.Now()
	for range 20 {
		l.Fail("ip", now)
	}
	if l.Allow("ip", now) {
		t.Fatal("authentication failures not limited")
	}
	if !l.Allow("ip", now.Add(time.Minute)) {
		t.Fatal("authentication limit did not expire")
	}
}

func sendFrame(t *testing.T, ctx context.Context, c *websocket.Conn, kind string, payload any) {
	t.Helper()
	frame, err := protocol.NewEnvelope(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}
