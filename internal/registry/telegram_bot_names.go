package registry

import (
	"context"
	"database/sql"
	"errors"
)

// TelegramBotNamePreference is separate from TelegramBotRecord because identity
// probes and credential refreshes must never replace the original default name.
// Nil names mean that the background synchronizer has not captured/applied one.
type TelegramBotNamePreference struct {
	BotID               string
	InheritSessionName  bool
	OriginalDisplayName *string
	AppliedDisplayName  *string
}

// Imported managed bots can lack the manager identity. Their same-owner
// connected creation still supplies the original name; a known manager scopes
// which creation records are authoritative. Share this selection with the WebUI
// so profile synchronization and the selector use the same creation name.
func telegramBotCreationNameSQL(bot string) string {
	return `(SELECT intent.display_name FROM telegram_managed_bot_intents intent
        WHERE intent.bot_id=` + bot + `.bot_id AND intent.owner_id=` + bot + `.owner_id
          AND (` + bot + `.managed_by='' OR intent.manager_id=` + bot + `.managed_by)
          AND intent.status='connected'
        ORDER BY intent.created_at DESC,intent.intent_id DESC LIMIT 1)`
}

// TelegramBotCreationName returns the authoritative managed creation name for
// this bot and its configured owner. A missing bot, owner mismatch or absent
// completed intent returns nil. Captured name preferences are never changed.
func (s *Store) TelegramBotCreationName(ctx context.Context, id string, owner int64) (*string, error) {
	var name sql.NullString
	err := s.pool.QueryRow(ctx, `SELECT `+telegramBotCreationNameSQL("bot")+`
        FROM telegram_bots bot WHERE bot.bot_id=$1 AND bot.owner_id=$2`, id, owner).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !name.Valid {
		return nil, nil
	}
	return &name.String, nil
}

func (s *Store) TelegramBotNamePreferences(ctx context.Context) ([]TelegramBotNamePreference, error) {
	rows, err := s.pool.Query(ctx, `SELECT bot_id,inherit_session_name,original_display_name,applied_display_name FROM telegram_bot_names ORDER BY bot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TelegramBotNamePreference{}
	for rows.Next() {
		var p TelegramBotNamePreference
		var original, applied sql.NullString
		if err := rows.Scan(&p.BotID, &p.InheritSessionName, &original, &applied); err != nil {
			return nil, err
		}
		if original.Valid {
			p.OriginalDisplayName = &original.String
		}
		if applied.Valid {
			p.AppliedDisplayName = &applied.String
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SetTelegramBotSessionName(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO telegram_bot_names(bot_id,inherit_session_name)
        SELECT bot_id,$2 FROM telegram_bots WHERE bot_id=$1
        ON CONFLICT(bot_id) DO UPDATE SET inherit_session_name=excluded.inherit_session_name`, id, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTelegramBotRecordNotFound
	}
	return nil
}

// CaptureTelegramBotOriginalName commits the original before any remote rename.
// It is write-once, including when the original is the empty default name.
func (s *Store) CaptureTelegramBotOriginalName(ctx context.Context, id, name string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_bot_names
        SET original_display_name=$2,applied_display_name=$2
        WHERE bot_id=$1 AND original_display_name IS NULL`, id, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTelegramBotRecordNotFound
	}
	s.notifySessionActivity()
	return nil
}

func (s *Store) SetTelegramBotAppliedName(ctx context.Context, id, name string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_bot_names SET applied_display_name=$2
        WHERE bot_id=$1 AND original_display_name IS NOT NULL`, id, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTelegramBotRecordNotFound
	}
	return nil
}

// MarkTelegramBotNamePending fences an uncertain API outcome durably. If the
// process stops after Telegram accepts a name but before its checkpoint, a
// subsequent flag-off must still restore the original rather than assume it is
// already applied.
func (s *Store) MarkTelegramBotNamePending(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE telegram_bot_names SET applied_display_name=NULL
        WHERE bot_id=$1 AND original_display_name IS NOT NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTelegramBotRecordNotFound
	}
	return nil
}

// TelegramBotOwnerSessionName considers only the configured owner's private
// selection. Group, forum, other-user and other-bot selections cannot rename a
// global bot profile, and revoked chats/sessions/workers stop contributing.
func (s *Store) TelegramBotOwnerSessionName(ctx context.Context, id string, owner int64) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(session.name,'')
        FROM telegram_bindings binding
        JOIN telegram_bots bot ON bot.bot_id=binding.bot_id AND bot.owner_id=$2 AND bot.enabled=1
        JOIN sessions session ON session.session_id=binding.session_id AND session.archived=0
        JOIN workers worker ON worker.worker_id=session.worker_id AND worker.enabled=1
        WHERE binding.bot_id=$1 AND binding.user_id=$2 AND binding.chat_id=$2 AND binding.message_thread_id=0
          AND `+telegramChatAllowedSQL("binding.bot_id", "binding.chat_id"), id, owner).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}
