package registry

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func managedBotIntentForTest(id, status string) TelegramManagedBotIntentRecord {
	now := time.Now().UTC()
	return TelegramManagedBotIntentRecord{ID: id, ManagerID: "manager", OwnerID: 10, RequestedUsername: "TestBot", DisplayName: "Test", LaunchURL: "https://t.me/newbot/ManagerBot/TestBot", Status: status, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
}

func TestTelegramManagedBotIntentDeleteGuardsAndReplayReceipts(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	for _, status := range []string{"pending", "expired", "failed", "cancelled", "activating", "connected"} {
		for _, associated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/associated=%v", status, associated), func(t *testing.T) {
				in := managedBotIntentForTest(fmt.Sprintf("%s-%v", status, associated), status)
				in.RequestedUsername = in.ID + "Bot"
				if associated {
					in.BotID = "telegram:222"
				}
				if err := store.SaveTelegramManagedBotIntent(ctx, in); err != nil {
					t.Fatal(err)
				}
				err := store.DeleteTelegramManagedBotIntent(ctx, in.ID, in.OwnerID)
				allowed := !associated && status != "activating" && status != "connected"
				if allowed {
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.TelegramManagedBotIntent(ctx, in.ID); !errors.Is(err, ErrTelegramBotRecordNotFound) {
						t.Fatalf("removed request remains: %v", err)
					}
				} else if !errors.Is(err, ErrTelegramManagedBotIntentConflict) {
					t.Fatalf("unsafe removal: %v", err)
				}
			})
		}
	}
	foreign := managedBotIntentForTest("foreign", "expired")
	if err := store.SaveTelegramManagedBotIntent(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTelegramManagedBotIntent(ctx, foreign.ID, 99); !errors.Is(err, ErrTelegramManagedBotIntentConflict) {
		t.Fatalf("foreign request removed: %v", err)
	}
	if err := store.DeleteTelegramManagedBotIntent(ctx, "missing", 10); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("missing request: %v", err)
	}
	if err := store.CompleteTelegramManagedBotUpdate(ctx, "manager", 100, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTelegramManagedBotIntent(ctx, foreign.ID, foreign.OwnerID); err != nil {
		t.Fatal(err)
	}
	if seen, err := store.TelegramManagedBotUpdateSeen(ctx, "manager", 100); err != nil || !seen {
		t.Fatalf("removal erased replay protection: seen=%v err=%v", seen, err)
	}
}

func TestTelegramManagedBotIntentReplaceGuardsAndExpiry(t *testing.T) {
	for _, test := range []struct {
		status     string
		expired    bool
		associated bool
		allowed    bool
	}{
		{status: "pending", expired: true, allowed: true},
		{status: "expired", allowed: true},
		{status: "failed", allowed: true},
		{status: "cancelled", allowed: true},
		{status: "pending"},
		{status: "activating"},
		{status: "connected"},
		{status: "expired", associated: true},
		{status: "failed", associated: true},
		{status: "cancelled", associated: true},
	} {
		t.Run(fmt.Sprintf("%s/expired=%v/associated=%v", test.status, test.expired, test.associated), func(t *testing.T) {
			store := integrationStore(t)
			in := managedBotIntentForTest("previous", test.status)
			if test.expired {
				in.ExpiresAt = time.Now().UTC().Add(-time.Minute)
			}
			if test.associated {
				in.BotID = "telegram:222"
			}
			if err := store.SaveTelegramManagedBotIntent(t.Context(), in); err != nil {
				t.Fatal(err)
			}
			next := managedBotIntentForTest("next", "pending")
			err := store.ReplaceTelegramManagedBotIntent(t.Context(), in, next)
			if !test.allowed {
				if !errors.Is(err, ErrTelegramManagedBotIntentConflict) {
					t.Fatalf("unsafe replacement: %v", err)
				}
				if _, err := store.TelegramManagedBotIntent(t.Context(), in.ID); err != nil {
					t.Fatalf("rejected replacement erased original: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.TelegramManagedBotIntent(t.Context(), in.ID); !errors.Is(err, ErrTelegramBotRecordNotFound) {
				t.Fatalf("replacement retained old request: %v", err)
			}
			if got, err := store.TelegramManagedBotIntent(t.Context(), next.ID); err != nil || got.Status != "pending" || got.BotID != "" {
				t.Fatalf("replacement not saved: %+v %v", got, err)
			}
		})
	}
}

func TestTelegramManagedBotIntentReplaceRollsBackSaveFailure(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	in := managedBotIntentForTest("previous", "expired")
	if err := store.SaveTelegramManagedBotIntent(ctx, in); err != nil {
		t.Fatal(err)
	}
	occupied := managedBotIntentForTest("occupied", "pending")
	occupied.RequestedUsername = "OtherBot"
	if err := store.SaveTelegramManagedBotIntent(ctx, occupied); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		next TelegramManagedBotIntentRecord
		want error
	}{
		{name: "duplicate username", next: func() TelegramManagedBotIntentRecord {
			next := managedBotIntentForTest("next", "pending")
			next.RequestedUsername = "OTHERBOT"
			return next
		}(), want: ErrTelegramBotUsernameReserved},
		{name: "failed insert", next: managedBotIntentForTest("occupied", "pending")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := store.ReplaceTelegramManagedBotIntent(ctx, in, test.next)
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("save unexpectedly succeeded: %v", err)
			}
			if got, err := store.TelegramManagedBotIntent(ctx, in.ID); err != nil || got != in {
				t.Fatalf("failed replacement modified original: %+v %v", got, err)
			}
		})
	}
}

