package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TelegramBotRecord is an internal persistence record. Credentials are opaque
// authenticated ciphertext; this type must never be returned by admin handlers.
type TelegramBotRecord struct {
	ID                             string
	TelegramID, OwnerID            int64
	Username, DisplayName          string
	Legacy, Enabled, CanManageBots bool
	ManagedBy                      string
	EncryptedCredentials           []byte
	LastError                      string
	CreatedAt, UpdatedAt           time.Time
}

type TelegramManagedBotIntentRecord struct {
	ID, ManagerID                                            string
	OwnerID                                                  int64
	RequestedUsername, DisplayName, LaunchURL, Status, BotID string
	CreatedAt, ExpiresAt                                     time.Time
}

var (
	ErrTelegramBotRecordNotFound   = errors.New("registry: Telegram bot record not found")
	ErrTelegramBotUsernameReserved = errors.New("registry: Telegram bot username is already reserved")
)

const telegramManagedBotUsernameReservedSQL = `SELECT EXISTS(
    SELECT 1 FROM telegram_bots WHERE username=$1 COLLATE NOCASE
    UNION ALL
    SELECT 1 FROM telegram_managed_bot_intents WHERE requested_username=$1 COLLATE NOCASE
      AND (status='activating' OR (status='pending' AND expires_at>$2)))`

// TelegramManagedBotUsernameReserved checks all connected bots and active
// requests, across managers. It deliberately does not use the bounded list
// returned to the admin UI or claim availability outside this gateway.
func (s *Store) TelegramManagedBotUsernameReserved(ctx context.Context, username string) (bool, error) {
	var reserved bool
	err := s.pool.QueryRow(ctx, telegramManagedBotUsernameReservedSQL, username, time.Now().UTC()).Scan(&reserved)
	return reserved, err
}

const saveTelegramBotSQL = `INSERT INTO telegram_bots(bot_id,telegram_id,owner_id,username,display_name,legacy,enabled,can_manage_bots,managed_by,encrypted_credentials,last_error,created_at,updated_at)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
        ON CONFLICT(bot_id) DO UPDATE SET telegram_id=excluded.telegram_id,owner_id=excluded.owner_id,username=excluded.username,display_name=excluded.display_name,enabled=excluded.enabled,can_manage_bots=excluded.can_manage_bots,managed_by=excluded.managed_by,encrypted_credentials=excluded.encrypted_credentials,last_error=excluded.last_error,updated_at=excluded.updated_at`

func telegramBotRecordArgs(b TelegramBotRecord) []any {
	return []any{b.ID, b.TelegramID, b.OwnerID, b.Username, b.DisplayName, b.Legacy, b.Enabled, b.CanManageBots, b.ManagedBy, b.EncryptedCredentials, b.LastError, b.CreatedAt, b.UpdatedAt}
}

