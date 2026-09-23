package admin

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/webpush"
)

func webPushTestSubscription(t *testing.T) webpush.Subscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := webpush.Subscription{Endpoint: "https://fcm.googleapis.com/private-device-endpoint"}
	s.Keys.P256DH = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString([]byte("privateauthkey16"))
	return s
}

func TestWebUIPushRequiresPasskeyOriginAndCSRF(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	sub := webPushTestSubscription(t)
	raw, _ := json.Marshal(map[string]any{"subscription": sub})
	for _, tc := range []struct {
		name, method, path, body, token, origin, header, cookie string
		status                                                  int
	}{
		{"config authentication", "GET", "config", "", "", "", "", "", 401},
		{"config method", "POST", "config", "{}", token, passkeyTestOrigin, "csrf", "csrf", 405},
		{"subscribe authentication", "POST", "subscribe", string(raw), "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"subscribe origin", "POST", "subscribe", string(raw), token, "https://evil.example", "csrf", "csrf", 403},
		{"subscribe CSRF", "POST", "subscribe", string(raw), token, passkeyTestOrigin, "bad", "csrf", 403},
		{"subscribe method", "GET", "subscribe", "", token, "", "", "", 405},
		{"subscribe unknown field", "POST", "subscribe", `{"subscription":{},"scope":"all"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"subscribe unknown nested field", "POST", "subscribe", strings.Replace(string(raw), `"endpoint":`, `"unknown":true,"endpoint":`, 1), token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"subscribe invalid key", "POST", "subscribe", `{"subscription":{"endpoint":"https://fcm.googleapis.com/x","keys":{"auth":"bad","p256dh":"bad"}}}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"subscribe localhost", "POST", "subscribe", strings.Replace(string(raw), sub.Endpoint, "https://localhost/private", 1), token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"subscribe oversized", "POST", "subscribe", strings.Replace(string(raw), sub.Endpoint, "https://fcm.googleapis.com/"+strings.Repeat("x", 8192), 1), token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"unsubscribe authentication", "POST", "unsubscribe", `{"subscription_id":"` + uuid.NewString() + `"}`, "", passkeyTestOrigin, "csrf", "csrf", 401},
		{"unsubscribe CSRF", "POST", "unsubscribe", `{"subscription_id":"` + uuid.NewString() + `"}`, token, passkeyTestOrigin, "bad", "csrf", 403},
		{"unsubscribe invalid ID", "POST", "unsubscribe", `{"subscription_id":"bad"}`, token, passkeyTestOrigin, "csrf", "csrf", 400},
		{"config invalid ID", "GET", "config?subscription_id=bad", "", token, "", "", "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := webUICommandRequestForTest(server, tc.method, "/tgw/api/v1/webui/push/"+tc.path, tc.body, tc.token, tc.origin, tc.header, tc.cookie)
			if response.Code != tc.status {
				t.Fatalf("status=%d want%d body=%s", response.Code, tc.status, response.Body.String())
			}
		})
	}
}

