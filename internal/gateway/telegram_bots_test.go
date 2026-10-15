package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

type managedBotFake struct {
	mu            sync.Mutex
	server        *httptest.Server
	identities    map[string]TelegramUser
	webhooks      map[string]string
	managedTokens map[int64]string
	calls         map[string]int
	secrets       map[string]string
	drops         map[string]bool
	failSet       bool
}

func newManagedBotFake(t *testing.T) *managedBotFake {
	t.Helper()
	f := &managedBotFake{identities: map[string]TelegramUser{}, webhooks: map[string]string{}, managedTokens: map[int64]string{}, calls: map[string]int{}, secrets: map[string]string{}, drops: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		token, method, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.calls[method]++
		var input struct {
			URL     string   `json:"url"`
			Secret  string   `json:"secret_token"`
			UserID  int64    `json:"user_id"`
			Allowed []string `json:"allowed_updates"`
			Drop    bool     `json:"drop_pending_updates"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		var result any = true
		switch method {
		case "getMe":
			identity, exists := f.identities[token]
			if !exists {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized " + token})
				return
			}
			result = identity
		case "getWebhookInfo":
			result = WebhookInfo{URL: f.webhooks[token]}
		case "setWebhook":
			if f.failSet {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 500, "description": "failure " + token + " " + input.Secret})
				return
			}
			f.webhooks[token] = input.URL
			f.secrets[token] = input.Secret
			f.drops[token] = input.Drop
		case "deleteWebhook":
			delete(f.webhooks, token)
		case "getManagedBotToken":
			result = f.managedTokens[input.UserID]
		default:
			http.Error(w, "unexpected method", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(f.server.Close)
	return f
}
func botTestToken(id int64) string { return strconv.FormatInt(id, 10) + ":" + strings.Repeat("x", 30) }
func (f *managedBotFake) add(id int64, username string, manager bool) string {
	token := botTestToken(id)
	f.mu.Lock()
	f.identities[token] = TelegramUser{ID: id, Username: username, IsBot: true, FirstName: "Test bot", CanManageBots: manager}
	f.managedTokens[id] = token
	f.mu.Unlock()
	return token
}
func (f *managedBotFake) client(token string) *TelegramClient {
	client := NewTelegramClient(token)
	client.endpoint = f.server.URL
	client.http = f.server.Client()
	return client
}
func (f *managedBotFake) attach(m *TelegramBotManager) {
	m.newClient = f.client
	for _, bot := range m.bots {
		bot.client = f.client(bot.cfg.Secrets.BotToken)
	}
}
func (f *managedBotFake) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func newTelegramBotManagerTest(t *testing.T) (*TelegramBotManager, *registry.Store, *managedBotFake) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.db")
	store, err := registry.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err = store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	fake := newManagedBotFake(t)
	token := fake.add(111, "PrimaryBot", true)
	cfg := config.GatewayConfig{DatabasePath: path, PublicBaseURL: "https://gateway.example.com", AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{7, 100}, CommandExpiry: time.Hour, Secrets: config.BotSecrets{BotName: "original-routing-id", BotToken: token, WLID: 7}}
	m, err := NewTelegramBotManager(t.Context(), store, cfg, "original-secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fake.attach(m)
	return m, store, fake
}
func managedUpdate(id, owner, botID int64, username string) TelegramUpdate {
	return TelegramUpdate{ID: id, ManagedBot: &TelegramManagedBotUpdated{User: TelegramUser{ID: owner}, Bot: TelegramUser{ID: botID, Username: username, IsBot: true}}}
}
func postManaged(t *testing.T, m *TelegramBotManager, id string, update TelegramUpdate, secret string) int {
	t.Helper()
	body, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	path := legacyTelegramWebhookPath
	if id != m.cfg.Secrets.BotName {
		path = telegramBotsWebhookPrefix + id + "/webhook"
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec.Code
}

func TestTelegramBotConnectPersistsEncryptedCredentialsAndRouting(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	token := fake.add(222, "SecondaryBot", false)
	summary, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ID != "telegram:222" || summary.Username != "SecondaryBot" || !summary.Enabled || summary.Legacy {
		t.Fatalf("unexpected bot summary: %+v", summary)
	}
	bot := m.get(summary.ID)
	if bot.secret == "original-secret" || len(bot.secret) < 32 {
		t.Fatal("new bot did not get an independent random secret")
	}
	rows, err := store.TelegramBotRecords(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if bytes.Contains(row.EncryptedCredentials, []byte(token)) || bytes.Contains(row.EncryptedCredentials, []byte(bot.secret)) {
			t.Fatal("plaintext credential reached the database")
		}
	}
	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte(bot.secret)) {
		t.Fatal("summary exposed credentials")
	}
	key, err := os.Stat(m.cfg.DatabasePath + ".telegram-bots.key")
	if err != nil || key.Mode().Perm() != 0600 {
		t.Fatalf("key is not private: %v", err)
	}
	reopened, err := registry.Open(t.Context(), m.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := NewTelegramBotManager(t.Context(), reopened, m.cfg, "original-secret", m.log)
	if err != nil {
		t.Fatal(err)
	}
	fake.attach(restarted)
	cfg, client, ok := restarted.ResolveTelegramBot(summary.ID)
	if !ok || client.token != token || cfg.Secrets.BotName != summary.ID || cfg.Secrets.BotUsername != "SecondaryBot" || cfg.Secrets.WLID != 7 || len(cfg.AllowedChatIDs) != 2 {
		t.Fatal("restart lost bot identity, credentials or owner policy")
	}
	if _, _, ok := restarted.ResolveTelegramBot(m.cfg.Secrets.BotName); !ok {
		t.Fatal("legacy routing ID was changed")
	}
	if _, _, ok := restarted.ResolveTelegramBot("not-present"); ok {
		t.Fatal("unknown identity fell back to the primary bot")
	}
}

func TestTelegramBotKeyLossAndPermissionsFailClosed(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	token := fake.add(222, "SecondaryBot", false)
	if _, err := m.ConnectTelegramBot(t.Context(), token, false); err != nil {
		t.Fatal(err)
	}
	path := m.cfg.DatabasePath + ".telegram-bots.key"
	key, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTelegramBotManager(t.Context(), m.store, m.cfg, "original-secret", m.log); err == nil {
		t.Fatal("missing credential key was regenerated")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing key was replaced")
	}
	if err := os.WriteFile(path, key, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTelegramBotManager(t.Context(), m.store, m.cfg, "original-secret", m.log); err == nil {
		t.Fatal("public key permissions were accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	key[0] ^= 0xff
	if err := os.WriteFile(path, key, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTelegramBotManager(t.Context(), m.store, m.cfg, "original-secret", m.log); err == nil {
		t.Fatal("wrong key did not fail authentication")
	}
}

func TestTelegramBotDuplicateAndWebhookTakeoverRequireExplicitAction(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	if _, err := m.ConnectTelegramBot(t.Context(), m.cfg.Secrets.BotToken, false); !errors.Is(err, ErrTelegramBotDuplicate) {
		t.Fatalf("primary was duplicated: %v", err)
	}
	token := fake.add(222, "SecondaryBot", false)
	fake.webhooks[token] = "https://elsewhere.example/webhook"
	if _, err := m.ConnectTelegramBot(t.Context(), token, false); !errors.Is(err, ErrTelegramBotWebhookConflict) {
		t.Fatalf("existing webhook silently replaced: %v", err)
	}
	if fake.count("setWebhook") != 0 {
		t.Fatal("conflict mutated Telegram")
	}
	summary, err := m.ConnectTelegramBot(t.Context(), token, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConnectTelegramBot(t.Context(), token, true); !errors.Is(err, ErrTelegramBotDuplicate) {
		t.Fatal("duplicate Telegram identity accepted")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), summary.ID, false); err != nil {
		t.Fatal(err)
	}
	fake.webhooks[token] = "https://elsewhere.example/webhook"
	if err := m.SetTelegramBotEnabled(t.Context(), summary.ID, true); !errors.Is(err, ErrTelegramBotWebhookConflict) {
		t.Fatal("enable overwrote a changed webhook")
	}
	if _, err := m.ReplaceTelegramBotToken(t.Context(), summary.ID, token, true); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramManagedBotRequiresOwnerIntentAndDeduplicates(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	fake.add(222, "CreatedBot", false)
	primary := m.cfg.Secrets.BotName
	// Unsolicited updates are acknowledged and permanently remembered, including
	// when the owner later creates an intent with the same requested username.
	if code := postManaged(t, m, primary, managedUpdate(10, 7, 222, "CreatedBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if fake.count("getManagedBotToken") != 0 {
		t.Fatal("unsolicited managed event fetched credentials")
	}
	intent, err := m.CreateManagedTelegramBot(t.Context(), primary, "CreatedBot", "Created name")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := url.Parse(intent.LaunchURL)
	if err != nil || launch.Scheme != "https" || launch.Host != "t.me" || launch.Path != "/newbot/PrimaryBot/CreatedBot" || launch.Query().Get("name") != "Created name" {
		t.Fatal("incorrect Telegram creation link")
	}
	for _, update := range []TelegramUpdate{managedUpdate(10, 7, 222, "CreatedBot"), managedUpdate(11, 9, 222, "CreatedBot"), managedUpdate(12, 7, 222, "OtherBot")} {
		if code := postManaged(t, m, primary, update, "original-secret"); code != 200 {
			t.Fatal(code)
		}
	}
	if fake.count("getManagedBotToken") != 0 {
		t.Fatal("replayed, foreign owner or mismatched username consumed an intent")
	}
	if code := postManaged(t, m, primary, managedUpdate(13, 7, 222, "CreatedBot"), "wrong-secret"); code != 401 {
		t.Fatal("managed update bypassed webhook authentication")
	}
	if code := postManaged(t, m, primary, managedUpdate(13, 7, 222, "CreatedBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	bot := m.get("telegram:222")
	if bot == nil || !bot.record.Enabled || bot.record.ManagedBy != primary {
		t.Fatal("authorized managed creation not connected")
	}
	if code := postManaged(t, m, primary, managedUpdate(13, 7, 222, "CreatedBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if fake.count("getManagedBotToken") != 1 {
		t.Fatal("managed event was not deduplicated")
	}
	intents, err := m.ListTelegramManagedBotIntents(t.Context())
	if err != nil || len(intents) != 1 || intents[0].Status != "connected" || intents[0].BotID != "telegram:222" {
		t.Fatalf("intent not durably consumed: %+v %v", intents, err)
	}
	if code := postManaged(t, m, primary, managedUpdate(14, 99, 222, "CreatedBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if _, _, ok := m.ResolveTelegramBot("telegram:222"); ok {
		t.Fatal("owner transfer retained bot access")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), "telegram:222", true); !errors.Is(err, ErrTelegramBotOwnerChanged) {
		t.Fatal("admin enabled a transferred managed bot")
	}
	if _, err := m.ReplaceTelegramBotToken(t.Context(), "telegram:222", bot.client.token, true); !errors.Is(err, ErrTelegramBotOwnerChanged) {
		t.Fatal("admin token replacement bypassed managed owner verification")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), "telegram:222", false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTelegramBotEnabled(t.Context(), "telegram:222", true); !errors.Is(err, ErrTelegramBotOwnerChanged) {
		t.Fatal("disabling a transferred bot erased its ownership restriction")
	}
	restarted, err := NewTelegramBotManager(t.Context(), m.store, m.cfg, "original-secret", m.log)
	if err != nil {
		t.Fatal(err)
	}
	fake.attach(restarted)
	if err := restarted.SetTelegramBotEnabled(t.Context(), "telegram:222", true); !errors.Is(err, ErrTelegramBotOwnerChanged) {
		t.Fatal("restart erased managed ownership restriction")
	}
	if code := postManaged(t, restarted, primary, managedUpdate(15, 7, 222, "CreatedBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if _, _, ok := restarted.ResolveTelegramBot("telegram:222"); ok {
		t.Fatal("ownership verification silently enabled a disabled bot")
	}
	if err := restarted.SetTelegramBotEnabled(t.Context(), "telegram:222", true); err != nil {
		t.Fatalf("fresh owner verification did not unlock explicit enable: %v", err)
	}
}

func TestTelegramManagedBotExpiredIntentCannotEnroll(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	fake.add(222, "ExpiredBot", false)
	now := time.Now().UTC()
	in := registry.TelegramManagedBotIntentRecord{ID: "expired-intent", ManagerID: m.cfg.Secrets.BotName, OwnerID: 7, RequestedUsername: "ExpiredBot", DisplayName: "Expired", LaunchURL: "https://t.me/newbot/PrimaryBot/ExpiredBot", Status: "pending", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if err := store.SaveTelegramManagedBotIntent(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(20, 7, 222, "ExpiredBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if fake.count("getManagedBotToken") != 0 || m.get("telegram:222") != nil {
		t.Fatal("expired intent enrolled a bot")
	}
	intents, err := m.ListTelegramManagedBotIntents(t.Context())
	if err != nil || len(intents) != 1 || intents[0].Status != "expired" {
		t.Fatal("expired request status not reconciled")
	}
}

func TestTelegramManagedBotActivationRetriesAfterFailureAndRestart(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	fake.add(222, "RetryBot", false)
	if _, err := m.CreateManagedTelegramBot(t.Context(), "", "RetryBot", "Retry"); err != nil {
		t.Fatal(err)
	}
	fake.failSet = true
	update := managedUpdate(30, 7, 222, "RetryBot")
	if code := postManaged(t, m, m.cfg.Secrets.BotName, update, "original-secret"); code != 503 {
		t.Fatal(code)
	}
	bot := m.get("telegram:222")
	if bot == nil || bot.record.Enabled || bot.record.LastError != "webhook_setup_failed" {
		t.Fatal("failed activation was not recoverable and disabled")
	}
	fake.failSet = false
	restarted, err := NewTelegramBotManager(t.Context(), store, m.cfg, "original-secret", m.log)
	if err != nil {
		t.Fatal(err)
	}
	fake.attach(restarted)
	if code := postManaged(t, restarted, m.cfg.Secrets.BotName, update, "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if _, _, ok := restarted.ResolveTelegramBot("telegram:222"); !ok {
		t.Fatal("redelivery did not recover interrupted activation")
	}
}

func TestTelegramManagedBotTokenRefreshKeepsIdentityAndDisable(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	fake.add(222, "RefreshBot", false)
	if _, err := m.CreateManagedTelegramBot(t.Context(), "", "RefreshBot", "Refresh"); err != nil {
		t.Fatal(err)
	}
	if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(40, 7, 222, "RefreshBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	old := m.get("telegram:222")
	newToken := "222:" + strings.Repeat("y", 30)
	fake.mu.Lock()
	fake.identities[newToken] = fake.identities[old.client.token]
	fake.managedTokens[222] = newToken
	fake.mu.Unlock()
	if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(41, 7, 222, "RefreshBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	current := m.get("telegram:222")
	if current.client.token != newToken || current.secret != old.secret || !current.record.Enabled {
		t.Fatal("token refresh changed identity or webhook secret")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), current.record.ID, false); err != nil {
		t.Fatal(err)
	}
	if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(42, 7, 222, "RefreshBot"), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if _, _, ok := m.ResolveTelegramBot(current.record.ID); ok {
		t.Fatal("token refresh silently enabled a disabled bot")
	}
}

func TestTelegramBotWebhookIsolationAndVerifiedUsername(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	token := fake.add(222, "SecondaryBot", false)
	summary, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err != nil {
		t.Fatal(err)
	}
	secret := m.get(summary.ID).secret
	post := func(id int64, actor, chat int64, text, header, path string) int {
		body, _ := json.Marshal(TelegramUpdate{ID: id, Message: &TelegramMessage{From: &TelegramUser{ID: actor}, Chat: TelegramChat{ID: chat}, Text: text}})
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, req)
		return rec.Code
	}
	path := telegramBotsWebhookPrefix + summary.ID + "/webhook"
	for _, tc := range []struct {
		actor, chat int64
		header      string
		want        int
	}{{7, 7, "original-secret", 401}, {8, 7, secret, 403}, {7, 999, secret, 403}} {
		if code := post(50, tc.actor, tc.chat, "/start@SecondaryBot", tc.header, path); code != tc.want {
			t.Fatalf("webhook authorization: got %d want %d", code, tc.want)
		}
	}
	if code := post(51, 7, 7, "/start@SecondaryBot", secret, path); code != 200 {
		t.Fatal(code)
	}
	// The same update_id can be used independently by the primary bot.
	if code := post(51, 7, 7, "/start", "original-secret", legacyTelegramWebhookPath); code != 200 {
		t.Fatal(code)
	}
	rows, err := store.TelegramBotRecords(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatal("cross-bot update routing changed registry metadata")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), summary.ID, false); err != nil {
		t.Fatal(err)
	}
	if code := post(52, 7, 7, "/start", secret, path); code != 404 {
		t.Fatal("disabled webhook accepted an update")
	}
	if err := m.RemoveTelegramBot(t.Context(), m.cfg.Secrets.BotName); !errors.Is(err, ErrTelegramBotLegacy) {
		t.Fatal("primary configuration was removed")
	}
}

func TestTelegramBotErrorsNeverExposeCredentials(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	unknown := "333:" + strings.Repeat("z", 30)
	_, err := m.ConnectTelegramBot(t.Context(), unknown, false)
	if err == nil || strings.Contains(err.Error(), unknown) {
		t.Fatal("identity error exposed token")
	}
	token := fake.add(222, "FailureBot", false)
	fake.failSet = true
	summary, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatal("webhook error exposed token")
	}
	body, _ := json.Marshal(summary)
	if strings.Contains(string(body), token) || strings.Contains(string(body), m.get(summary.ID).secret) {
		t.Fatal("failed summary exposed credentials")
	}
}

func TestTelegramBotListIdentityProbesAreCached(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	for range 2 {
		bots, err := m.ListTelegramBots(context.Background())
		if err != nil || len(bots) != 1 || !bots[0].CanManageBots || bots[0].Username != "PrimaryBot" {
			t.Fatalf("identity metadata missing: %+v %v", bots, err)
		}
	}
	if fake.count("getMe") != 1 {
		t.Fatal("list polled Telegram repeatedly")
	}
}

func TestTelegramBotEnableAndReconnectDiscardDisabledWindowUpdates(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	token := fake.add(222, "SecondaryBot", false)
	summary, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	droppedOnConnect := fake.drops[token]
	fake.mu.Unlock()
	if droppedOnConnect {
		t.Fatal("first connect unexpectedly discarded pending updates")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), summary.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTelegramBotEnabled(t.Context(), summary.ID, true); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	droppedOnEnable := fake.drops[token]
	fake.mu.Unlock()
	if !droppedOnEnable {
		t.Fatal("enable replays messages queued while disabled")
	}
	if err := m.RemoveTelegramBot(t.Context(), summary.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConnectTelegramBot(t.Context(), token, false); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	droppedOnReconnect := fake.drops[token]
	fake.mu.Unlock()
	if !droppedOnReconnect {
		t.Fatal("reconnect replays messages queued while removed")
	}
}

func TestTelegramPrimaryIdentityCannotInheritDifferentBotsBindings(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	cfg := m.cfg
	cfg.Secrets.BotToken = fake.add(222, "DifferentBot", false)
	if _, err := NewTelegramBotManager(t.Context(), store, cfg, "original-secret", m.log); err == nil {
		t.Fatal("different primary identity inherited the original routing ID")
	}
	rows, err := store.TelegramBotRecords(t.Context())
	if err != nil || len(rows) != 1 || rows[0].TelegramID != 111 {
		t.Fatal("rejected primary identity change mutated registry")
	}
}
