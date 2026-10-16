package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// WebUITelegramBot exposes only the original public name and the configured
// owner's authorized private selection. It never includes bot credentials.
type WebUITelegramBot struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	SessionID   string `json:"session_id"`
}

type WebUITelegramBotSnapshot struct {
	Revision string             `json:"revision"`
	Bots     []WebUITelegramBot `json:"bots"`
}

const webUITelegramBotLimit = 1000

const webUITelegramBotAvailableSQL = `bot.enabled=1 AND bot.owner_id<>0
    AND NOT EXISTS(SELECT 1 FROM telegram_bot_retirements retired WHERE retired.bot_id=bot.bot_id)
    AND `

// The owner's private binding is the session connection shown in the WebUI.
// Other users, groups and topics remain independent Telegram contexts.
func telegramOwnerPrivateBindingSQL(bot, binding string) string {
	return binding + `.user_id=` + bot + `.owner_id AND ` + binding + `.chat_id=` + bot + `.owner_id AND ` + binding + `.message_thread_id=0`
}

func readWebUITelegramBots(ctx context.Context, db interface {
	Query(context.Context, string, ...any) (*dbRows, error)
}) (WebUITelegramBotSnapshot, error) {
	// Imported managed bots can lack the manager identity. Their same-owner
	// connected creation still supplies the original name; a known manager
	// continues to scope which creation records are authoritative.
	rows, err := db.Query(ctx, `SELECT bot.bot_id,COALESCE((SELECT intent.display_name
            FROM telegram_managed_bot_intents intent WHERE intent.bot_id=bot.bot_id
              AND (bot.managed_by='' OR intent.manager_id=bot.managed_by) AND intent.owner_id=bot.owner_id
              AND intent.status='connected' ORDER BY intent.created_at DESC,intent.intent_id DESC LIMIT 1),
            names.original_display_name,bot.display_name),
        COALESCE((SELECT binding.session_id FROM telegram_bindings binding
            JOIN sessions session ON session.session_id=binding.session_id AND session.archived=0
            JOIN workers worker ON worker.worker_id=session.worker_id AND worker.enabled=1
            WHERE binding.bot_id=bot.bot_id AND `+telegramOwnerPrivateBindingSQL("bot", "binding")+`),'')
        FROM telegram_bots bot LEFT JOIN telegram_bot_names names ON names.bot_id=bot.bot_id
        WHERE `+webUITelegramBotAvailableSQL+telegramChatAllowedSQL("bot.bot_id", "bot.owner_id")+`
        ORDER BY bot.bot_id LIMIT $1`, webUITelegramBotLimit)
	if err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	defer rows.Close()
	snapshot := WebUITelegramBotSnapshot{Bots: make([]WebUITelegramBot, 0)}
	for rows.Next() {
		var bot WebUITelegramBot
		if err := rows.Scan(&bot.ID, &bot.DisplayName, &bot.SessionID); err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		snapshot.Bots = append(snapshot.Bots, bot)
	}
	if err := rows.Err(); err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	encoded, err := json.Marshal(snapshot.Bots)
	if err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	hash := sha256.Sum256(encoded)
	snapshot.Revision = hex.EncodeToString(hash[:])
	return snapshot, nil
}

// WebUITelegramBots returns bounded metadata for the chat selector. Offline
// enabled workers remain selectable, matching Telegram's /tgsessions routing.
func (s *Store) WebUITelegramBots(ctx context.Context) (WebUITelegramBotSnapshot, error) {
	return readWebUITelegramBots(ctx, s.pool)
}

