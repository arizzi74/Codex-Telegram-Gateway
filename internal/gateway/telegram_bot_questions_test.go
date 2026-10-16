package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

func questionBotSummary(t *testing.T, m *TelegramBotManager, id string) TelegramBotSummary {
	t.Helper()
	bots, err := m.ListTelegramBots(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, bot := range bots {
		if bot.ID == id {
			return bot
		}
	}
	t.Fatalf("bot %q missing from list", id)
	return TelegramBotSummary{}
}

func TestTelegramBotQuestionPreferencesDefaultOffAndReadAuthoritativeState(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	secondary, err := m.ConnectTelegramBot(t.Context(), fake.add(222, "SecondaryBot", false), false)
	if err != nil {
		t.Fatal(err)
	}
	if secondary.BroadcastQuestions {
		t.Fatal("newly connected bot broadcasts questions by default")
	}
	primary := m.cfg.Secrets.BotName
	if questionBotSummary(t, m, primary).BroadcastQuestions {
		t.Fatal("existing primary bot broadcasts questions by default")
	}
	before := map[string]int{}
	fake.mu.Lock()
	for method, count := range fake.calls {
		before[method] = count
	}
	fake.mu.Unlock()
	for _, id := range []string{primary, secondary.ID} {
		if err := m.SetTelegramBotBroadcastQuestions(t.Context(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetTelegramBotBroadcastQuestions(t.Context(), "missing", true); !errors.Is(err, ErrTelegramBotNotFound) {
		t.Fatalf("unknown bot error = %v", err)
	}
	fake.mu.Lock()
	for method, count := range fake.calls {
		if before[method] != count {
			t.Errorf("setting routing preference called Telegram %s", method)
		}
	}
	fake.mu.Unlock()
	if !questionBotSummary(t, m, primary).BroadcastQuestions || !questionBotSummary(t, m, secondary.ID).BroadcastQuestions {
		t.Fatal("manager did not publish persisted opt-in")
	}
	// Another registry writer can change the preference while the manager's
	// bot identity remains cached. The next list must use persisted state.
	if err := store.SetTelegramBotBroadcastQuestions(t.Context(), secondary.ID, false); err != nil {
		t.Fatal(err)
	}
	if questionBotSummary(t, m, secondary.ID).BroadcastQuestions {
		t.Fatal("list returned a stale question preference")
	}
	if !questionBotSummary(t, m, primary).BroadcastQuestions {
		t.Fatal("secondary preference changed the primary bot")
	}
	data, err := json.Marshal(questionBotSummary(t, m, secondary.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"broadcast_questions":false`)) {
		t.Fatalf("default-off flag was omitted: %s", data)
	}
	for _, secret := range []string{m.cfg.Secrets.BotToken, m.get(secondary.ID).cfg.Secrets.BotToken, m.get(secondary.ID).secret} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("question preference summary exposed credentials")
		}
	}
}

func TestTelegramBotQuestionPreferenceSurvivesIdentityAndCredentialChanges(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	token := fake.add(222, "SecondaryBot", false)
	secondary, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err != nil {
		t.Fatal(err)
	}
	id := secondary.ID
	if err := m.SetTelegramBotBroadcastQuestions(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.refreshed[token] = TelegramUser{ID: 222, Username: "RenamedBot", FirstName: "New profile", IsBot: true}
	fake.mu.Unlock()
	refreshed, err := m.RefreshTelegramBots(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, bot := range refreshed {
		if bot.ID == id && (!bot.BroadcastQuestions || bot.Username != "RenamedBot") {
			t.Fatalf("profile refresh lost question preference: %+v", bot)
		}
	}
	rotated := "222:" + strings.Repeat("z", 30)
	fake.mu.Lock()
	fake.identities[rotated] = fake.identities[token]
	fake.mu.Unlock()
	replaced, err := m.ReplaceTelegramBotToken(t.Context(), id, rotated, false)
	if err != nil {
		t.Fatal(err)
	}
	if !replaced.BroadcastQuestions || m.get(id).cfg.Secrets.BotToken != rotated {
		t.Fatal("token replacement lost preference or did not rotate credentials")
	}
	if err := m.SetTelegramBotEnabled(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if bot := questionBotSummary(t, m, id); bot.Enabled || !bot.BroadcastQuestions {
		t.Fatalf("disable lost preference: %+v", bot)
	}
	if err := m.SetTelegramBotEnabled(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	if !questionBotSummary(t, m, id).BroadcastQuestions {
		t.Fatal("re-enable lost preference")
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
	if !questionBotSummary(t, restarted, id).BroadcastQuestions || questionBotSummary(t, restarted, m.cfg.Secrets.BotName).BroadcastQuestions {
		t.Fatal("restart did not preserve each bot's preference")
	}
	if err := restarted.SetTelegramBotBroadcastQuestions(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if questionBotSummary(t, m, id).BroadcastQuestions {
		t.Fatal("another manager's opt-out was hidden by cached state")
	}
	if err := restarted.SetTelegramBotBroadcastQuestions(t.Context(), id, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RemoveTelegramBot(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	preferences, err := store.TelegramBotQuestionPreferences(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, preference := range preferences {
		if preference.BotID == id {
			t.Fatal("removed bot retained its question preference")
		}
	}
	if err := restarted.SetTelegramBotBroadcastQuestions(t.Context(), id, true); !errors.Is(err, ErrTelegramBotNotFound) {
		t.Fatalf("removed bot setting error = %v", err)
	}
	reconnected, err := restarted.ConnectTelegramBot(t.Context(), rotated, false)
	if err != nil {
		t.Fatal(err)
	}
	if reconnected.BroadcastQuestions {
		t.Fatal("reconnecting a removed bot restored an obsolete opt-in")
	}
}

func TestTelegramBotQuestionPreferencesFailClosedWithoutCachedSuccess(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	secondary, err := m.ConnectTelegramBot(t.Context(), fake.add(222, "SecondaryBot", false), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetTelegramBotBroadcastQuestions(t.Context(), secondary.ID, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.SetTelegramBotBroadcastQuestions(ctx, secondary.ID, false); err == nil {
		t.Fatal("failed preference write reported success")
	}
	if !questionBotSummary(t, m, secondary.ID).BroadcastQuestions {
		t.Fatal("failed write changed the published preference")
	}
	// Ensure cached identity metadata cannot turn a failed registry read into
	// a successful response with a default or stale routing preference.
	for _, id := range []string{m.cfg.Secrets.BotName, secondary.ID} {
		copy := *m.get(id)
		copy.checkedAt = time.Now()
		m.put(&copy)
	}
	if bots, err := m.ListTelegramBots(ctx); err == nil || bots != nil {
		t.Fatalf("failed preference read returned a list: %v %v", bots, err)
	}
	if err := store.DeleteTelegramBotRecord(t.Context(), secondary.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTelegramBotBroadcastQuestions(t.Context(), secondary.ID, true); !errors.Is(err, ErrTelegramBotNotFound) {
		t.Fatalf("stale runtime bot hid missing registry state: %v", err)
	}
}
