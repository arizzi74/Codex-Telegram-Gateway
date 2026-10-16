package registry

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"github.com/iaia/telegramgw/migrations"
)

func TestTelegramBotQuestionPreferencesDefaultOffPersistAndCascade(t *testing.T) {
	store := integrationStore(t)
	ctx := t.Context()
	bot := telegramBotRecordForTest("bot", true)
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	assertPreference := func(s *Store, enabled bool) {
		t.Helper()
		preferences, err := s.TelegramBotQuestionPreferences(ctx)
		if err != nil || len(preferences) != 1 || preferences[0].BotID != bot.ID || preferences[0].BroadcastQuestions != enabled {
			t.Fatalf("question preference = %+v %v, want broadcast=%v", preferences, err, enabled)
		}
	}
	assertPreference(store, false)
	if err := store.SetTelegramBotBroadcastQuestions(ctx, "missing", true); !errors.Is(err, ErrTelegramBotRecordNotFound) {
		t.Fatalf("unknown bot = %v", err)
	}
	if err := store.SetTelegramBotBroadcastQuestions(ctx, bot.ID, true); err != nil {
		t.Fatal(err)
	}
	bot.DisplayName, bot.EncryptedCredentials = "Refreshed display name", []byte("refreshed credentials")
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTelegramBotSessionName(ctx, bot.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.CaptureTelegramBotOriginalName(ctx, bot.ID, "Original name"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTelegramBotAppliedName(ctx, bot.ID, "Session name"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, store.pool.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertPreference(reopened, true)
	if err := reopened.SetTelegramBotBroadcastQuestions(ctx, bot.ID, false); err != nil {
		t.Fatal(err)
	}
	assertPreference(store, false)
	if err := store.DeleteTelegramBotRecord(ctx, bot.ID); err != nil {
		t.Fatal(err)
	}
	preferences, err := reopened.TelegramBotQuestionPreferences(ctx)
	if err != nil || len(preferences) != 0 {
		t.Fatalf("deleted bot retained preference: %+v %v", preferences, err)
	}
	var rows int
	if err := reopened.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_bot_question_preferences`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("preference did not cascade: rows=%d err=%v", rows, err)
	}
	if err := store.SaveTelegramBotRecord(ctx, bot); err != nil {
		t.Fatal(err)
	}
	assertPreference(reopened, false)
}

func TestTelegramBotQuestionMigrationDefaultsExistingBotToOff(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "gateway-v26.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `CREATE TABLE schema_migrations(version INTEGER NOT NULL PRIMARY KEY,checksum BLOB NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, file := range migrations.Files {
		version, err := migrationVersion(file)
		if err != nil {
			t.Fatal(err)
		}
		if version > 26 {
			continue
		}
		content, err := migrations.SQL.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			t.Fatalf("apply deployed schema %s: %v", file, err)
		}
		checksum := sha256.Sum256(content)
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, version, checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	bot := telegramBotRecordForTest("existing", true)
	if _, err := tx.Exec(ctx, saveTelegramBotSQL, telegramBotRecordArgs(bot)...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	preferences, err := store.TelegramBotQuestionPreferences(ctx)
	if err != nil || len(preferences) != 1 || preferences[0].BotID != bot.ID || preferences[0].BroadcastQuestions {
		t.Fatalf("migration enabled existing bot's broadcast: %+v %v", preferences, err)
	}
	if err := store.CheckMigrations(ctx); err != nil {
		t.Fatal(err)
	}
}
