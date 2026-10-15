package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestTelegramClientDefaultNameMethods(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		requests = append(requests, input)
		if strings.HasSuffix(r.URL.Path, "/getMyName") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"name":"Default 😸"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	client := NewTelegramClient(botTestToken(111))
	client.endpoint, client.http = server.URL, server.Client()
	if got, err := client.GetMyName(t.Context()); err != nil || got != "Default 😸" {
		t.Fatalf("default name = %q %v", got, err)
	}
	for _, name := range []string{"", strings.Repeat("😸", 64)} {
		if err := client.SetMyName(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{strings.Repeat("a", 65), strings.Repeat("😸", 65), "bad\xff"} {
		if err := client.SetMyName(t.Context(), name); err == nil {
			t.Fatal("invalid name accepted")
		}
	}
	if len(requests) != 3 || len(requests[0]) != 0 || requests[1]["name"] != "" {
		t.Fatalf("default API payloads = %#v", requests)
	}
	for _, input := range requests {
		if _, exists := input["language_code"]; exists {
			t.Fatal("default-name call modified a language override")
		}
	}
}

func TestTelegramClientNameErrorsKeepRetryAndHideToken(t *testing.T) {
	token := botTestToken(111)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 429, "description": token, "parameters": map[string]int{"retry_after": 23}})
	}))
	defer server.Close()
	client := NewTelegramClient(token)
	client.endpoint, client.http = server.URL, server.Client()
	err := client.SetMyName(t.Context(), "Name")
	var apiError *TelegramError
	if !errors.As(err, &apiError) || apiError.RetryAfter != 23*time.Second || strings.Contains(err.Error(), token) {
		t.Fatalf("unsafe or incomplete API error: %v", err)
	}
}

