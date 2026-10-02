package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIActivityRefreshesZeroSessionWorkerInventory(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	console, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(console)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/activity?v=2",
		&websocket.DialOptions{HTTPClient: passkeyHTTPClient(t, server), HTTPHeader: http.Header{
			"Origin": []string{passkeyTestOrigin}, "Cookie": []string{adminCookie + "=" + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	type frame struct {
		Type              string                     `json:"type"`
		Version           int                        `json:"version"`
		Sequence          uint64                     `json:"sequence"`
		InventoryRevision string                     `json:"inventory_revision"`
		Sessions          []registry.SessionActivity `json:"sessions"`
	}
	wantSequence := uint64(0)
	read := func() frame {
		t.Helper()
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var result frame
		if json.Unmarshal(raw, &result) != nil || result.Type != "activity_snapshot" || result.Version != 2 || result.Sequence != wantSequence ||
			len(result.InventoryRevision) != 64 || result.Sessions == nil || len(result.Sessions) != 0 {
			t.Fatalf("empty worker snapshot: %s", raw)
		}
		if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "/home/") {
			t.Fatalf("inventory fingerprint leaked metadata: %s", raw)
		}
		wantSequence++
		return result
	}
	initial := read()
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "PRIVATE fresh worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("PRIVATE token")})
	if err != nil {
		t.Fatal(err)
	}
	enrolled := read()
	if enrolled.InventoryRevision == initial.InventoryRevision {
		t.Fatal("new worker did not refresh zero-session activity")
	}
	connection := uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	connected := read()
	if connected.InventoryRevision == enrolled.InventoryRevision {
		t.Fatal("worker connection did not update empty inventory")
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection,
		Runtimes: []registry.Runtime{{ID: uuid.New(), WorkerID: worker.ID, ProfileID: "primary", Name: "PRIVATE runtime", State: "running", Generation: 1, DefaultCWD: "/home/private"}}}); err != nil {
		t.Fatal(err)
	}
	runtimeReady := read()
	if runtimeReady.InventoryRevision == connected.InventoryRevision {
		t.Fatal("first runtime did not update zero-session inventory")
	}
}
