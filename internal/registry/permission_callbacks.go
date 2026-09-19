package registry

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

func validPermissionDecision(decision string) bool {
	return strings.TrimSpace(decision) != "" && len(decision) <= 256 && utf8.ValidString(decision) && strings.IndexFunc(decision, unicode.IsControl) < 0
}

// A menu is tied to its requesting command rather than to a mutable Telegram
// selection. Even trusted callers cannot construct a picker for another user
// or session using a command ID they did not originate.
func validatePermissionCallbackOrigin(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, commandID string, sessionID uuid.UUID, runtimeID string, generation int64, botID string, userID, chatID, topicID int64) error {
	id, err := uuid.Parse(commandID)
	if err != nil || id == uuid.Nil {
		return ErrCallbackInvalid
	}
	var valid bool
	err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands
        WHERE command_id=$1 AND source='telegram' AND operation='codex_command'
          AND json_extract(payload, '$.arguments.codex.name')='permissions'
          AND session_id=$2 AND runtime_id=$3 AND runtime_generation=$4
          AND telegram_bot_id=$5 AND telegram_user_id=$6 AND telegram_chat_id=$7
          AND telegram_message_thread_id=$8)`, id, sessionID, runtimeID, generation, botID, userID, chatID, topicID).Scan(&valid)
	if err != nil {
		return fmt.Errorf("registry: validate permissions callback origin: %w", err)
	}
	if !valid {
		return ErrCallbackInvalid
	}
	return nil
}

func consumePermissionsCallback(ctx context.Context, tx *dbTx, in IncomingUpdate, callback callbackPayload, sessionID *uuid.UUID) (AcceptResult, error) {
	if sessionID == nil || *sessionID == uuid.Nil || callback.RuntimeID == "" || callback.Generation <= 0 || !validPermissionDecision(callback.Decision) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	target, err := sessionRoute(ctx, tx, *sessionID)
	if err != nil {
		return AcceptResult{}, callbackTargetError(err)
	}
	if !callbackMatchesTarget(callback, target) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err := validatePermissionCallbackOrigin(ctx, tx, callback.QuestionID, *sessionID, callback.RuntimeID, callback.Generation, in.BotID, in.UserID, in.ChatID, in.TopicID); err != nil {
		return AcceptResult{}, err
	}
	// The worker applies the selected preset only after checking that the
	// session is idle and that the option is still allowed by runtime policy.
	result, err := acceptRoutedCodexCommand(ctx, tx, in, target, "permissions", callback.Decision)
	if err != nil {
		return AcceptResult{}, err
	}
	// Claim the whole menu atomically, so tapping a second choice cannot queue
	// competing permission changes. Confirmation menus have a new command ID.
	used, err := tx.Exec(ctx, `UPDATE telegram_callbacks SET used_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE action='permissions' AND used_at IS NULL
          AND json_extract(payload, '$.question_id')=$1`, callback.QuestionID)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: consume permissions menu: %w", err)
	}
	if used.RowsAffected() == 0 {
		return AcceptResult{}, ErrCallbackInvalid
	}
	return result, nil
}