func TestTelegramClientGetMyNameRejectsInvalidResults(t *testing.T) {
	for _, result := range []string{`{}`, `null`, `{"name":null}`, `{"name":1}`, `{"name":"` + strings.Repeat("a", 65) + `"}`} {
		t.Run(result[:min(len(result), 24)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"ok":true,"result":`+result+`}`)
			}))
			defer server.Close()
			client := NewTelegramClient(botTestToken(111))
			client.endpoint, client.http = server.URL, server.Client()
			if _, err := client.GetMyName(t.Context()); err == nil {
				t.Fatal("invalid result became the empty original")
			}
		})
	}
}

type botNameSessionFixture struct {
	store                       *registry.Store
	worker, connection, runtime uuid.UUID
	seq                         uint64
	update                      int64
}

func newBotNameSessionFixture(t *testing.T, store *registry.Store) *botNameSessionFixture {
	t.Helper()
	worker, err := store.CreateWorker(t.Context(), registry.CreateWorkerInput{Name: "names", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("name-test")})
	if err != nil {
		t.Fatal(err)
	}
	f := &botNameSessionFixture{store: store, worker: worker.ID, connection: uuid.New(), runtime: uuid.New()}
	if err := store.BindConnection(t.Context(), f.worker, f.connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(t.Context(), registry.Heartbeat{WorkerID: f.worker, ConnectionID: f.connection, Runtimes: []registry.Runtime{{ID: f.runtime, WorkerID: f.worker, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *botNameSessionFixture) session(t *testing.T, id uuid.UUID, name string, archived bool) {
	t.Helper()
	f.seq++
	data, _ := json.Marshal(protocol.Session{ID: id.String(), WorkerID: f.worker.String(), RuntimeID: f.runtime.String(), ThreadID: id.String(), Name: name, State: "idle", Archived: archived, UpdatedAt: time.Now().UTC()})
	if err := f.store.IngestEvent(t.Context(), f.worker, f.connection, protocol.Event{ID: uuid.NewString(), Seq: f.seq, WorkerID: f.worker.String(), RuntimeID: f.runtime.String(), RuntimeGeneration: 1, SessionID: id.String(), Kind: "session_discovered", OccurredAt: time.Now().UTC(), Data: data}); err != nil {
		t.Fatal(err)
	}
}

func (f *botNameSessionFixture) selectSession(t *testing.T, bot string, user, chat, topic int64, id uuid.UUID) {
	t.Helper()
	f.update++
	result, err := f.store.AcceptTelegram(t.Context(), registry.IncomingUpdate{BotID: bot, UserID: user, ChatID: chat, TopicID: topic, UpdateID: f.update, Action: "select", Target: id.String()})
	if err != nil || result.ErrorCode != "" {
		t.Fatalf("selection = %+v %v", result, err)
	}
}

func (f *managedBotFake) name(token string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.defaultNames[token]
}

func readyBotNames(t *testing.T) (*TelegramBotManager, *registry.Store, *managedBotFake, *botNameSessionFixture, string) {
	t.Helper()
	m, store, fake := newTelegramBotManagerTest(t)
	id := m.cfg.Secrets.BotName
	fake.mu.Lock()
	fake.webhooks[m.cfg.Secrets.BotToken] = m.webhookURL(m.get(id).record)
	// getMe's localized/cached name must never become the default baseline.
	fake.defaultNames[m.cfg.Secrets.BotToken] = "Original default"
	fake.mu.Unlock()
	return m, store, fake, newBotNameSessionFixture(t, store), id
}

func TestTelegramBotNamesOptInTracksPrivateSessionRenameAndDisconnect(t *testing.T) {
	m, store, fake, f, id := readyBotNames(t)
	initialUsername := m.get(id).record.Username
	session, group := uuid.New(), uuid.New()
	f.session(t, session, "Project One", false)
	f.session(t, group, "Group secret", false)
	f.selectSession(t, id, 7, 100, 0, group)
	m.syncBotNames(t.Context())
	if fake.count("getMyName") != 0 || fake.count("setMyName") != 0 {
		t.Fatal("default-off feature made profile calls")
	}
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	if fake.count("getMyName") != 0 || fake.count("setMyName") != 0 {
		t.Fatal("setting flag synchronously called Telegram")
	}
	m.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default" {
		t.Fatalf("group selection renamed bot: %q", got)
	}
	f.selectSession(t, id, 7, 7, 0, session)
	m.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default · Project One" {
		t.Fatalf("private selection = %q", got)
	}
	before := fake.count("setMyName")
	m.syncBotNames(t.Context())
	if fake.count("setMyName") != before {
		t.Fatal("unchanged selection retried a successful rename")
	}
	f.session(t, session, "Project renamed", false)
	m.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default · Project renamed" {
		t.Fatalf("session rename = %q", got)
	}
	f.update++
	if result, err := store.AcceptTelegram(t.Context(), registry.IncomingUpdate{BotID: id, UpdateID: f.update, UserID: 7, ChatID: 7, Action: "disconnect"}); err != nil || result.ErrorCode != "" {
		t.Fatalf("disconnect = %+v %v", result, err)
	}
	m.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default" {
		t.Fatalf("disconnect did not restore baseline: %q", got)
	}
	bot := m.summary(m.get(id))
	if !bot.InheritSessionName || bot.NameSyncPending || bot.NameError != "" || bot.Username != initialUsername || bot.ID != id || bot.OriginalDisplayName != "Original default" || bot.DisplayName != "Original default" {
		t.Fatalf("name changed durable identity or summary: %+v", bot)
	}
}

func TestTelegramBotNamesDoNotMutateDisabledBotsAndResumeAfterEnable(t *testing.T) {
	m, _, fake, f, id := readyBotNames(t)
	session := uuid.New()
	f.session(t, session, "Selected", false)
	f.selectSession(t, id, 7, 7, 0, session)
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	m.syncBotNames(t.Context())
	if err := m.SetTelegramBotEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	before := fake.count("setMyName")
	m.syncBotNames(t.Context())
	if fake.count("setMyName") != before || !m.summary(m.get(id)).NameSyncPending {
		t.Fatal("disabled bot mutated its profile or lost pending restore")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	m.syncBotNames(t.Context())
	if fake.name(m.cfg.Secrets.BotToken) != "Original default" || m.summary(m.get(id)).NameSyncPending {
		t.Fatal("reenabled bot did not complete deferred restore")
	}
}

func TestTelegramBotNamesPersistFlagAndOriginalAcrossRestartAndProfileRefresh(t *testing.T) {
	m, store, fake, f, id := readyBotNames(t)
	session := uuid.New()
	f.session(t, session, "One", false)
	f.selectSession(t, id, 7, 7, 0, session)
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	m.syncBotNames(t.Context())
	if _, err := m.RefreshTelegramBots(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.summary(m.get(id)); got.DisplayName != "Original default · One" || !got.InheritSessionName {
		t.Fatalf("profile refresh replaced name settings: %+v", got)
	}
	restarted, err := NewTelegramBotManager(t.Context(), store, m.cfg, "original-secret", m.log)
	if err != nil {
		t.Fatal(err)
	}
	fake.attach(restarted)
	before := fake.count("getMyName")
	f.session(t, session, "Two", false)
	restarted.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default · Two" || fake.count("getMyName") != before {
		t.Fatalf("restart lost original or recaptured suffix: %q", got)
	}
	if err := restarted.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	restarted.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default" {
		t.Fatalf("opt-out = %q", got)
	}
}

func TestTelegramBotNamesRetryUnknownOutcomeAcrossRestartAndOptOut(t *testing.T) {
	m, store, fake, f, id := readyBotNames(t)
	session := uuid.New()
	f.session(t, session, "Selected", false)
	f.selectSession(t, id, 7, 7, 0, session)
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.nameApplyThenFail = true
	fake.mu.Unlock()
	m.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default · Selected" {
		t.Fatalf("fixture did not apply lost-response name: %q", got)
	}
	prefs, err := store.TelegramBotNamePreferences(t.Context())
	if err != nil || len(prefs) != 1 || prefs[0].AppliedDisplayName != nil || prefs[0].OriginalDisplayName == nil || *prefs[0].OriginalDisplayName != "Original default" {
		t.Fatalf("uncertain update not durably marked: %+v %v", prefs, err)
	}
	// The flag-off itself must remain safe even before the process restarts.
	if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if !m.summary(m.get(id)).NameSyncPending {
		t.Fatal("opt-out discarded an unknown remote outcome")
	}
	restarted, err := NewTelegramBotManager(t.Context(), store, m.cfg, "original-secret", m.log)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.nameApplyThenFail = false
	fake.mu.Unlock()
	fake.attach(restarted)
	restarted.syncBotNames(t.Context())
	if got := fake.name(m.cfg.Secrets.BotToken); got != "Original default" {
		t.Fatalf("restart/opt-out did not restore uncertain rename: %q", got)
	}
}

func TestTelegramBotNamesBackoffAndMovedWebhook(t *testing.T) {
	m, _, fake, f, id := readyBotNames(t)
	session := uuid.New()
	f.session(t, session, "Selected", false)
	f.selectSession(t, id, 7, 7, 0, session)
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.nameErrorCode, fake.nameRetryAfter = 429, 60
	fake.mu.Unlock()
	m.syncBotNames(t.Context())
	state := m.botNameState(id)
	if state.lastError != "name_update_unavailable" || !state.pending || time.Until(state.retryAt) < 59*time.Second {
		t.Fatalf("retry state = %+v", state)
	}
	before := fake.count("setMyName")
	m.syncBotNames(t.Context())
	if fake.count("setMyName") != before {
		t.Fatal("worker ignored Telegram RetryAfter")
	}
	fake.mu.Lock()
	fake.nameErrorCode = 0
	fake.webhooks[m.cfg.Secrets.BotToken] = "https://elsewhere.example/webhook"
	fake.mu.Unlock()
	m.op.Lock()
	m.syncBotName(t.Context(), id, state.retryAt.Add(time.Second))
	m.op.Unlock()
	if fake.count("setMyName") != before || m.summary(m.get(id)).NameError != "name_webhook_moved" {
		t.Fatal("worker changed a profile after the webhook moved")
	}
	fake.mu.Lock()
	fake.webhooks[m.cfg.Secrets.BotToken] = m.webhookURL(m.get(id).record)
	fake.mu.Unlock()
	m.op.Lock()
	m.syncBotName(t.Context(), id, m.botNameState(id).retryAt.Add(time.Second))
	m.op.Unlock()
	if fake.count("setMyName") != before+1 || m.summary(m.get(id)).NameError != "" {
		t.Fatal("retry failed to recover automatically")
	}
}

func TestTelegramBotNamesBlankOriginalUnicodeAndRedaction(t *testing.T) {
	redactor, err := auth.NewRedactor([]string{`secret-[a-z]+`}, "[REDACTED]")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ original, session, want string }{
		{"", "Selected", "Selected"},
		{"Original", "  One\n\x00 Two  ", "Original · One Two"},
		{" Original ", "", " Original "},
		{"Original", "secret-abcdef", "Original · [REDACTED]"},
	} {
		if got := telegramBotSessionDisplayName(test.original, test.session, redactor); got != test.want {
			t.Fatalf("display name %q/%q = %q", test.original, test.session, got)
		}
	}
	got := telegramBotSessionDisplayName(strings.Repeat("😸", 64), strings.Repeat("界", 200), redactor)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 64 || !strings.Contains(got, " · ") || !strings.Contains(got, "界") {
		t.Fatalf("long Unicode name = %q", got)
	}
	m, store, fake, f, id := readyBotNames(t)
	fake.mu.Lock()
	fake.defaultNames[m.cfg.Secrets.BotToken] = ""
	fake.mu.Unlock()
	session := uuid.New()
	f.session(t, session, "Selected", false)
	f.selectSession(t, id, 7, 7, 0, session)
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	m.syncBotNames(t.Context())
	if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	m.syncBotNames(t.Context())
	prefs, err := store.TelegramBotNamePreferences(t.Context())
	if err != nil || len(prefs) != 1 || prefs[0].OriginalDisplayName == nil || *prefs[0].OriginalDisplayName != "" || fake.name(m.cfg.Secrets.BotToken) != "" {
		t.Fatalf("empty default original lost: %+v %v", prefs, err)
	}
}

func TestTelegramBotNamesCancellationAndMissingBot(t *testing.T) {
	m, _, fake, _, _ := readyBotNames(t)
	if err := m.SetTelegramBotSessionName(t.Context(), "missing", true); !errors.Is(err, ErrTelegramBotNotFound) {
		t.Fatalf("missing bot = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() { m.RunBotNames(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("name worker did not cancel")
	}
	if fake.count("getMyName") != 0 || fake.count("setMyName") != 0 {
		t.Fatal("cancelled worker made profile calls")
	}
}

type botNameRoundTripFunc func(*http.Request) (*http.Response, error)

func (f botNameRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramBotNamesInFlightCancellationKeepsAcceptanceAndConcurrentDisableSafe(t *testing.T) {
	m, store, fake, f, id := readyBotNames(t)
	session := uuid.New()
	f.session(t, session, "Selected", false)
	f.selectSession(t, id, 7, 7, 0, session)
	if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	transport := fake.server.Client().Transport
	client := m.get(id).client
	client.http = &http.Client{Transport: botNameRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err != nil || !strings.HasSuffix(r.URL.Path, "/setMyName") {
			return response, err
		}
		// Telegram has applied the name; its successful response is lost when
		// shutdown cancels the caller before the client can read/checkpoint it.
		close(accepted)
		<-r.Context().Done()
		_ = response.Body.Close()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { m.syncBotNames(ctx); close(done) }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("name request never reached Telegram")
	}
	acceptCtx, acceptCancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer acceptCancel()
	f.update++
	if result, err := store.AcceptTelegram(acceptCtx, registry.IncomingUpdate{BotID: id, UpdateID: f.update, UserID: 7, ChatID: 7, Action: "disconnect"}); err != nil || result.ErrorCode != "" {
		t.Fatalf("in-flight profile request blocked normal acceptance: %+v %v", result, err)
	}
	disabled := make(chan error, 1)
	go func() { disabled <- m.SetTelegramBotEnabled(t.Context(), id, false) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight cancellation did not stop the update")
	}
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent disable did not finish")
	}
	if m.get(id).record.Enabled || m.botNameState(id).preference.AppliedDisplayName != nil {
		t.Fatal("stale name response overwrote disable or claimed a known API outcome")
	}
	// Re-enable with an ordinary client and restore the uncertain name.
	client.http = fake.server.Client()
	if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTelegramBotEnabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	m.syncBotNames(t.Context())
	if fake.name(m.cfg.Secrets.BotToken) != "Original default" {
		t.Fatal("cancelled remote outcome could not be restored")
	}
}