func TestWebUIPushSubscriptionConfigAndLogout(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	config := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/push/config", "", token, "", "", "")
	var initial map[string]any
	if config.Code != 200 || json.Unmarshal(config.Body.Bytes(), &initial) != nil || initial["supported"] != true || initial["subscribed"] != false || initial["scope"] != "all" {
		t.Fatalf("config=%d %s", config.Code, config.Body.String())
	}
	keys, err := store.EnsureWebPushKeys(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if initial["public_key"] != keys.PublicKey || strings.Contains(config.Body.String(), keys.PrivateKey) {
		t.Fatal("wrong public key projection")
	}
	sub := webPushTestSubscription(t)
	raw, _ := json.Marshal(map[string]any{"subscription": sub})
	var id string
	for range 2 {
		response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/push/subscribe", string(raw), token, passkeyTestOrigin, "csrf", "csrf")
		var result struct {
			SubscriptionID string `json:"subscription_id"`
			Subscribed     bool   `json:"subscribed"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Subscribed || result.SubscriptionID == "" {
			t.Fatalf("subscribe=%d %s", response.Code, response.Body.String())
		}
		if id != "" && id != result.SubscriptionID {
			t.Fatal("repeat subscription created duplicate")
		}
		id = result.SubscriptionID
		if strings.Contains(response.Body.String(), sub.Endpoint) || strings.Contains(response.Body.String(), sub.Keys.Auth) || strings.Contains(response.Body.String(), keys.PrivateKey) {
			t.Fatal("subscription capabilities leaked")
		}
	}
	config = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/push/config?subscription_id="+id, "", token, "", "", "")
	if config.Code != 200 || !strings.Contains(config.Body.String(), `"subscribed":true`) {
		t.Fatalf("subscribed config=%d %s", config.Code, config.Body.String())
	}
	other := webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/push/config?subscription_id="+uuid.NewString(), "", token, "", "", "")
	if other.Code != 200 || !strings.Contains(other.Body.String(), `"subscribed":false`) {
		t.Fatal("unknown device appeared registered")
	}
	for range 2 {
		response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/push/unsubscribe", `{"subscription_id":"`+id+`"}`, token, passkeyTestOrigin, "csrf", "csrf")
		if response.Code != 204 {
			t.Fatalf("unsubscribe=%d %s", response.Code, response.Body.String())
		}
	}
	response := webUICommandRequestForTest(server, "POST", "/tgw/api/v1/webui/push/subscribe", string(raw), token, passkeyTestOrigin, "csrf", "csrf")
	var result struct {
		ID string `json:"subscription_id"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.ID == "" {
		t.Fatal("resubscribe failed")
	}
	response = webUICommandRequestForTest(server, "POST", "/tgw/api/v1/admin/logout", "{}", token, passkeyTestOrigin, "csrf", "csrf")
	if response.Code != 204 {
		t.Fatal("logout failed")
	}
	fresh, err := store.CreateAdminSession(t.Context(), []byte("webui-test-passkey"))
	if err != nil {
		t.Fatal(err)
	}
	config = webUICommandRequestForTest(server, "GET", "/tgw/api/v1/webui/push/config?subscription_id="+result.ID, "", fresh, "", "", "")
	if config.Code != 200 || !strings.Contains(config.Body.String(), `"subscribed":false`) {
		t.Fatalf("logout retained device: %d %s", config.Code, config.Body.String())
	}
}

type webPushRecorder struct{ sent chan uuid.UUID }

func (r *webPushRecorder) Send(ctx context.Context, _ webpush.Subscription, event, session uuid.UUID, _ time.Time) string {
	select {
	case r.sent <- event:
	case <-ctx.Done():
	}
	return "delivered"
}

func TestWebUIPushDispatcherDeliversDurableCompletionAndStops(t *testing.T) {
	store := adminIntegrationStore(t)
	token := webuiTestLogin(t, store)
	server, err := New(store, Config{Origin: passkeyTestOrigin})
	if err != nil {
		t.Fatal(err)
	}
	sub := webPushTestSubscription(t)
	if _, err = store.UpsertWebPushSubscription(t.Context(), token, registry.WebPushSubscription{Endpoint: sub.Endpoint, P256DH: sub.Keys.P256DH, Auth: sub.Keys.Auth}); err != nil {
		t.Fatal(err)
	}
	worker, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{Name: "Worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime, session := uuid.New(), uuid.New(), uuid.New()
	if err = store.BindConnection(t.Context(), worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordHeartbeat(t.Context(), registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtime, WorkerID: worker.ID, Name: "Runtime", ProfileID: "main", State: "running", Generation: 1}}}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(protocol.Session{ID: session.String(), WorkerID: worker.ID.String(), RuntimeID: runtime.String(), ThreadID: "thread", CWD: "/work", State: "idle", UpdatedAt: time.Now().UTC()})
	event := protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: worker.ID.String(), RuntimeID: runtime.String(), RuntimeGeneration: 1, SessionID: session.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: data}
	if err = store.IngestEvent(t.Context(), worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	recorder := &webPushRecorder{sent: make(chan uuid.UUID, 4)}
	done := make(chan struct{})
	go func() { defer close(done); server.runWebPush(ctx, nil, recorder) }()
	data, _ = json.Marshal(protocol.Result{TurnID: "turn-completed", Text: "Private conversation never enters push"})
	event.ID, event.Seq, event.Kind, event.Data, event.OccurredAt = uuid.NewString(), 2, "turn_completed", data, time.Now().UTC()
	if err = store.IngestEvent(t.Context(), worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-recorder.sent:
		if got.String() != event.ID {
			t.Fatal("wrong notification")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completion did not wake dispatcher")
	}
	// Durable event replay cannot generate another delivery.
	if err = store.IngestEvent(t.Context(), worker.ID, connection, event); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher ignored cancellation")
	}
	select {
	case <-recorder.sent:
		t.Fatal("duplicate notification")
	default:
	}
}
