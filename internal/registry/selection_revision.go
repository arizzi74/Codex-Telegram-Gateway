package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// lockAutomaticBindingContext establishes the same context-first lock order as
// AcceptTelegram before event ingestion takes worker, runtime, session, or
// command locks. Only new/fork completions can automatically change a Telegram
// selection, so all other events avoid this lookup and lock.
func lockAutomaticBindingContext(ctx context.Context, tx pgx.Tx, workerID uuid.UUID, event protocol.Event) error {
	if event.Kind != "command_completed" {
		return nil
	}
	var result protocol.Result
	if err := json.Unmarshal(event.Data, &result); err != nil {
		return nil // The normal event decoder reports malformed result payloads.
	}
	if result.Session == nil {
		return nil
	}
	commandID, err := uuid.Parse(result.CommandID)
	if err != nil {
		return nil // Normal target validation reports a missing/invalid command ID.
	}
	var runtimeID, sessionID any
	if parsed, err := uuid.Parse(event.RuntimeID); err == nil {
		runtimeID = parsed
	}
	if parsed, err := uuid.Parse(event.SessionID); err == nil {
		sessionID = parsed
	}
	var in IncomingUpdate
	err = tx.QueryRow(ctx, `SELECT telegram_bot_id, telegram_user_id,
        telegram_chat_id, COALESCE(telegram_message_thread_id, 0)
        FROM commands
        WHERE command_id=$1 AND worker_id=$2
          AND runtime_id IS NOT DISTINCT FROM $3
          AND runtime_generation=$4
          AND session_id IS NOT DISTINCT FROM $5
          AND telegram_bot_id IS NOT NULL AND telegram_user_id IS NOT NULL
          AND telegram_chat_id IS NOT NULL
          AND (operation='new_session' OR
              (operation='codex_command' AND payload #>> '{arguments,codex,name}'='fork'))`,
		commandID, workerID, runtimeID, int64(event.RuntimeGeneration), sessionID).
		Scan(&in.BotID, &in.UserID, &in.ChatID, &in.TopicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registry: resolve automatic Telegram binding context: %w", err)
	}
	return lockSelectionRevision(ctx, tx, in)
}

func selectionRevision(ctx context.Context, tx pgx.Tx, in IncomingUpdate) (uint64, error) {
	if err := lockSelectionRevision(ctx, tx, in); err != nil {
		return 0, err
	}
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO telegram_selection_revisions
        (bot_id, user_id, chat_id, message_thread_id, revision)
        VALUES ($1,$2,$3,$4,0)
        ON CONFLICT (bot_id, user_id, chat_id, message_thread_id) DO UPDATE
        SET revision = telegram_selection_revisions.revision
        RETURNING revision`, in.BotID, in.UserID, in.ChatID, in.TopicID).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("registry: read Telegram selection revision: %w", err)
	}
	return uint64(revision), nil
}

func bumpSelectionRevision(ctx context.Context, tx pgx.Tx, in IncomingUpdate) error {
	if err := lockSelectionRevision(ctx, tx, in); err != nil {
		return err
	}
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO telegram_selection_revisions
        (bot_id, user_id, chat_id, message_thread_id, revision)
        VALUES ($1,$2,$3,$4,1)
        ON CONFLICT (bot_id, user_id, chat_id, message_thread_id) DO UPDATE
        SET revision = telegram_selection_revisions.revision + 1
        RETURNING revision`, in.BotID, in.UserID, in.ChatID, in.TopicID).Scan(&revision)
	if err != nil {
		return fmt.Errorf("registry: advance Telegram selection revision: %w", err)
	}
	return nil
}

func lockSelectionRevision(ctx context.Context, tx pgx.Tx, in IncomingUpdate) error {
	if tx == nil || strings.TrimSpace(in.BotID) == "" || in.UserID == 0 || in.ChatID == 0 || in.TopicID < 0 {
		return errors.New("registry: invalid Telegram selection context")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", telegramContextKey(in)); err != nil {
		return fmt.Errorf("registry: lock Telegram selection context: %w", err)
	}
	return nil
}
