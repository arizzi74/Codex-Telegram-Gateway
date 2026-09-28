package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// UsageRequester preserves the requesting user's scope through usage and reset menus.
func (s *Store) UsageRequester(ctx context.Context, commandID uuid.UUID) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx, `SELECT telegram_user_id FROM commands
        WHERE command_id=$1 AND source='telegram' AND operation='codex_command'
          AND json_extract(payload, '$.arguments.codex.name')='usage'
          AND telegram_user_id IS NOT NULL`, commandID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && userID == 0) {
		return 0, ErrTelegramTarget
	}
	if err != nil {
		return 0, fmt.Errorf("registry: read usage requester: %w", err)
	}
	return userID, nil
}

func consumeUsageCallback(ctx context.Context, tx *dbTx, in IncomingUpdate, callback callbackPayload, sessionID *uuid.UUID) (AcceptResult, error) {
	if sessionID == nil || *sessionID == uuid.Nil || callback.RuntimeID == "" || callback.Generation <= 0 || !protocol.ValidUsageMenuArgs(callback.Decision) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	target, err := sessionRoute(ctx, tx, *sessionID)
	if err != nil {
		return AcceptResult{}, callbackTargetError(err)
	}
	if !callbackMatchesTarget(callback, target) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err := validateSettingsCallbackOrigin(ctx, tx, "usage", callback.QuestionID, *sessionID, callback.RuntimeID, callback.Generation, in.BotID, in.UserID, in.ChatID, in.TopicID); err != nil {
		return AcceptResult{}, err
	}
	if err := validateUsageMenuChoice(ctx, tx, callback.QuestionID, callback.Decision); err != nil {
		return AcceptResult{}, err
	}
	result, err := acceptRoutedCodexCommand(ctx, tx, in, target, "usage", callback.Decision)
	if err != nil {
		return AcceptResult{}, err
	}
	// Claim every button in this step atomically. The next step is tied to the
	// newly queued command, so competing confirm/cancel taps cannot both proceed.
	used, err := tx.Exec(ctx, `UPDATE telegram_callbacks SET used_at=(strftime('%Y-%m-%dT%H:%M:%f','now') || '000000Z')
        WHERE action='usage' AND used_at IS NULL
          AND json_extract(payload, '$.question_id')=$1`, callback.QuestionID)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: consume usage menu: %w", err)
	}
	if used.RowsAffected() == 0 {
		return AcceptResult{}, ErrCallbackInvalid
	}
	return result, nil
}

// A usage button may only open its advertised next step. Only an
// option actually offered by that step may queue a reset confirmation, even if
// a trusted caller accidentally constructs a different callback descriptor.
func validateUsageMenuChoice(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) *dbRow
}, commandID, args string) error {
	var valid bool
	err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands c
        JOIN events e ON e.worker_id=c.worker_id AND e.runtime_id=c.runtime_id
          AND e.runtime_generation=c.runtime_generation AND e.session_id=c.session_id
        JOIN json_each(e.payload, '$.usage_menu.options') choice
        WHERE c.command_id=$1 AND c.status='completed' AND e.kind='command_completed'
          AND json_extract(e.payload, '$.command_id')=c.command_id
          AND json_extract(choice.value, '$.args')=$2)`, commandID, args).Scan(&valid)
	if err != nil {
		return fmt.Errorf("registry: validate offered usage choice: %w", err)
	}
	if !valid {
		return ErrCallbackInvalid
	}
	return nil
}
