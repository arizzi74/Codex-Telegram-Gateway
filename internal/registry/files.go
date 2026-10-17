package registry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// PrepareTelegramFile authorizes a document and freezes its destination before
// downloading. Its actual content determines whether the image or file
// capability is required when the downloaded payload is accepted.
func (s *Store) PrepareTelegramFile(ctx context.Context, in IncomingUpdate) (IncomingUpdate, error) {
	if err := validateIncoming(in); err != nil {
		return IncomingUpdate{}, err
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return IncomingUpdate{}, fmt.Errorf("registry: begin file routing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reject := func(code string) (IncomingUpdate, error) {
		in.Action, in.MediaError, in.Files, in.Images, in.fileTarget = "media_error", code, nil, nil, nil
		return in, nil
	}
	allowed, err := telegramMediaAuthorized(ctx, tx, in)
	if err != nil {
		return IncomingUpdate{}, err
	}
	if !allowed {
		return reject("file_unauthorized")
	}
	target, err := resolveRoute(ctx, tx, in)
	if err != nil {
		if deterministicTelegramError(err) {
			return reject(telegramErrorCode(err))
		}
		return IncomingUpdate{}, err
	}
	files, err := workerSupportsFileInput(ctx, tx, target.workerID)
	if err != nil {
		return IncomingUpdate{}, err
	}
	images, err := workerSupportsImageInput(ctx, tx, target.workerID)
	if err != nil {
		return IncomingUpdate{}, err
	}
	if !files && !images {
		return reject("file_worker_upgrade")
	}
	in.fileTarget = &target
	return in, nil
}

func workerSupportsFileInput(ctx context.Context, tx *dbTx, workerID uuid.UUID) (bool, error) {
	var metadata []byte
	if err := tx.QueryRow(ctx, "SELECT heartbeat_metadata FROM workers WHERE worker_id=$1", workerID).Scan(&metadata); err != nil {
		return false, fmt.Errorf("registry: read file capability: %w", err)
	}
	var capability struct {
		SupportsFileInput bool `json:"supports_file_input"`
	}
	return json.Unmarshal(metadata, &capability) == nil && capability.SupportsFileInput, nil
}

// Webhook configuration checks the source owner and chat before entering the
// registry. Check persisted policy as well to close bot or ACL changes during
// an in-flight webhook. Missing legacy bot records retain existing semantics.
func telegramMediaAuthorized(ctx context.Context, tx *dbTx, in IncomingUpdate) (bool, error) {
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT
        NOT EXISTS(SELECT 1 FROM telegram_bot_retirements WHERE bot_id=$1)
        AND NOT EXISTS(SELECT 1 FROM telegram_bots WHERE bot_id=$1 AND (enabled=0 OR owner_id<>$2))
        AND `+telegramChatAllowedSQL("$1", "$3"), in.BotID, in.UserID, in.ChatID).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("registry: authorize Telegram media: %w", err)
	}
	return allowed, nil
}