// SetWebUITelegramBot moves this bot's exact owner-private selection atomically,
// replacing any other bot's owner-private selection of the destination session.
// An empty bot ID disconnects the owner-private bots currently on this session.
// Group, topic and other-user bindings, and frozen command routes, are separate.
// Unavailable sessions return ErrTelegramTarget; unavailable bots return
// ErrTelegramBotRecordNotFound.
func (s *Store) SetWebUITelegramBot(ctx context.Context, sessionID uuid.UUID, botID string) (WebUITelegramBotSnapshot, error) {
	if sessionID == uuid.Nil {
		return WebUITelegramBotSnapshot{}, ErrTelegramTarget
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := sessionRoute(ctx, tx, sessionID); err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	changed := false
	if botID != "" {
		var owner int64
		err := tx.QueryRow(ctx, `SELECT bot.owner_id FROM telegram_bots bot
            WHERE bot.bot_id=$1 AND `+webUITelegramBotAvailableSQL+telegramChatAllowedSQL("bot.bot_id", "bot.owner_id"), botID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return WebUITelegramBotSnapshot{}, ErrTelegramBotRecordNotFound
		}
		if err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		in := IncomingUpdate{BotID: botID, UserID: owner, ChatID: owner}
		if err := rememberTelegramContext(ctx, tx, in); err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		if err := retirePrivateTelegramSelection(ctx, tx, in); err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		if err := setBinding(ctx, tx, in, sessionID); err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		changed = true
	} else {
		rows, err := tx.Query(ctx, `SELECT binding.bot_id,binding.user_id FROM telegram_bindings binding
            JOIN telegram_bots bot ON bot.bot_id=binding.bot_id
            WHERE binding.session_id=$1 AND binding.user_id=bot.owner_id
              AND binding.chat_id=bot.owner_id AND binding.message_thread_id=0`, sessionID)
		if err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		var selections []IncomingUpdate
		for rows.Next() {
			var in IncomingUpdate
			if err := rows.Scan(&in.BotID, &in.UserID); err != nil {
				rows.Close()
				return WebUITelegramBotSnapshot{}, err
			}
			in.ChatID = in.UserID
			selections = append(selections, in)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return WebUITelegramBotSnapshot{}, err
		}
		for _, in := range selections {
			if err := disconnectPrivateTelegramSelection(ctx, tx, in); err != nil {
				return WebUITelegramBotSnapshot{}, err
			}
		}
		changed = len(selections) > 0
	}
	snapshot, err := readWebUITelegramBots(ctx, tx)
	if err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WebUITelegramBotSnapshot{}, err
	}
	if changed {
		s.notifySessionActivity()
	}
	return snapshot, nil
}

// Replacing a private selection supersedes interactive selection workflows. The
// accepted commands themselves keep their immutable target and are not cancelled.
func retirePrivateTelegramSelection(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	if err := retireSelectionConfirmations(ctx, tx, in, AcceptResult{View: "selected"}); err != nil {
		return err
	}
	for _, query := range []string{
		`UPDATE telegram_deliveries SET visibility_revoked=1 WHERE bot_id=$1 AND chat_id=$2
            AND message_thread_id=0 AND kind='ui_response' AND status IN ('pending','failed','sending')
            AND json_extract(payload,'$.wizard_id') IN (SELECT wizard_id FROM telegram_session_wizards
                WHERE bot_id=$1 AND user_id=$2 AND chat_id=$2 AND message_thread_id=0)`,
		`DELETE FROM telegram_callbacks WHERE json_extract(payload,'$.wizard_id') IN (
            SELECT wizard_id FROM telegram_session_wizards WHERE bot_id=$1 AND user_id=$2 AND chat_id=$2 AND message_thread_id=0)`,
		`DELETE FROM telegram_session_wizards WHERE bot_id=$1 AND user_id=$2 AND chat_id=$2 AND message_thread_id=0`,
	} {
		if _, err := tx.Exec(ctx, query, in.BotID, in.UserID); err != nil {
			return err
		}
	}
	return nil
}

func disconnectPrivateTelegramSelection(ctx context.Context, tx *dbTx, in IncomingUpdate) error {
	if err := retirePrivateTelegramSelection(ctx, tx, in); err != nil {
		return err
	}
	if err := bumpSelectionRevision(ctx, tx, in); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM telegram_bindings
        WHERE bot_id=$1 AND user_id=$2 AND chat_id=$2 AND message_thread_id=0`, in.BotID, in.UserID); err != nil {
		return err
	}
	return reconcileTelegramSelection(ctx, tx, in)
}

// Owner-private connections have one bot per session. Every other Telegram
// context retains its independent routing and question fanout.
func replacePrivateTelegramSessionBot(ctx context.Context, tx *dbTx, in IncomingUpdate, sessionID uuid.UUID) error {
	if in.UserID != in.ChatID || in.TopicID != 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT binding.bot_id,binding.user_id
        FROM telegram_bindings binding JOIN telegram_bots bot ON bot.bot_id=binding.bot_id
        WHERE binding.session_id=$1 AND binding.bot_id<>$2 AND binding.user_id=bot.owner_id
          AND binding.chat_id=bot.owner_id AND binding.message_thread_id=0
          AND EXISTS(SELECT 1 FROM telegram_bots chosen WHERE chosen.bot_id=$2 AND chosen.owner_id=$3)`,
		sessionID, in.BotID, in.UserID)
	if err != nil {
		return err
	}
	var selections []IncomingUpdate
	for rows.Next() {
		var previous IncomingUpdate
		if err := rows.Scan(&previous.BotID, &previous.UserID); err != nil {
			rows.Close()
			return err
		}
		previous.ChatID = previous.UserID
		selections = append(selections, previous)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, previous := range selections {
		if err := disconnectPrivateTelegramSelection(ctx, tx, previous); err != nil {
			return err
		}
	}
	return nil
}
