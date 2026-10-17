package gateway

import (
	"bytes"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/registry"
)

const (
	enkiCapturedName = "EnkiCodex · Fix Telegram gateway commands"
	enkiAppliedName  = "EnkiCodex · Fix Telegram gatewa… · Assessment_sic_medico"
)

func historicalBotNames(t *testing.T, original, applied string, inherit bool, manager string) (*TelegramBotManager, *registry.Store, *managedBotFake, *botNameSessionFixture, string, string) {
	t.Helper()
	m, store, fake := newTelegramBotManagerTest(t)
	token := fake.add(8980848680, "enkicodex_bot", false)
	bot, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTelegramBotSessionName(t.Context(), bot.ID, inherit); err != nil {
		t.Fatal(err)
	}
	if err := store.CaptureTelegramBotOriginalName(t.Context(), bot.ID, original); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTelegramBotAppliedName(t.Context(), bot.ID, applied); err != nil {
		t.Fatal(err)
	}
	record := m.get(bot.ID).record
	record.DisplayName, record.ManagedBy = applied, manager
	if err := store.SaveTelegramBotRecord(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.defaultNames[token] = applied
	fake.mu.Unlock()
	m = restartBotNames(t, m, store, fake)
	return m, store, fake, newBotNameSessionFixture(t, store), bot.ID, token
}

func restartBotNames(t *testing.T, m *TelegramBotManager, store *registry.Store, fake *managedBotFake) *TelegramBotManager {
	t.Helper()
	restarted, err := NewTelegramBotManager(t.Context(), store, m.cfg, "original-secret", m.log)
	if err != nil {
		t.Fatal(err)
	}
	fake.attach(restarted)
	return restarted
}

func saveBotNameCreation(t *testing.T, m *TelegramBotManager, id, manager string, owner int64, name string) {
	t.Helper()
	now := time.Now().UTC()
	if err := m.store.SaveTelegramManagedBotIntent(t.Context(), registry.TelegramManagedBotIntentRecord{
		ID: uuid.NewString(), ManagerID: manager, OwnerID: owner, RequestedUsername: m.get(id).record.Username,
		DisplayName: name, Status: "connected", BotID: id, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}

func assertBotNameCapture(t *testing.T, m *TelegramBotManager, id, original string, before registry.TelegramBotRecord) {
	t.Helper()
	prefs, err := m.store.TelegramBotNamePreferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pref := range prefs {
		if pref.BotID == id {
			found = true
			if pref.OriginalDisplayName == nil || *pref.OriginalDisplayName != original {
				t.Fatalf("captured original changed: %+v", pref)
			}
		}
	}
	if !found {
		t.Fatal("captured original disappeared")
	}
	records, err := m.store.TelegramBotRecords(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.ID == id && (record.Username != before.Username || record.OwnerID != before.OwnerID || record.ManagedBy != before.ManagedBy || !bytes.Equal(record.EncryptedCredentials, before.EncryptedCredentials)) {
			t.Fatal("name synchronization changed bot identity or encrypted credentials")
		}
	}
}

func TestTelegramBotNamesCreationBaseRepairsHistoricalSessionCapture(t *testing.T) {
	for _, manager := range []string{"", "original-routing-id"} {
		t.Run("managed_by="+manager, func(t *testing.T) {
			m, store, fake, f, id, token := historicalBotNames(t, enkiCapturedName, enkiAppliedName, true, manager)
			before := m.get(id).record
			saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
			session, next := uuid.New(), uuid.New()
			f.session(t, session, "Assessment_sic_medico", false)
			f.selectSession(t, id, 7, 7, 0, session)
			m = restartBotNames(t, m, store, fake)
			if got := m.summary(m.get(id)).OriginalDisplayName; got != "EnkiCodex" {
				t.Fatalf("restart summary base = %q", got)
			}
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != "EnkiCodex · Assessment_sic_medico" {
				t.Fatalf("historical name repair = %q", got)
			}
			f.session(t, next, "Next session", false)
			f.selectSession(t, id, 7, 7, 0, next)
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != "EnkiCodex · Next session" {
				t.Fatalf("switch reused an old session: %q", got)
			}
			if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
				t.Fatal(err)
			}
			m.syncBotNames(t.Context())
			m = restartBotNames(t, m, store, fake)
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != "EnkiCodex" {
				t.Fatalf("flag-off/restart restored an old session: %q", got)
			}
			if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
				t.Fatal(err)
			}
			m.syncBotNames(t.Context())
			f.update++
			if result, err := store.AcceptTelegram(t.Context(), registry.IncomingUpdate{BotID: id, UpdateID: f.update, UserID: 7, ChatID: 7, Action: "disconnect"}); err != nil || result.ErrorCode != "" {
				t.Fatalf("disconnect = %+v %v", result, err)
			}
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != "EnkiCodex" || fake.count("getMyName") != 0 {
				t.Fatalf("disconnect recaptured or restored a session: %q", got)
			}
			assertBotNameCapture(t, m, id, enkiCapturedName, before)
		})
	}
}

func TestTelegramBotNamesCreationBaseRechecksCompletedFlagOff(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "late source", true: "startup source"}[restart], func(t *testing.T) {
			m, store, fake, _, id, token := historicalBotNames(t, enkiCapturedName, enkiCapturedName, false, "")
			if m.botNameState(id).pending {
				t.Fatal("fixture already pending")
			}
			saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
			if restart {
				m = restartBotNames(t, m, store, fake)
				if !m.summary(m.get(id)).NameSyncPending {
					t.Fatal("restart lost the required base correction")
				}
			}
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != "EnkiCodex" || m.summary(m.get(id)).NameSyncPending {
				t.Fatalf("completed flag-off ignored creation source: %q", got)
			}
		})
	}
}

