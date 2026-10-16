package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

func saveManagedIntentTest(t *testing.T, m *TelegramBotManager, status string) registry.TelegramManagedBotIntentRecord {
	t.Helper()
	now := time.Now().UTC()
	in := registry.TelegramManagedBotIntentRecord{ID: "request", ManagerID: m.cfg.Secrets.BotName, OwnerID: m.cfg.Secrets.WLID, RequestedUsername: "Second_Agent_bot", DisplayName: "Second Agent", LaunchURL: "https://t.me/newbot/PrimaryBot/Second_Agent_bot?name=Second+Agent", Status: status, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if status == "pending" {
		in.ExpiresAt = now.Add(time.Hour)
	}
	if err := m.store.SaveTelegramManagedBotIntent(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestTelegramManagedBotIntentRemoveIsLocalAndPreservesReplayProtection(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	intent, err := m.CreateManagedTelegramBot(t.Context(), "", "CreatedBot", "Created")
	if err != nil {
		t.Fatal(err)
	}
	fake.add(222, "CreatedBot", false)
	manager := m.cfg.Secrets.BotName
	if err := store.CompleteTelegramManagedBotUpdate(t.Context(), manager, 99, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveTelegramManagedBotIntent(t.Context(), intent.ID); err != nil {
		t.Fatal(err)
	}
	if intents, err := m.ListTelegramManagedBotIntents(t.Context()); err != nil || len(intents) != 0 {
		t.Fatalf("removed request still listed: %+v %v", intents, err)
	}
	if fake.count("deleteWebhook") != 0 || fake.count("getManagedBotToken") != 0 || fake.count("setWebhook") != 1 {
		t.Fatal("local request removal changed Telegram state")
	}
	if seen, err := store.TelegramManagedBotUpdateSeen(t.Context(), manager, 99); err != nil || !seen {
		t.Fatalf("removal erased replay receipt: seen=%v err=%v", seen, err)
	}
	late := managedUpdate(100, 7, 222, "CreatedBot")
	if code := postManaged(t, m, manager, late, "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if fake.count("getManagedBotToken") != 0 || m.get("telegram:222") != nil {
		t.Fatal("late callback enrolled removed creation request")
	}
	if _, err := m.CreateManagedTelegramBot(t.Context(), manager, "CreatedBot", "Created"); err != nil {
		t.Fatal(err)
	}
	if code := postManaged(t, m, manager, late, "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if fake.count("getManagedBotToken") != 0 || m.get("telegram:222") != nil {
		t.Fatal("ignored late callback replay consumed a later request")
	}
}

func TestTelegramManagedBotIntentRetryRechecksOriginalManagerAndUsername(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	managerToken := fake.add(333, "SecondaryBot", true)
	manager, err := m.ConnectTelegramBot(t.Context(), managerToken, false)
	if err != nil {
		t.Fatal(err)
	}
	in := saveManagedIntentTest(t, m, "expired")
	// The stored manager, rather than the current primary manager, owns retry.
	db, err := sql.Open("sqlite", m.cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `UPDATE telegram_managed_bot_intents SET manager_id=? WHERE intent_id=?`, manager.ID, in.ID); err != nil {
		t.Fatal(err)
	}
	in.ManagerID = manager.ID
	fake.mu.Lock()
	fresh := fake.identities[managerToken]
	fresh.Username = "RenamedManagerBot"
	fake.refreshed[managerToken] = fresh
	fake.mu.Unlock()
	if _, err := m.ConnectTelegramBot(t.Context(), fake.add(222, "Second_Agent_bot", false), false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.SaveTelegramManagedBotIntent(t.Context(), registry.TelegramManagedBotIntentRecord{ID: "competitor", ManagerID: "elsewhere", OwnerID: 7, RequestedUsername: "SECOND_AGENT1_BOT", DisplayName: "Other", LaunchURL: "https://t.me/", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var checked []string
	m.usernameExists = func(_ context.Context, username string) (bool, error) {
		checked = append(checked, username)
		return username == "second_agent2_bot", nil
	}
	started := time.Now().UTC()
	next, err := m.RetryTelegramManagedBotIntent(t.Context(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := url.Parse(next.LaunchURL)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == in.ID || next.ManagerID != manager.ID || next.DisplayName != in.DisplayName || next.Status != "pending" || next.BotID != "" || next.UsernameAvailability != "telegram_confirmation_required" || next.RequestedUsername != "second_agent3_bot" || launch.Path != "/newbot/RenamedManagerBot/second_agent3_bot" || launch.Query().Get("name") != in.DisplayName {
		t.Fatalf("incorrect retry result: %+v", next)
	}
	if !reflect.DeepEqual(checked, []string{"second_agent2_bot", "second_agent3_bot"}) {
		t.Fatalf("retry skipped username safeguards: %v", checked)
	}
	if !next.ExpiresAt.After(started.Add(TelegramManagedBotIntentTTL-time.Second)) || next.ExpiresAt.After(time.Now().UTC().Add(TelegramManagedBotIntentTTL)) {
		t.Fatalf("retry expiry was not refreshed: %v", next.ExpiresAt)
	}
	if _, err := store.TelegramManagedBotIntent(t.Context(), in.ID); !errors.Is(err, registry.ErrTelegramBotRecordNotFound) {
		t.Fatalf("retry retained original request: %v", err)
	}
	if records, err := store.TelegramManagedBotIntents(t.Context()); err != nil || len(records) != 2 {
		t.Fatalf("retry did not replace exactly one request: %+v %v", records, err)
	}
}

func TestTelegramManagedBotIntentRetryAllowsUnclaimedTerminalStates(t *testing.T) {
	for _, status := range []string{"expired", "failed", "cancelled", "pending"} {
		t.Run(status, func(t *testing.T) {
			m, store, _ := newTelegramBotManagerTest(t)
			in := saveManagedIntentTest(t, m, status)
			if status == "pending" {
				// A request may expire before any UI list operation reconciles it.
				db, err := sql.Open("sqlite", m.cfg.DatabasePath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.ExecContext(t.Context(), `UPDATE telegram_managed_bot_intents SET expires_at=? WHERE intent_id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), in.ID); err != nil {
					t.Fatal(err)
				}
			}
			m.usernameExists = func(context.Context, string) (bool, error) { return false, nil }
			next, err := m.RetryTelegramManagedBotIntent(t.Context(), in.ID)
			if err != nil || next.ID == in.ID || next.Status != "pending" || strings.EqualFold(next.RequestedUsername, in.RequestedUsername) {
				t.Fatalf("retry %s: %+v %v", status, next, err)
			}
			if intents, err := store.TelegramManagedBotIntents(t.Context()); err != nil || len(intents) != 1 || intents[0].ID != next.ID {
				t.Fatalf("retry did not replace request: %+v %v", intents, err)
			}
		})
	}
}

func TestTelegramManagedBotIntentLifecycleGuards(t *testing.T) {
	for _, test := range []struct {
		name, status string
		owner        int64
		bot          string
		remove       error
		retry        error
	}{
		{name: "pending", status: "pending", owner: 7, retry: ErrTelegramManagedBotIntentConflict},
		{name: "activating", status: "activating", owner: 7, remove: ErrTelegramManagedBotIntentConflict, retry: ErrTelegramManagedBotIntentConflict},
		{name: "connected", status: "connected", owner: 7, remove: ErrTelegramManagedBotIntentConflict, retry: ErrTelegramManagedBotIntentConflict},
		{name: "failed activation", status: "failed", owner: 7, bot: "telegram:222", remove: ErrTelegramManagedBotIntentConflict, retry: ErrTelegramManagedBotIntentConflict},
		{name: "cancelled activation", status: "cancelled", owner: 7, bot: "telegram:222", remove: ErrTelegramManagedBotIntentConflict, retry: ErrTelegramManagedBotIntentConflict},
		{name: "foreign owner", status: "expired", owner: 99, remove: ErrTelegramBotInvalid, retry: ErrTelegramBotInvalid},
	} {
		for _, operation := range []string{"retry", "remove"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				m, store, fake := newTelegramBotManagerTest(t)
				in := saveManagedIntentTest(t, m, test.status)
				db, err := sql.Open("sqlite", m.cfg.DatabasePath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.ExecContext(t.Context(), `UPDATE telegram_managed_bot_intents SET owner_id=?,bot_id=? WHERE intent_id=?`, test.owner, test.bot, in.ID); err != nil {
					t.Fatal(err)
				}
				want := test.remove
				if operation == "retry" {
					want = test.retry
					_, err = m.RetryTelegramManagedBotIntent(t.Context(), in.ID)
				} else {
					err = m.RemoveTelegramManagedBotIntent(t.Context(), in.ID)
				}
				if !errors.Is(err, want) {
					t.Fatalf("%s error = %v, want %v", operation, err, want)
				}
				if want != nil {
					if _, err := store.TelegramManagedBotIntent(t.Context(), in.ID); err != nil {
						t.Fatalf("rejection changed original request: %v", err)
					}
				}
				if fake.count("getChat") != 0 || fake.count("getManagedBotToken") != 0 || fake.count("deleteWebhook") != 0 || fake.count("setWebhook") != 0 {
					t.Fatal("guarded request reached Telegram")
				}
			})
		}
	}
	m, _, _ := newTelegramBotManagerTest(t)
	for _, id := range []string{"", "   ", "missing"} {
		want := ErrTelegramBotInvalid
		if id == "missing" {
			want = ErrTelegramBotNotFound
		}
		if err := m.RemoveTelegramManagedBotIntent(t.Context(), id); !errors.Is(err, want) {
			t.Fatalf("remove(%q): %v", id, err)
		}
		if _, err := m.RetryTelegramManagedBotIntent(t.Context(), id); !errors.Is(err, want) {
			t.Fatalf("retry(%q): %v", id, err)
		}
	}
}

func TestTelegramManagedBotIntentRetryFailurePreservesOriginal(t *testing.T) {
	for _, failure := range []string{"manager missing", "manager disabled", "management disabled", "profile unavailable", "username unavailable", "webhook failure", "save failure", "claim wins"} {
		t.Run(failure, func(t *testing.T) {
			m, store, fake := newTelegramBotManagerTest(t)
			in := saveManagedIntentTest(t, m, "expired")
			want := error(nil)
			m.usernameExists = func(context.Context, string) (bool, error) { return false, nil }
			switch failure {
			case "manager missing":
				delete(m.bots, m.cfg.Secrets.BotName)
				want = ErrTelegramBotNotFound
			case "manager disabled":
				bot := *m.get(m.cfg.Secrets.BotName)
				bot.record.Enabled = false
				m.put(&bot)
				want = ErrTelegramBotNotFound
			case "management disabled":
				fake.mu.Lock()
				fresh := fake.identities[m.cfg.Secrets.BotToken]
				fresh.CanManageBots = false
				fake.refreshed[m.cfg.Secrets.BotToken] = fresh
				fake.mu.Unlock()
				want = ErrTelegramBotManagerUnavailable
			case "profile unavailable":
				fake.failRefresh = true
			case "username unavailable":
				m.usernameExists = func(context.Context, string) (bool, error) { return false, errors.New("private details") }
				want = ErrTelegramBotUsernameUnverified
			case "webhook failure":
				fake.failSet = true
			case "save failure":
				db, err := sql.Open("sqlite", m.cfg.DatabasePath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_retry BEFORE INSERT ON telegram_managed_bot_intents BEGIN SELECT RAISE(FAIL,'private save details'); END`); err != nil {
					t.Fatal(err)
				}
			case "claim wins":
				// Model a different gateway claiming the record after retry reads
				// its snapshot, before the replacement transaction starts.
				m.usernameExists = func(ctx context.Context, _ string) (bool, error) {
					db, err := sql.Open("sqlite", m.cfg.DatabasePath)
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if _, err := db.ExecContext(ctx, `UPDATE telegram_managed_bot_intents SET status='activating',bot_id='telegram:222' WHERE intent_id=?`, in.ID); err != nil {
						t.Fatal(err)
					}
					return false, nil
				}
				want = ErrTelegramManagedBotIntentConflict
			}
			_, err := m.RetryTelegramManagedBotIntent(t.Context(), in.ID)
			if err == nil || (want != nil && !errors.Is(err, want)) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), m.cfg.Secrets.BotToken) {
				t.Fatalf("retry failure = %v, want %v", err, want)
			}
			got, err := store.TelegramManagedBotIntent(t.Context(), in.ID)
			if failure == "claim wins" {
				in.Status, in.BotID = "activating", "telegram:222"
			}
			if err != nil || got != in {
				t.Fatalf("failed retry modified original: %+v %v", got, err)
			}
			if intents, err := store.TelegramManagedBotIntents(t.Context()); err != nil || len(intents) != 1 {
				t.Fatalf("failed retry left an extra request: %+v %v", intents, err)
			}
		})
	}
}

func TestTelegramManagedBotIntentRetryHandlesReservationRace(t *testing.T) {
	m, store, _ := newTelegramBotManagerTest(t)
	in := saveManagedIntentTest(t, m, "expired")
	checks := 0
	m.usernameExists = func(ctx context.Context, username string) (bool, error) {
		checks++
		if checks == 1 {
			now := time.Now().UTC()
			if err := store.SaveTelegramManagedBotIntent(ctx, registry.TelegramManagedBotIntentRecord{ID: "competitor", ManagerID: "elsewhere", OwnerID: 7, RequestedUsername: username, DisplayName: "Other", LaunchURL: "https://t.me/", Status: "pending", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
		}
		return false, nil
	}
	next, err := m.RetryTelegramManagedBotIntent(t.Context(), in.ID)
	if err != nil || next.RequestedUsername != "second_agent2_bot" || checks != 2 {
		t.Fatalf("retry did not recover reservation race: %+v %v, checks=%d", next, err, checks)
	}
	if records, err := store.TelegramManagedBotIntents(t.Context()); err != nil || len(records) != 2 {
		t.Fatalf("reservation retry left incorrect requests: %s %v", fmt.Sprint(records), err)
	}
}

func TestTelegramManagedBotIntentRetryRejectsUnseenOldCallback(t *testing.T) {
	m, store, fake := newTelegramBotManagerTest(t)
	in := saveManagedIntentTest(t, m, "expired")
	fake.add(222, in.RequestedUsername, false)
	var checked []string
	m.usernameExists = func(_ context.Context, username string) (bool, error) {
		checked = append(checked, username)
		return false, nil
	}
	next, err := m.RetryTelegramManagedBotIntent(t.Context(), in.ID)
	if err != nil || next.RequestedUsername != "second_agent1_bot" || !reflect.DeepEqual(checked, []string{"second_agent1_bot"}) {
		t.Fatalf("retry reused or checked original username: %+v %v, checked=%v", next, err, checked)
	}
	manager := m.cfg.Secrets.BotName
	if code := postManaged(t, m, manager, managedUpdate(100, 7, 222, in.RequestedUsername), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if fake.count("getManagedBotToken") != 0 || m.get("telegram:222") != nil {
		t.Fatal("unseen callback for original request claimed replacement")
	}
	if seen, err := store.TelegramManagedBotUpdateSeen(t.Context(), manager, 100); err != nil || !seen {
		t.Fatalf("ignored old callback did not acquire replay receipt: %v %v", seen, err)
	}
	fake.add(333, next.RequestedUsername, false)
	if code := postManaged(t, m, manager, managedUpdate(101, 7, 333, next.RequestedUsername), "original-secret"); code != 200 {
		t.Fatal(code)
	}
	if bot := m.get("telegram:333"); bot == nil || !bot.record.Enabled {
		t.Fatal("replacement request could not enroll its intended bot")
	}
}