func (s *Store) TelegramBotRecords(ctx context.Context) ([]TelegramBotRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT bot_id,telegram_id,owner_id,username,display_name,legacy,enabled,can_manage_bots,managed_by,encrypted_credentials,last_error,created_at,updated_at FROM telegram_bots ORDER BY legacy DESC,created_at,bot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramBotRecord{}
	for rows.Next() {
		var b TelegramBotRecord
		if err := rows.Scan(&b.ID, &b.TelegramID, &b.OwnerID, &b.Username, &b.DisplayName, &b.Legacy, &b.Enabled, &b.CanManageBots, &b.ManagedBy, &b.EncryptedCredentials, &b.LastError, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) SaveTelegramBotRecord(ctx context.Context, b TelegramBotRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var previousName string
	var previousOwner int64
	var previousEnabled, previousRetired bool
	err = tx.QueryRow(ctx, `SELECT display_name,owner_id,enabled,
        EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=$1)
        FROM telegram_bots WHERE bot_id=$1`, b.ID).Scan(&previousName, &previousOwner, &previousEnabled, &previousRetired)
	changed := errors.Is(err, sql.ErrNoRows) || previousName != b.DisplayName || previousOwner != b.OwnerID || previousEnabled != b.Enabled || (b.Enabled && previousRetired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(ctx, saveTelegramBotSQL, telegramBotRecordArgs(b)...); err != nil {
		return err
	}
	if b.Enabled {
		if _, err = tx.Exec(ctx, `DELETE FROM telegram_bot_retirements WHERE bot_id=$1`, b.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if changed {
		s.notifySessionActivity()
	}
	return nil
}

// ClaimTelegramManagedBotCreation consumes an unexpired request and saves its
// disabled credentials in one transaction. Webhook activation happens after
// this commit, so a crash or slow API call cannot enroll an expired request.
func (s *Store) ClaimTelegramManagedBotCreation(ctx context.Context, b TelegramBotRecord, intent string) error {
	if b.Enabled || b.Legacy || b.ManagedBy == "" || len(b.EncryptedCredentials) == 0 {
		return ErrTelegramBotRecordNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE telegram_managed_bot_intents SET status='activating',bot_id=$1 WHERE intent_id=$2 AND manager_id=$3 AND owner_id=$4 AND requested_username=$5 COLLATE NOCASE AND status='pending' AND bot_id='' AND expires_at>$6`, b.ID, intent, b.ManagedBy, b.OwnerID, b.Username, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTelegramBotRecordNotFound
	}
	if _, err = tx.Exec(ctx, saveTelegramBotSQL, telegramBotRecordArgs(b)...); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifySessionActivity()
	return nil
}

func (s *Store) DeleteTelegramBotRecord(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var legacy bool
	if err = tx.QueryRow(ctx, `SELECT legacy FROM telegram_bots WHERE bot_id=$1`, id).Scan(&legacy); err != nil {
		return err
	}
	if legacy {
		return errors.New("registry: primary Telegram bot cannot be removed")
	}
	for _, query := range []string{
		`INSERT INTO telegram_bot_retirements(bot_id) VALUES($1) ON CONFLICT DO NOTHING`,
		`INSERT INTO telegram_bot_retired_commands(bot_id,command_id) SELECT telegram_bot_id,command_id FROM commands WHERE telegram_bot_id=$1 ON CONFLICT DO NOTHING`,
		`DELETE FROM telegram_callbacks WHERE json_extract(payload,'$.bot_id')=$1`,
		`DELETE FROM telegram_input_reply_messages WHERE bot_id=$1`,
		`UPDATE telegram_input_reply_helpers SET retired=1,backfill_pending=0 WHERE delivery_id IN (SELECT delivery_id FROM telegram_deliveries WHERE bot_id=$1)`,
		`DELETE FROM bot_message_routes WHERE bot_id=$1`,
		`DELETE FROM telegram_bindings WHERE bot_id=$1`,
		`DELETE FROM telegram_chat_modes WHERE bot_id=$1`,
		`DELETE FROM telegram_session_wizards WHERE bot_id=$1`,
		// Keep only the monotonic fence, so a late new/fork completion cannot
		// recover its previously captured revision after the bot is reconnected.
		`UPDATE telegram_selection_revisions SET revision=revision+1 WHERE bot_id=$1`,
		`DELETE FROM worker_update_watchers WHERE bot_id=$1`,
		`DELETE FROM telegram_chat_policies WHERE bot_id=$1`,
		`UPDATE telegram_managed_bot_intents SET status='cancelled' WHERE (manager_id=$1 AND status='pending') OR (bot_id=$1 AND status='activating')`,
		`DELETE FROM telegram_bots WHERE bot_id=$1 AND legacy=0`,
	} {
		if _, err = tx.Exec(ctx, query, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifySessionActivity()
	return nil
}

func (s *Store) TelegramBotRetired(ctx context.Context, id string) (bool, error) {
	var retired bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=$1)`, id).Scan(&retired)
	return retired, err
}

// RetireTelegramBotWork irrevocably discards queued content on disable/remove.
// A later enable or reconnect must not replay previously queued private work.
func (s *Store) RetireTelegramBotWork(ctx context.Context, bot string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE telegram_deliveries SET status=CASE WHEN status='sending' THEN 'sending' ELSE 'cancelled' END,visibility_revoked=1,last_error=NULL WHERE bot_id=$1 AND status IN ('pending','failed','sending')`, bot); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_progress_messages SET retire_requested=1,next_attempt_at=`+sqliteNow+` WHERE bot_id=$1 AND status<>'deleted'`, bot); err != nil {
		return err
	}
	if err = cancelTelegramVoiceJobs(ctx, tx, "bot_id=$1", []any{bot}, "voice_unauthorized"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='cancelled',prepared=0,lease_id=NULL,lease_until=NULL,error_code='speech_unauthorized',completed_at=`+sqliteNow+` WHERE bot_id=$1 AND status IN ('pending','processing')`, bot); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE telegram_voice_jobs SET reply_with_audio=0 WHERE bot_id=$1`, bot); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	s.notifyTelegramVoiceJobs()
	return nil
}

func (s *Store) SaveTelegramManagedBotIntent(ctx context.Context, in TelegramManagedBotIntentRecord) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = saveTelegramManagedBotIntent(ctx, tx, in); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func saveTelegramManagedBotIntent(ctx context.Context, tx *dbTx, in TelegramManagedBotIntentRecord) error {
	if _, err := tx.Exec(ctx, `UPDATE telegram_managed_bot_intents SET status='expired' WHERE status='pending' AND expires_at<=$1`, time.Now().UTC()); err != nil {
		return err
	}
	if in.Status == "pending" {
		// SQLite IMMEDIATE transactions serialize this check and insert, even
		// when separate gateway instances use different manager bots.
		var reserved bool
		if err := tx.QueryRow(ctx, telegramManagedBotUsernameReservedSQL, in.RequestedUsername, time.Now().UTC()).Scan(&reserved); err != nil {
			return err
		}
		if reserved {
			return ErrTelegramBotUsernameReserved
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO telegram_managed_bot_intents(intent_id,manager_id,owner_id,requested_username,display_name,launch_url,status,bot_id,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, in.ID, in.ManagerID, in.OwnerID, in.RequestedUsername, in.DisplayName, in.LaunchURL, in.Status, in.BotID, in.CreatedAt, in.ExpiresAt)
	return err
}

func (s *Store) TelegramManagedBotIntents(ctx context.Context) ([]TelegramManagedBotIntentRecord, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE telegram_managed_bot_intents SET status='expired' WHERE status='pending' AND expires_at<=$1`, time.Now().UTC()); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT intent_id,manager_id,owner_id,requested_username,display_name,launch_url,status,bot_id,created_at,expires_at FROM telegram_managed_bot_intents ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramManagedBotIntentRecord{}
	for rows.Next() {
		var in TelegramManagedBotIntentRecord
		if err := rows.Scan(&in.ID, &in.ManagerID, &in.OwnerID, &in.RequestedUsername, &in.DisplayName, &in.LaunchURL, &in.Status, &in.BotID, &in.CreatedAt, &in.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) PendingTelegramManagedBotIntent(ctx context.Context, manager string, owner int64, username string) (TelegramManagedBotIntentRecord, error) {
	var in TelegramManagedBotIntentRecord
	err := s.pool.QueryRow(ctx, `SELECT intent_id,manager_id,owner_id,requested_username,display_name,launch_url,status,bot_id,created_at,expires_at FROM telegram_managed_bot_intents WHERE manager_id=$1 AND owner_id=$2 AND requested_username=$3 COLLATE NOCASE AND status='pending' AND expires_at>$4`, manager, owner, username, time.Now().UTC()).Scan(&in.ID, &in.ManagerID, &in.OwnerID, &in.RequestedUsername, &in.DisplayName, &in.LaunchURL, &in.Status, &in.BotID, &in.CreatedAt, &in.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrTelegramBotRecordNotFound
	}
	return in, err
}

// TelegramManagedBotActivationPending identifies a claimed creation by verified
// numeric bot identity, manager and owner. Completed creations and disabled bots
// cannot trigger default profile changes on later token/ownership updates.
func (s *Store) TelegramManagedBotActivationPending(ctx context.Context, manager string, owner, telegramID int64) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(
        SELECT 1 FROM telegram_managed_bot_intents intent JOIN telegram_bots bot ON bot.bot_id=intent.bot_id
        WHERE intent.status='activating' AND intent.manager_id=$1 AND intent.owner_id=$2
          AND bot.managed_by=$1 AND bot.owner_id=$2 AND bot.telegram_id=$3 AND bot.enabled=1 AND bot.legacy=0)`, manager, owner, telegramID).Scan(&pending)
	return pending, err
}

func (s *Store) TelegramManagedBotUpdateSeen(ctx context.Context, manager string, update int64) (bool, error) {
	var seen bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_managed_bot_updates WHERE manager_id=$1 AND update_id=$2)`, manager, update).Scan(&seen)
	return seen, err
}

// CompleteTelegramManagedBotUpdate commits completion and replay protection.
// Creation is separately claimed before activation. Ignored updates get receipts,
// so a replay cannot consume an intent created later.
func (s *Store) CompleteTelegramManagedBotUpdate(ctx context.Context, manager string, update int64, bot string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	connected := false
	if bot != "" {
		tag, err := tx.Exec(ctx, `UPDATE telegram_managed_bot_intents SET status='connected' WHERE manager_id=$1 AND bot_id=$2 AND status='activating' AND EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=$2 AND enabled=1)`, manager, bot)
		if err != nil {
			return err
		}
		connected = tag.RowsAffected() > 0
	}
	if _, err = tx.Exec(ctx, `INSERT INTO telegram_managed_bot_updates(manager_id,update_id,received_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, manager, update, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if connected {
		s.notifySessionActivity()
	}
	return nil
}
