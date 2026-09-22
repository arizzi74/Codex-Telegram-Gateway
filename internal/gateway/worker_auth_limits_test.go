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

func TestAnonymousFailuresDoNotBlockAuthenticatedWorkers(t *testing.T) {
	store := testRegistry(t)
	token, err := auth.GenerateWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if _, err := store.CreateWorker(context.Background(), registry.CreateWorkerInput{
		ID: id, Name: "legitimate", OS: "unknown", Arch: "unknown", TokenHash: auth.HashWorkerToken(token),
	}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, 3*time.Second)
	server := httptest.NewServer(hub)
	defer server.Close()
	// Fill both the legacy shared loopback bucket and a real client bucket.
	for _, ip := range []string{"", "198.51.100.1"} {
		for i := range 21 {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			if ip != "" {
				r.Header.Set("X-Real-IP", ip)
			}
			r.Header.Set("X-Forwarded-For", "203.0.113.100")
			w := httptest.NewRecorder()
			hub.ServeHTTP(w, r)
			want := http.StatusUnauthorized
			if i == 20 {
				want = http.StatusTooManyRequests
			}
			if w.Code != want {
				t.Fatalf("anonymous request %d = %d, want %d", i, w.Code, want)
			}
		}
	}
	for _, ip := range []string{"", "198.51.100.1", "203.0.113.2"} {
		t.Run("client-"+ip, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			headers := http.Header{"Authorization": []string{"Bearer " + token}}
			if ip != "" {
				headers.Set("X-Real-IP", ip)
			}
			conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &websocket.DialOptions{HTTPHeader: headers})
			if err != nil {
				t.Fatalf("valid worker blocked: response=%v err=%v", response, err)
			}
			defer conn.CloseNow()
			sendFrame(t, ctx, conn, "hello", protocol.Hello{WorkerID: id.String(), WorkerName: "legitimate", OS: "linux", Arch: "amd64", ProtocolMin: 1, ProtocolMax: 1})
			frame, err := readEnvelope(ctx, conn)
			if err != nil || frame.Type != "hello_ack" {
				t.Fatalf("valid worker hello = %+v, %v", frame, err)
			}
		})
	}
}