func TestTelegramManagedBotIntentReplaceRejectsForeignIdentity(t *testing.T) {
	store := integrationStore(t)
	in := managedBotIntentForTest("previous", "expired")
	if err := store.SaveTelegramManagedBotIntent(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"owner", "manager", "display name", "snapshot"} {
		t.Run(field, func(t *testing.T) {
			previous, next := in, managedBotIntentForTest("next", "pending")
			switch field {
			case "owner":
				next.OwnerID = 99
			case "manager":
				next.ManagerID = "foreign"
			case "display name":
				next.DisplayName = "Changed"
			case "snapshot":
				previous.OwnerID, next.OwnerID = 99, 99
			}
			if err := store.ReplaceTelegramManagedBotIntent(t.Context(), previous, next); !errors.Is(err, ErrTelegramManagedBotIntentConflict) {
				t.Fatalf("foreign replacement: %v", err)
			}
		})
	}
	previous := in
	previous.ID = "missing"
	if err := store.ReplaceTelegramManagedBotIntent(t.Context(), previous, managedBotIntentForTest("next", "pending")); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("missing replacement: %v", err)
	}
}

func TestTelegramManagedBotIntentDeleteSerializesWithClaim(t *testing.T) {
	store := integrationStore(t)
	other, err := Open(t.Context(), store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	for i := 0; i < 12; i++ {
		in := managedBotIntentForTest(fmt.Sprintf("request-%d", i), "pending")
		in.RequestedUsername = fmt.Sprintf("Test%dBot", i)
		if err := store.SaveTelegramManagedBotIntent(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		bot := telegramBotRecordForTest(fmt.Sprintf("telegram:%d", 222+i), false)
		bot.TelegramID, bot.Username, bot.ManagedBy = int64(222+i), in.RequestedUsername, in.ManagerID
		start := make(chan struct{})
		removed, claimed := make(chan error, 1), make(chan error, 1)
		go func() { <-start; removed <- store.DeleteTelegramManagedBotIntent(t.Context(), in.ID, in.OwnerID) }()
		go func() { <-start; claimed <- other.ClaimTelegramManagedBotCreation(t.Context(), bot, in.ID) }()
		close(start)
		removeErr, claimErr := <-removed, <-claimed
		switch {
		case removeErr == nil:
			if !errors.Is(claimErr, ErrTelegramBotRecordNotFound) {
				t.Fatalf("removed request claimed: %v", claimErr)
			}
		case claimErr == nil:
			if !errors.Is(removeErr, ErrTelegramManagedBotIntentConflict) {
				t.Fatalf("claimed request removed: %v", removeErr)
			}
			if err := store.ReplaceTelegramManagedBotIntent(t.Context(), in, managedBotIntentForTest("replacement", "pending")); !errors.Is(err, ErrTelegramManagedBotIntentConflict) {
				t.Fatalf("claimed request replaced: %v", err)
			}
		default:
			t.Fatalf("claim/delete race had no winner: remove=%v claim=%v", removeErr, claimErr)
		}
	}
}

func TestTelegramManagedBotIntentDeleteRejectsLateClaim(t *testing.T) {
	store := integrationStore(t)
	in := managedBotIntentForTest("request", "pending")
	if err := store.SaveTelegramManagedBotIntent(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteTelegramManagedBotIntent(t.Context(), in.ID, in.OwnerID); err != nil {
		t.Fatal(err)
	}
	bot := telegramBotRecordForTest("telegram:222", false)
	bot.ManagedBy = in.ManagerID
	if err := store.ClaimTelegramManagedBotCreation(t.Context(), bot, in.ID); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("late claim enrolled removed request: %v", err)
	}
	if records, err := store.TelegramBotRecords(t.Context()); err != nil || len(records) != 0 {
		t.Fatalf("late claim saved credentials: %+v %v", records, err)
	}
}
