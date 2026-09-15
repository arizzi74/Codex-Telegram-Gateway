package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SQLite write transactions begin with BEGIN IMMEDIATE, serializing command
// acceptance and event ingestion before either can read a selection revision.
func selectionRevision(ctx context.Context, tx *dbTx, in IncomingUpdate) (uint64, error) {
	if err := validateSelectionContext(tx, in); err != nil {
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

func bumpSelectionRevision(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	if err := validateSelectionContext(tx, in); err != nil {
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

func validateSelectionContext(tx *dbTx, in IncomingUpdate) error {
	if tx == nil || strings.TrimSpace(in.BotID) == "" || in.UserID == 0 || in.ChatID == 0 || in.TopicID < 0 {
		return errors.New("registry: invalid Telegram selection context")
	}
	return nil
}
