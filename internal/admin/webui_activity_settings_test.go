package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestWebUIActivitySettingsBroadcastAndV1Compatibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtimeID, sessionID := uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtimeID, WorkerID: worker.ID, Name: "Runtime", ProfileID: "main", State: "running", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	session := protocol.Session{ID: sessionID.String(), WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), ThreadID: "thread", Name: "PRIVATE NAME", CWD: "/private/path", State: "idle", UpdatedAt: time.Now().UTC()}
	seq := uint64(0)
	emit := func(kind string, settings *protocol.SessionSettings) {
		t.Helper()
		seq++
		session.Settings = settings
		session.Stats = &protocol.SessionStats{Model: "PRIVATE HISTORICAL MODEL", ReasoningEffort: "PRIVATE OLD EFFORT", LastMessage: "PRIVATE MESSAGE", ObservedAt: time.Now()}
		raw, _ := json.Marshal(session)
		if err := store.IngestEvent(ctx, worker.ID, connection, protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: worker.ID.String(), RuntimeID: runtimeID.String(), RuntimeGeneration: 1, SessionID: sessionID.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	emit("session_discovered", nil)
	console, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(console)
	defer server.Close()
	client := passkeyHTTPClient(t, server)
	type viewer struct {
		conn    *websocket.Conn
		version int
	}
	viewers := make([]viewer, 0, 3)
	for _, version := range []int{2, 2, 1} {
		conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/activity?v="+strconv.Itoa(version), &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}, "Cookie": []string{adminCookie + "=" + token}}})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		viewers = append(viewers, viewer{conn, version})
	}
	type frame struct {
		Type      string                     `json:"type"`
		Version   int                        `json:"version"`
		Sequence  uint64                     `json:"sequence"`
		Event     string                     `json:"event"`
		SessionID string                     `json:"session_id"`
		Session   *registry.SessionActivity  `json:"session"`
		Sessions  []registry.SessionActivity `json:"sessions"`
	}
	read := func(view viewer) frame {
		t.Helper()
		_, raw, err := view.conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var result frame
		if err := json.Unmarshal(raw, &result); err != nil || result.Version != view.version || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "/private") {
			t.Fatalf("invalid/private settings frame: %s (%v)", raw, err)
		}
		return result
	}
	for _, view := range viewers {
		initial := read(view)
		if initial.Type != "activity_snapshot" || initial.Sequence != 0 || len(initial.Sessions) != 1 || initial.Sessions[0].SettingsRevision != "" {
			t.Fatalf("initial preferences incorrectly taken from old stats: %+v", initial)
		}
	}
	emit("session_state_changed", &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 1, Model: "model-a", ReasoningEffort: "high"})
	for _, view := range viewers {
		changed := read(view)
		name := "session_settings_changed"
		if view.version == 1 {
			name = "session_changed"
		}
		if changed.Type != "activity_event" || changed.Event != name || changed.Sequence != 1 || changed.SessionID != sessionID.String() || changed.Session == nil || changed.Session.Model != "model-a" || changed.Session.ReasoningEffort != "high" || changed.Session.SettingsRevision != "1:1" {
			t.Fatalf("settings not delivered to v%d viewer: %+v", view.version, changed)
		}
	}
	// All viewers receive every confirmed edge even when the wakeup coalesces.
	emit("session_state_changed", &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 2, Model: "model-b", ReasoningEffort: "low"})
	emit("session_state_changed", &protocol.SessionSettings{RuntimeGeneration: 1, Revision: 3, Model: "model-b", ReasoningEffort: ""})
	for _, view := range viewers {
		second, third := read(view), read(view)
		if second.Sequence != 2 || third.Sequence != 3 || second.Session == nil || third.Session == nil || second.Session.Model != "model-b" || second.Session.ReasoningEffort != "low" || third.Session.ReasoningEffort != "" || third.Session.SettingsRevision != "1:3" {
			t.Fatalf("settings edges/default lost: %+v %+v", second, third)
		}
	}
}
