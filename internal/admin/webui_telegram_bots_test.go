package admin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
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

// Exercise the normal browser registration and signed assertion flow. These
// tests never install invented credential JSON or bypass session validation.
func webUITelegramBotBrowser(t *testing.T, store *registry.Store) (*Server, *http.Client, string) {
	t.Helper()
	console, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(console)
	t.Cleanup(server.Close)
	client := passkeyHTTPClient(t, server)
	t.Cleanup(client.CloseIdleConnections)
	bootstrap, err := store.BootstrapAdmin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	requireHTTPStatus(t, adminRequest(t, client, passkeyTestOrigin, "GET", "/tgw/webui/", nil, "", ""), http.StatusOK)
	csrf := cookieValue(t, client, passkeyTestOrigin, csrfCookie)
	var ceremony struct {
		ID        string `json:"ceremony_id"`
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	decodeHTTPJSON(t, adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/passkeys/register/begin",
		map[string]string{"bootstrap_token": bootstrap}, passkeyTestOrigin, csrf), http.StatusOK, &ceremony)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credentialID := []byte(uuid.NewString())
	userHandle, err := base64.RawURLEncoding.DecodeString(ceremony.PublicKey.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	rpID := strings.TrimPrefix(passkeyTestOrigin, "https://")
	credential := registrationCredential(t, key, credentialID, ceremony.PublicKey.Challenge, passkeyTestOrigin, rpID, 0x45)
	requireHTTPStatus(t, registrationFinishRequest(t, client, passkeyTestOrigin, csrf, ceremony.ID, bootstrap, credential), http.StatusCreated)
	decodeHTTPJSON(t, adminRequest(t, client, passkeyTestOrigin, "POST", "/tgw/api/v1/admin/login/begin", struct{}{}, passkeyTestOrigin, csrf), http.StatusOK, &ceremony)
	assertion := browserAssertion(t, key, credentialID, userHandle, ceremony.PublicKey.Challenge, passkeyTestOrigin, rpID, 0x05)
	requireHTTPStatus(t, loginFinishRequest(t, client, passkeyTestOrigin, csrf, ceremony.ID, assertion), http.StatusOK)
	return console, client, cookieValue(t, client, passkeyTestOrigin, adminCookie)
}

type webUITelegramBotFixture struct {
	store    *registry.Store
	worker   uuid.UUID
	runtime  uuid.UUID
	sessions [2]uuid.UUID
	bots     [2]registry.TelegramBotRecord
	emit     func(uuid.UUID, string, any)
}

func newWebUITelegramBotFixture(t *testing.T) webUITelegramBotFixture {
	t.Helper()
	ctx := t.Context()
	f := webUITelegramBotFixture{store: adminIntegrationStore(t), runtime: uuid.New(), sessions: [2]uuid.UUID{uuid.New(), uuid.New()}}
	worker, err := f.store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-selector-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	f.worker = worker.ID
	connection := uuid.New()
	if err := f.store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection,
		Runtimes: []registry.Runtime{{ID: f.runtime, WorkerID: worker.ID, Name: "Runtime", ProfileID: "main", State: "running", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	sequence := uint64(0)
	f.emit = func(id uuid.UUID, kind string, value any) {
		t.Helper()
		sequence++
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.IngestEvent(ctx, worker.ID, connection, protocol.Event{ID: uuid.NewString(), Seq: sequence, WorkerID: worker.ID.String(), RuntimeID: f.runtime.String(), RuntimeGeneration: 1,
			SessionID: id.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range f.sessions {
		f.emit(id, "session_discovered", protocol.Session{ID: id.String(), WorkerID: worker.ID.String(), RuntimeID: f.runtime.String(), ThreadID: "selector-thread-" + strconv.Itoa(i),
			Name: "private-selector-conversation", CWD: "/private/selector", State: "idle", UpdatedAt: time.Now().UTC()})
	}
	for i := range f.bots {
		f.bots[i] = registry.TelegramBotRecord{ID: "selector-bot-" + strconv.Itoa(i), TelegramID: int64(77001 + i), OwnerID: int64(88001 + i), Username: "SelectorBot" + strconv.Itoa(i),
			DisplayName: "Selector Bot " + strconv.Itoa(i), Enabled: true, EncryptedCredentials: []byte("private-selector-credentials"), LastError: "private-selector-error", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if err := f.store.SaveTelegramBotRecord(ctx, f.bots[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.SetTelegramBotSessionName(ctx, f.bots[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.store.CaptureTelegramBotOriginalName(ctx, f.bots[0].ID, "Original Selector Bot"); err != nil {
		t.Fatal(err)
	}
	return f
}

func webUITelegramBotSnapshotForTest(t *testing.T, response *httptest.ResponseRecorder) registry.WebUITelegramBotSnapshot {
	t.Helper()
	var snapshot registry.WebUITelegramBotSnapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Revision == "" || snapshot.Bots == nil {
		t.Fatalf("bot snapshot: status=%d body=%s", response.Code, response.Body.String())
	}
	var exact map[string]json.RawMessage
	if json.Unmarshal(response.Body.Bytes(), &exact) != nil || len(exact) != 2 || exact["revision"] == nil || exact["bots"] == nil {
		t.Fatalf("unexpected bot snapshot fields: %s", response.Body.String())
	}
	var bots []map[string]json.RawMessage
	if json.Unmarshal(exact["bots"], &bots) != nil {
		t.Fatal("invalid bots projection")
	}
	for _, bot := range bots {
		if len(bot) != 3 || bot["id"] == nil || bot["display_name"] == nil || bot["session_id"] == nil {
			t.Fatalf("unsafe bot projection: %s", response.Body.String())
		}
	}
	if strings.Contains(response.Body.String(), "private-selector") {
		t.Fatalf("bot response leaked private details: %s", response.Body.String())
	}
	return snapshot
}

func TestWebUITelegramBotsAuthenticationCSRFAndStrictInput(t *testing.T) {
	f := newWebUITelegramBotFixture(t)
	console, _, token := webUITelegramBotBrowser(t, f.store)
	body := `{"session_id":"` + f.sessions[0].String() + `","bot_id":"` + f.bots[0].ID + `"}`
	initial, err := f.store.WebUITelegramBots(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, query, body, token, origin, header, cookie string
		status                                                   int
	}{
		{"GET missing auth", "GET", "", "", "", "", "", "", 401},
		{"GET invalid auth", "GET", "", "", "invalid", "", "", "", 401},
		{"POST missing auth", "POST", "", body, "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"POST invalid auth", "POST", "", body, "invalid", passkeyTestOrigin, "csrf", "csrf", 401},
		{"missing origin", "POST", "", body, token, "", "csrf", "csrf", 403},
		{"wrong origin", "POST", "", body, token, "https://evil.example", "csrf", "csrf", 403},
		{"different port", "POST", "", body, token, passkeyTestOrigin + ":8443", "csrf", "csrf", 403},
		{"missing CSRF header", "POST", "", body, token, passkeyTestOrigin, "", "csrf", 403},
		{"missing CSRF cookie", "POST", "", body, token, passkeyTestOrigin, "csrf", "", 403},
		{"mismatched CSRF", "POST", "", body, token, passkeyTestOrigin, "different", "csrf", 403},
		{"missing session", "POST", "", `{"bot_id":""}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"missing bot", "POST", "", `{"session_id":"` + f.sessions[0].String() + `"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"nil session", "POST", "", `{"session_id":"` + uuid.Nil.String() + `","bot_id":""}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"invalid session", "POST", "", `{"session_id":"bad","bot_id":""}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"null bot", "POST", "", `{"session_id":"` + f.sessions[0].String() + `","bot_id":null}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"numeric bot", "POST", "", `{"session_id":"` + f.sessions[0].String() + `","bot_id":12}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"unknown owner", "POST", "", body[:len(body)-1] + `,"owner_id":88001}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"unknown chat", "POST", "", body[:len(body)-1] + `,"chat_id":88001}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"query destination", "POST", "?owner_id=88001", body, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"duplicate bot", "POST", "", body[:len(body)-1] + `,"bot_id":""}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"case changed field", "POST", "", strings.Replace(body, "bot_id", "Bot_ID", 1), token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"trailing JSON", "POST", "", body + `{}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"nonobject JSON", "POST", "", `null`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"oversized JSON", "POST", "", `{"session_id":"` + f.sessions[0].String() + `","bot_id":"` + strings.Repeat("x", maxWebUITelegramBotBody) + `"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"unknown session", "POST", "", `{"session_id":"` + uuid.NewString() + `","bot_id":"` + f.bots[0].ID + `"}`, token, passkeyTestOrigin, "csrf", "csrf", 404},
		{"unknown bot", "POST", "", `{"session_id":"` + f.sessions[0].String() + `","bot_id":"private-selector-unknown"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"wrong method", "DELETE", "", body, token, passkeyTestOrigin, "csrf", "csrf", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := webUICommandRequestForTest(console, tc.method, "/tgw/api/v1/webui/telegram-bots"+tc.query, tc.body, tc.token, tc.origin, tc.header, tc.cookie)
			if response.Code != tc.status || response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "private-selector") {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
			}
		})
	}
	latest, err := f.store.WebUITelegramBots(t.Context())
	if err != nil || !reflect.DeepEqual(initial, latest) {
		t.Fatalf("rejected requests changed routing: before=%+v after=%+v err=%v", initial, latest, err)
	}
	if err := f.store.RevokeAdminSession(t.Context(), token); err != nil {
		t.Fatal(err)
	}
	response := webUICommandRequestForTest(console, "GET", "/tgw/api/v1/webui/telegram-bots", "", token, "", "", "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session can list bots: %d", response.Code)
	}
}

func TestWebUITelegramBotsReturnsAuthoritativeExclusiveRouting(t *testing.T) {
	f := newWebUITelegramBotFixture(t)
	console, _, token := webUITelegramBotBrowser(t, f.store)
	read := func() registry.WebUITelegramBotSnapshot {
		return webUITelegramBotSnapshotForTest(t, webUICommandRequestForTest(console, "GET", "/tgw/api/v1/webui/telegram-bots", "", token, "", "", ""))
	}
	set := func(id uuid.UUID, botID string) registry.WebUITelegramBotSnapshot {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"session_id": id.String(), "bot_id": botID})
		response := webUICommandRequestForTest(console, "POST", "/tgw/api/v1/webui/telegram-bots", string(body), token, passkeyTestOrigin, "csrf", "csrf")
		snapshot := webUITelegramBotSnapshotForTest(t, response)
		if latest := read(); !reflect.DeepEqual(snapshot, latest) {
			t.Fatalf("POST did not return authoritative snapshot: %+v %+v", snapshot, latest)
		}
		return snapshot
	}
	assertRoutes := func(snapshot registry.WebUITelegramBotSnapshot, first, second string) {
		t.Helper()
		if len(snapshot.Bots) != 2 || snapshot.Bots[0].ID != f.bots[0].ID || snapshot.Bots[0].DisplayName != "Original Selector Bot" || snapshot.Bots[0].SessionID != first || snapshot.Bots[1].SessionID != second {
			t.Fatalf("incorrect/original-name bot routing: %+v", snapshot)
		}
	}
	initial := read()
	assertRoutes(initial, "", "")
	selected := set(f.sessions[0], f.bots[0].ID)
	assertRoutes(selected, f.sessions[0].String(), "")
	if selected.Revision == initial.Revision {
		t.Fatal("selection did not change metadata revision")
	}
	if duplicate := set(f.sessions[0], f.bots[0].ID); duplicate.Revision != selected.Revision {
		t.Fatal("idempotent selection changed metadata revision")
	}
	assertRoutes(set(f.sessions[1], f.bots[0].ID), f.sessions[1].String(), "")
	assertRoutes(set(f.sessions[1], f.bots[1].ID), "", f.sessions[1].String())
	assertRoutes(set(f.sessions[0], ""), "", f.sessions[1].String())
	assertRoutes(set(f.sessions[1], ""), "", "")
	// Disabled bots disappear and cannot be selected even if an old tab keeps
	// their ID. No management credentials or private destination IDs are sent.
	disabled := f.bots[0]
	disabled.Enabled = false
	if err := f.store.SaveTelegramBotRecord(t.Context(), disabled); err != nil {
		t.Fatal(err)
	}
	if snapshot := read(); len(snapshot.Bots) != 1 || snapshot.Bots[0].ID != f.bots[1].ID {
		t.Fatalf("disabled bot still offered: %+v", snapshot)
	}
	body := `{"session_id":"` + f.sessions[0].String() + `","bot_id":"` + disabled.ID + `"}`
	if response := webUICommandRequestForTest(console, "POST", "/tgw/api/v1/webui/telegram-bots", body, token, passkeyTestOrigin, "csrf", "csrf"); response.Code != http.StatusBadRequest {
		t.Fatalf("disabled bot accepted: %d %s", response.Code, response.Body.String())
	}
}

func TestWebUIActivityV3BotRoutingBroadcastAndV2Compatibility(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	f := newWebUITelegramBotFixture(t)
	_, client, token := webUITelegramBotBrowser(t, f.store)
	type viewer struct {
		conn     *websocket.Conn
		version  int
		sequence uint64
	}
	dial := func(version int) *viewer {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/activity?v="+strconv.Itoa(version),
			&websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		return &viewer{conn: conn, version: version}
	}
	type frame struct {
		Type                 string                      `json:"type"`
		Version              int                         `json:"version"`
		Sequence             uint64                      `json:"sequence"`
		Event                string                      `json:"event"`
		SessionID            string                      `json:"session_id"`
		Session              *registry.SessionActivity   `json:"session"`
		Sessions             []registry.SessionActivity  `json:"sessions"`
		TelegramBots         []registry.WebUITelegramBot `json:"telegram_bots"`
		TelegramBotsRevision string                      `json:"telegram_bots_revision"`
	}
	read := func(view *viewer, kind, event string) frame {
		t.Helper()
		_, raw, err := view.conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var result frame
		if json.Unmarshal(raw, &result) != nil || result.Type != kind || result.Event != event || result.Version != view.version || result.Sequence != view.sequence || strings.Contains(string(raw), "private-selector") {
			t.Fatalf("incorrect/private activity frame: %s", raw)
		}
		view.sequence++
		if view.version < 3 && strings.Contains(string(raw), "telegram_bots") {
			t.Fatalf("v2 received v3 metadata: %s", raw)
		}
		if event == "telegram_bots_changed" {
			var envelope map[string]json.RawMessage
			if json.Unmarshal(raw, &envelope) != nil || envelope["session_id"] != nil || envelope["session"] != nil {
				t.Fatalf("global bot event has a session payload: %s", raw)
			}
		}
		return result
	}
	viewers := []*viewer{dial(3), dial(3), dial(2)}
	for _, view := range viewers {
		initial := read(view, "activity_snapshot", "")
		if len(initial.Sessions) != 2 || (view.version == 3 && (len(initial.TelegramBots) != 2 || initial.TelegramBotsRevision == "")) {
			t.Fatalf("missing initial activity/bot snapshot: %+v", initial)
		}
	}
	assertBotUpdate := func(first, second string) {
		t.Helper()
		latest, err := f.store.WebUITelegramBots(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, view := range viewers[:2] {
			update := read(view, "activity_event", "telegram_bots_changed")
			if !reflect.DeepEqual(update.TelegramBots, latest.Bots) || update.TelegramBotsRevision != latest.Revision || update.TelegramBots[0].SessionID != first || update.TelegramBots[1].SessionID != second {
				t.Fatalf("bot update differs from committed routing: %+v", update)
			}
		}
	}
	if _, err := f.store.SetWebUITelegramBot(ctx, f.sessions[0], f.bots[0].ID); err != nil {
		t.Fatal(err)
	}
	assertBotUpdate(f.sessions[0].String(), "")
	// A canonical Telegram picker callback changes routing without a WebUI
	// POST. Every v3 viewer receives that committed move, clearing the old row.
	callback, err := f.store.CreateCallback(ctx, registry.Callback{Action: "select", BotID: f.bots[0].ID, UserID: f.bots[0].OwnerID, ChatID: f.bots[0].OwnerID,
		SessionID: f.sessions[1], RuntimeID: f.runtime, Generation: 1, ExpiresAt: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: f.bots[0].ID, UserID: f.bots[0].OwnerID, ChatID: f.bots[0].OwnerID, UpdateID: 1, CallbackToken: callback})
	if err != nil || result.View != "selected" {
		t.Fatalf("Telegram picker selection failed: %+v %v", result, err)
	}
	assertBotUpdate(f.sessions[1].String(), "")
	// Session activity shares the same sequence, and v2 receives exactly its
	// original session event despite both intervening bot metadata changes.
	f.emit(f.sessions[0], "turn_started", protocol.Result{TurnID: "selector-turn", State: "running"})
	for _, view := range viewers {
		turn := read(view, "activity_event", "turn_started")
		if turn.SessionID != f.sessions[0].String() || turn.Session == nil || turn.Session.ActiveTurnID != "selector-turn" {
			t.Fatalf("session event changed in v%d: %+v", view.version, turn)
		}
	}
	disabled := f.bots[0]
	disabled.Enabled = false
	if err := f.store.SaveTelegramBotRecord(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	for _, view := range viewers[:2] {
		update := read(view, "activity_event", "telegram_bots_changed")
		if len(update.TelegramBots) != 1 || update.TelegramBots[0].ID != f.bots[1].ID {
			t.Fatalf("disabled bot remained in live metadata: %+v", update)
		}
	}
	// Reconnecting starts at sequence zero with the latest routing metadata.
	reconnected := dial(3)
	initial := read(reconnected, "activity_snapshot", "")
	if len(initial.TelegramBots) != 1 || initial.TelegramBots[0].ID != f.bots[1].ID {
		t.Fatalf("reconnect carried stale bot list: %+v", initial)
	}
	if err := f.store.RevokeAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetWebUITelegramBot(ctx, f.sessions[0], f.bots[1].ID); err != nil {
		t.Fatal(err)
	}
	for _, view := range append(viewers, reconnected) {
		_, raw, err := view.conn.Read(ctx)
		if err != nil || !strings.Contains(string(raw), `"type":"error"`) || strings.Contains(string(raw), "telegram_bots") {
			t.Fatalf("revoked bot feed leaked metadata: %s %v", raw, err)
		}
		if _, _, err := view.conn.Read(ctx); websocket.CloseStatus(err) != 4001 {
			t.Fatalf("revoked bot feed remained open: %v", err)
		}
	}
}

func TestWebUIActivityV3SnapshotsAndHeartbeatReadExternalBotChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "shared-gateway.db")
	store, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	_, client, _ := webUITelegramBotBrowser(t, store)
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(passkeyTestOrigin, "https")+"/tgw/api/v1/webui/activity?v=3",
		&websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{passkeyTestOrigin}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	type frame struct {
		Type                 string                      `json:"type"`
		Version              int                         `json:"version"`
		Sequence             uint64                      `json:"sequence"`
		Event                string                      `json:"event"`
		TelegramBots         []registry.WebUITelegramBot `json:"telegram_bots"`
		TelegramBotsRevision string                      `json:"telegram_bots_revision"`
	}
	read := func(kind string, sequence uint64) frame {
		t.Helper()
		_, raw, err := conn.Read(ctx)
		var result frame
		if err != nil || json.Unmarshal(raw, &result) != nil || result.Type != kind || result.Version != 3 || result.Sequence != sequence {
			t.Fatalf("external bot activity frame: %s %v", raw, err)
		}
		return result
	}
	initial := read("activity_snapshot", 0)
	if initial.TelegramBots == nil || len(initial.TelegramBots) != 0 || initial.TelegramBotsRevision == "" {
		t.Fatalf("empty bot metadata is not an array/revision: %+v", initial)
	}
	// This independent store shares SQLite but has its own activity hub, as
	// another gateway process would. Its commits cannot wake this connection.
	external, err := registry.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(external.Close)
	bot := registry.TelegramBotRecord{ID: "external-selector-bot", TelegramID: 90001, OwnerID: 91001, Username: "ExternalSelectorBot", DisplayName: "External Selector Bot",
		Enabled: true, EncryptedCredentials: []byte("private-selector-external"), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := external.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	// An inventory resnapshot must fetch fresh bot metadata even though the
	// metadata change itself never notified this process's activity hub.
	if _, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "Inventory refresh worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("private-selector-worker")}); err != nil {
		t.Fatal(err)
	}
	resnapshot := read("activity_snapshot", 1)
	if len(resnapshot.TelegramBots) != 1 || resnapshot.TelegramBots[0].ID != bot.ID || resnapshot.TelegramBotsRevision == initial.TelegramBotsRevision {
		t.Fatalf("inventory resnapshot retained old bot metadata: %+v", resnapshot)
	}
	bot.DisplayName = "Updated External Selector Bot"
	if err := external.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	updated := read("activity_event", 2)
	if updated.Event != "telegram_bots_changed" || len(updated.TelegramBots) != 1 || updated.TelegramBots[0].DisplayName != bot.DisplayName || updated.TelegramBotsRevision == resnapshot.TelegramBotsRevision {
		t.Fatalf("heartbeat did not refresh external bot metadata: %+v", updated)
	}
	read("heartbeat", 2)
}
