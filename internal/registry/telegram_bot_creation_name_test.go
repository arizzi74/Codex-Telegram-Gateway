package registry

import (
	"reflect"
	"testing"
	"time"
)

func TestTelegramBotCreationNameScopesCompletedIntent(t *testing.T) {
	const original = "EnkiCodex · Fix Telegram gateway commands"
	for _, test := range []struct {
		name, manager, intentManager, status, creationName string
		intentOwner, lookupOwner                           int64
		noIntent, otherBot, wantName                       bool
	}{
		{name: "historical missing manager", intentManager: "manager", status: "connected", intentOwner: 10, lookupOwner: 10, creationName: "EnkiCodex", wantName: true},
		{name: "current manager", manager: "manager", intentManager: "manager", status: "connected", intentOwner: 10, lookupOwner: 10, creationName: "EnkiCodex", wantName: true},
		{name: "legitimate delimiter", manager: "manager", intentManager: "manager", status: "connected", intentOwner: 10, lookupOwner: 10, creationName: "EnkiCodex · Original name", wantName: true},
		{name: "empty creation name", manager: "manager", intentManager: "manager", status: "connected", intentOwner: 10, lookupOwner: 10, wantName: true},
		{name: "historical owner mismatch", intentManager: "manager", status: "connected", intentOwner: 11, lookupOwner: 10},
		{name: "current owner mismatch", manager: "manager", intentManager: "manager", status: "connected", intentOwner: 11, lookupOwner: 10},
		{name: "configured owner mismatch", manager: "manager", intentManager: "manager", status: "connected", intentOwner: 10, lookupOwner: 11},
		{name: "current manager mismatch", manager: "current", intentManager: "previous", status: "connected", intentOwner: 10, lookupOwner: 10},
		{name: "activating intent", intentManager: "manager", status: "activating", intentOwner: 10, lookupOwner: 10},
		{name: "pending intent", intentManager: "manager", status: "pending", intentOwner: 10, lookupOwner: 10},
		{name: "failed intent", intentManager: "manager", status: "failed", intentOwner: 10, lookupOwner: 10},
		{name: "expired intent", intentManager: "manager", status: "expired", intentOwner: 10, lookupOwner: 10},
		{name: "cancelled intent", intentManager: "manager", status: "cancelled", intentOwner: 10, lookupOwner: 10},
		{name: "other bot", intentManager: "manager", status: "connected", intentOwner: 10, lookupOwner: 10, otherBot: true},
		{name: "external bot without creation intent", lookupOwner: 10, noIntent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := integrationStore(t)
			ctx := t.Context()
			bot := telegramBotRecordForTest("bot", true)
			bot.ManagedBy = test.manager
			if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
				t.Fatal(err)
			}
			if err := store.SetTelegramBotSessionName(ctx, bot.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := store.CaptureTelegramBotOriginalName(ctx, bot.ID, original); err != nil {
				t.Fatal(err)
			}
			before, err := store.TelegramBotNamePreferences(ctx)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			intent := TelegramManagedBotIntentRecord{
				ID: "creation", ManagerID: test.intentManager, OwnerID: test.intentOwner,
				RequestedUsername: "CreationBot", DisplayName: test.creationName, Status: test.status,
				BotID: bot.ID, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
			}
			if test.otherBot {
				intent.BotID = "other"
			}
			if !test.noIntent {
				if err := store.SaveTelegramManagedBotIntent(ctx, intent); err != nil {
					t.Fatal(err)
				}
			}
			got, err := store.TelegramBotCreationName(ctx, bot.ID, test.lookupOwner)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantName {
				if got == nil || *got != test.creationName {
					t.Fatalf("creation name = %v, want %q", got, test.creationName)
				}
			} else if got != nil {
				t.Fatalf("untrusted creation name = %q", *got)
			}
			if test.lookupOwner == bot.OwnerID {
				want := original
				if test.wantName {
					want = test.creationName
				}
				snapshot := webUIBotTestSnapshot(t, store)
				if len(snapshot.Bots) != 1 || snapshot.Bots[0].DisplayName != want {
					t.Fatalf("WebUI creation name = %+v, want %q", snapshot.Bots, want)
				}
			}
			after, err := store.TelegramBotNamePreferences(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("creation lookup changed captured preferences: before=%+v after=%+v err=%v", before, after, err)
			}
			if !test.noIntent {
				afterIntent, err := store.TelegramManagedBotIntent(ctx, intent.ID)
				if err != nil || afterIntent.Status != intent.Status || afterIntent.DisplayName != intent.DisplayName {
					t.Fatalf("creation lookup changed intent: %+v %v", afterIntent, err)
				}
			}
		})
	}
}

func TestTelegramBotCreationNameSelectsNewestMatchingIntent(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	bot := telegramBotRecordForTest("bot", true)
	bot.ManagedBy = "manager"
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, test := range []struct {
		id, manager, status, name string
		owner                     int64
		created                   time.Time
	}{
		{"older-z", "manager", "connected", "Older name", 10, now.Add(-2 * time.Hour)},
		{"newer-z", "manager", "connected", "Newest name", 10, now.Add(-time.Hour)},
		{"newer-a", "manager", "connected", "Wrong tie winner", 10, now.Add(-time.Hour)},
		{"wrong-owner", "manager", "connected", "Wrong owner", 11, now},
		{"wrong-manager", "other", "connected", "Wrong manager", 10, now},
		{"unconnected", "manager", "activating", "Unfinished creation", 10, now},
	} {
		if err := store.SaveTelegramManagedBotIntent(ctx, TelegramManagedBotIntentRecord{
			ID: test.id, ManagerID: test.manager, OwnerID: test.owner, RequestedUsername: bot.Username,
			DisplayName: test.name, Status: test.status, BotID: bot.ID,
			CreatedAt: test.created, ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.TelegramBotCreationName(ctx, bot.ID, bot.OwnerID)
	if err != nil || got == nil || *got != "Newest name" {
		t.Fatalf("newest creation name = %v %v", got, err)
	}
	snapshot := webUIBotTestSnapshot(t, store)
	if len(snapshot.Bots) != 1 || snapshot.Bots[0].DisplayName != "Newest name" {
		t.Fatalf("WebUI chose a different creation: %+v", snapshot.Bots)
	}
}

func TestTelegramBotCreationNameRequiresExistingBot(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	if got, err := store.TelegramBotCreationName(ctx, "missing", 10); err != nil || got != nil {
		t.Fatalf("missing bot creation name = %v %v", got, err)
	}
	bot := telegramBotRecordForTest("bot", true)
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.SaveTelegramManagedBotIntent(ctx, TelegramManagedBotIntentRecord{
		ID: "creation", ManagerID: "manager", OwnerID: bot.OwnerID, RequestedUsername: bot.Username,
		DisplayName: "Creation name", Status: "connected", BotID: bot.ID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.TelegramBotCreationName(ctx, bot.ID, bot.OwnerID); err != nil || got == nil || *got != "Creation name" {
		t.Fatalf("existing bot creation name = %v %v", got, err)
	}
	if err := store.DeleteTelegramBotRecord(ctx, bot.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.TelegramBotCreationName(ctx, bot.ID, bot.OwnerID); err != nil || got != nil {
		t.Fatalf("deleted bot retained creation name = %v %v", got, err)
	}
	intent, err := store.TelegramManagedBotIntent(ctx, "creation")
	if err != nil || intent.Status != "connected" {
		t.Fatalf("expected historical connected intent to remain: %+v %v", intent, err)
	}
}
