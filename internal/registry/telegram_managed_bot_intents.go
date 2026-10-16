package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrTelegramManagedBotIntentConflict = errors.New("registry: Telegram creation request is already active or connected")

// TelegramManagedBotIntent looks up one request without the history list limit.
// Reconcile expiry in the returned value; mutation guards also check the stored
// expiry so callers do not need a preceding list operation.
func (s *Store) TelegramManagedBotIntent(ctx context.Context, id string) (TelegramManagedBotIntentRecord, error) {
	var in TelegramManagedBotIntentRecord
	err := s.pool.QueryRow(ctx, `SELECT intent_id,manager_id,owner_id,requested_username,display_name,launch_url,status,bot_id,created_at,expires_at FROM telegram_managed_bot_intents WHERE intent_id=$1`, id).Scan(&in.ID, &in.ManagerID, &in.OwnerID, &in.RequestedUsername, &in.DisplayName, &in.LaunchURL, &in.Status, &in.BotID, &in.CreatedAt, &in.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrTelegramBotRecordNotFound
	}
	if err == nil && in.Status == "pending" && !in.ExpiresAt.After(time.Now().UTC()) {
		in.Status = "expired"
	}
	return in, err
}

// DeleteTelegramManagedBotIntent removes only an unclaimed local request. The
// conditional delete shares the transaction boundary with creation claims;
// bot credentials and managed-update replay receipts are never removed here.
func (s *Store) DeleteTelegramManagedBotIntent(ctx context.Context, id string, owner int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM telegram_managed_bot_intents WHERE intent_id=$1 AND owner_id=$2 AND bot_id='' AND status IN ('pending','expired','failed','cancelled')`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return telegramManagedBotIntentMutationMiss(ctx, tx, id)
	}
	return tx.Commit(ctx)
}

// ReplaceTelegramManagedBotIntent atomically replaces an unsuccessful,
// unclaimed request with a fresh reservation. Any save failure rolls back the
// deletion, and a competing callback claim makes the replacement conflict.
func (s *Store) ReplaceTelegramManagedBotIntent(ctx context.Context, previous, next TelegramManagedBotIntentRecord) error {
	if next.ID == "" || next.ID == previous.ID || next.ManagerID != previous.ManagerID || next.OwnerID != previous.OwnerID || next.DisplayName != previous.DisplayName || next.Status != "pending" || next.BotID != "" {
		return ErrTelegramManagedBotIntentConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM telegram_managed_bot_intents WHERE intent_id=$1 AND manager_id=$2 AND owner_id=$3 AND requested_username=$4 COLLATE NOCASE AND display_name=$5 AND bot_id='' AND (status IN ('expired','failed','cancelled') OR (status='pending' AND expires_at<=$6))`, previous.ID, previous.ManagerID, previous.OwnerID, previous.RequestedUsername, previous.DisplayName, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return telegramManagedBotIntentMutationMiss(ctx, tx, previous.ID)
	}
	if err = saveTelegramManagedBotIntent(ctx, tx, next); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func telegramManagedBotIntentMutationMiss(ctx context.Context, tx *dbTx, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_managed_bot_intents WHERE intent_id=$1)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrTelegramBotRecordNotFound
	}
	return ErrTelegramManagedBotIntentConflict
}