func TestTelegramBotNamesExternalBaseKeepsLiteralSeparator(t *testing.T) {
	for _, test := range []struct {
		name, manager, intentManager string
		intentOwner                  int64
	}{
		{name: "no intent"},
		{name: "wrong owner", intentManager: "original-routing-id", intentOwner: 8},
		{name: "wrong manager", manager: "current-manager", intentManager: "other-manager", intentOwner: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			const original = "Support · Europe"
			m, store, fake, f, id, token := historicalBotNames(t, original, original, true, test.manager)
			before := m.get(id).record
			if test.intentManager != "" {
				saveBotNameCreation(t, m, id, test.intentManager, test.intentOwner, "Untrusted")
			}
			session := uuid.New()
			f.session(t, session, "Selected", false)
			f.selectSession(t, id, 7, 7, 0, session)
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != original+" · Selected" {
				t.Fatalf("external base was guessed: %q", got)
			}
			if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
				t.Fatal(err)
			}
			m = restartBotNames(t, m, store, fake)
			m.syncBotNames(t.Context())
			if got := fake.name(token); got != original {
				t.Fatalf("external base was not restored exactly: %q", got)
			}
			assertBotNameCapture(t, m, id, original, before)
		})
	}
}

func TestTelegramBotNamesCreationBaseStillCapturesActualDefaultBeforeRename(t *testing.T) {
	for _, test := range []struct{ actual, base, want string }{
		{"EnkiCodex · Old session", "EnkiCodex", "EnkiCodex · Assessment_sic_medico"},
		{"", "EnkiCodex", "EnkiCodex · Assessment_sic_medico"},
		{"EnkiCodex · Old session", "", "Assessment_sic_medico"},
	} {
		t.Run("actual="+test.actual+"/base="+test.base, func(t *testing.T) {
			m, _, fake, f, id := readyBotNames(t)
			before := m.get(id).record
			saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, test.base)
			fake.mu.Lock()
			fake.defaultNames[m.cfg.Secrets.BotToken] = test.actual
			fake.mu.Unlock()
			session := uuid.New()
			f.session(t, session, "Assessment_sic_medico", false)
			f.selectSession(t, id, 7, 7, 0, session)
			if err := m.SetTelegramBotSessionName(t.Context(), id, true); err != nil {
				t.Fatal(err)
			}
			m.syncBotNames(t.Context())
			if got := fake.name(m.cfg.Secrets.BotToken); got != test.want || fake.count("getMyName") != 1 {
				t.Fatalf("trusted source replaced actual capture: %q", got)
			}
			assertBotNameCapture(t, m, id, test.actual, before)
		})
	}
}

func TestTelegramBotNamesCreationBaseChangesRefreshAppliedCacheAndRetry(t *testing.T) {
	m, _, fake, f, id, token := historicalBotNames(t, enkiCapturedName, enkiAppliedName, true, "")
	before := m.get(id).record
	session := uuid.New()
	f.session(t, session, "Assessment_sic_medico", false)
	f.selectSession(t, id, 7, 7, 0, session)
	m.syncBotNames(t.Context())
	saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
	m.syncBotNames(t.Context())
	if got := fake.name(token); got != "EnkiCodex · Assessment_sic_medico" {
		t.Fatalf("late creation source did not refresh applied cache: %q", got)
	}
	saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "Enki Revised")
	fake.mu.Lock()
	fake.nameErrorCode, fake.nameRetryAfter = 429, 60
	fake.mu.Unlock()
	m.syncBotNames(t.Context())
	state := m.botNameState(id)
	if !state.pending || state.lastError != "name_update_unavailable" || time.Until(state.retryAt) < 59*time.Second {
		t.Fatalf("trusted-base retry state = %+v", state)
	}
	updates := fake.count("setMyName")
	m.syncBotNames(t.Context())
	if fake.count("setMyName") != updates {
		t.Fatal("base change ignored Telegram retry deadline")
	}
	fake.mu.Lock()
	fake.nameErrorCode = 0
	fake.mu.Unlock()
	f.session(t, session, "Latest selection", false)
	m.op.Lock()
	m.syncBotName(t.Context(), id, state.retryAt.Add(time.Second))
	m.op.Unlock()
	if got := fake.name(token); got != "Enki Revised · Latest selection" || m.summary(m.get(id)).NameError != "" {
		t.Fatalf("base retry did not use newest trusted source and selection: %q", got)
	}
	assertBotNameCapture(t, m, id, enkiCapturedName, before)
}

func TestTelegramBotNamesCreationBaseRestoresUnknownOutcomeAfterRestart(t *testing.T) {
	m, store, fake, f, id, token := historicalBotNames(t, enkiCapturedName, enkiAppliedName, true, "")
	before := m.get(id).record
	saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
	session := uuid.New()
	f.session(t, session, "Assessment_sic_medico", false)
	f.selectSession(t, id, 7, 7, 0, session)
	fake.mu.Lock()
	fake.nameApplyThenFail = true
	fake.mu.Unlock()
	m.syncBotNames(t.Context())
	if got := fake.name(token); got != "EnkiCodex · Assessment_sic_medico" || m.botNameState(id).preference.AppliedDisplayName != nil {
		t.Fatalf("fixture did not retain uncertain clean rename: %q", got)
	}
	if err := m.SetTelegramBotSessionName(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	m = restartBotNames(t, m, store, fake)
	fake.mu.Lock()
	fake.nameApplyThenFail = false
	fake.mu.Unlock()
	m.syncBotNames(t.Context())
	if got := fake.name(token); got != "EnkiCodex" || m.summary(m.get(id)).NameSyncPending {
		t.Fatalf("uncertain restart restored corrupted capture: %q", got)
	}
	assertBotNameCapture(t, m, id, enkiCapturedName, before)
}

func TestTelegramBotNamesCreationBaseSourceFailureRetainsSafeState(t *testing.T) {
	m, _, fake, f, id, token := historicalBotNames(t, enkiCapturedName, enkiAppliedName, true, "")
	before := m.get(id).record
	saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
	session := uuid.New()
	f.session(t, session, "Assessment_sic_medico", false)
	f.selectSession(t, id, 7, 7, 0, session)
	m.syncBotNames(t.Context())
	db, err := sql.Open("sqlite", m.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE telegram_managed_bot_intents RENAME TO unavailable_creation_intents`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(t.Context(), `ALTER TABLE unavailable_creation_intents RENAME TO telegram_managed_bot_intents`)
	}()
	f.session(t, session, "Renamed during outage", false)
	updates := fake.count("setMyName")
	m.syncBotNames(t.Context())
	summary := m.summary(m.get(id))
	if fake.count("setMyName") != updates || !summary.NameSyncPending || summary.NameError != "name_state_unavailable" || summary.OriginalDisplayName != "EnkiCodex" {
		t.Fatalf("source failure changed name or lost clean source: %+v", summary)
	}
	assertBotNameCapture(t, m, id, enkiCapturedName, before)
	if _, err := db.ExecContext(t.Context(), `ALTER TABLE unavailable_creation_intents RENAME TO telegram_managed_bot_intents`); err != nil {
		t.Fatal(err)
	}
	m.op.Lock()
	m.syncBotName(t.Context(), id, m.botNameState(id).retryAt.Add(time.Second))
	m.op.Unlock()
	if got := fake.name(token); got != "EnkiCodex · Renamed during outage" || m.summary(m.get(id)).NameError != "" {
		t.Fatalf("source recovery did not use current selection: %q", got)
	}
}

func TestTelegramBotNamesCreationBaseMovedWebhookBlocksCorrection(t *testing.T) {
	m, _, fake, _, id, token := historicalBotNames(t, enkiCapturedName, enkiCapturedName, false, "")
	saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
	fake.mu.Lock()
	fake.webhooks[token] = "https://elsewhere.example/webhook"
	fake.mu.Unlock()
	m.syncBotNames(t.Context())
	if fake.count("setMyName") != 0 || fake.name(token) != enkiCapturedName || m.summary(m.get(id)).NameError != "name_webhook_moved" {
		t.Fatal("base correction bypassed webhook ownership")
	}
}

func TestTelegramBotNamesCreationBaseSelectionChangesDuringRename(t *testing.T) {
	m, _, fake, f, id, token := historicalBotNames(t, enkiCapturedName, enkiAppliedName, true, "")
	saveBotNameCreation(t, m, id, m.cfg.Secrets.BotName, 7, "EnkiCodex")
	session, next := uuid.New(), uuid.New()
	f.session(t, session, "Assessment_sic_medico", false)
	f.session(t, next, "Latest selection", false)
	f.selectSession(t, id, 7, 7, 0, session)
	accepted, release := make(chan struct{}), make(chan struct{})
	transport := fake.server.Client().Transport
	client := m.get(id).client
	client.http = &http.Client{Transport: botNameRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/setMyName") {
			close(accepted)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return response, err
	})}
	done := make(chan struct{})
	go func() { m.syncBotNames(t.Context()); close(done) }()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("rename did not reach Telegram")
	}
	f.selectSession(t, id, 7, 7, 0, next)
	f.session(t, next, "Latest renamed selection", false)
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rename did not finish")
	}
	client.http = fake.server.Client()
	m.syncBotNames(t.Context())
	if got := fake.name(token); got != "EnkiCodex · Latest renamed selection" {
		t.Fatalf("in-flight selection change reused an old suffix: %q", got)
	}
}
