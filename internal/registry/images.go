package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// PrepareTelegramImage freezes the authorized destination before network I/O
// downloads an image. The returned value must be preserved when attaching its
// downloaded bytes; a later selection change cannot reroute this update.
func (s *Store) PrepareTelegramImage(ctx context.Context, in IncomingUpdate) (IncomingUpdate, error) {
	if err := validateIncoming(in); err != nil {
		return IncomingUpdate{}, err
	}
	tx, err := s.pool.BeginTx(ctx, TxOptions{})
	if err != nil {
		return IncomingUpdate{}, fmt.Errorf("registry: begin image routing: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reject := func(code string) (IncomingUpdate, error) {
		in.Action, in.MediaError, in.Images, in.imageTarget = "media_error", code, nil, nil
		return in, nil
	}
	if in.ReplyToMessageID > 0 {
		var approvalReply bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bot_message_routes
            WHERE bot_id=$1 AND chat_id=$2 AND message_id=$3 AND approval_id IS NOT NULL)`,
			in.BotID, in.ChatID, in.ReplyToMessageID).Scan(&approvalReply); err != nil {
			return IncomingUpdate{}, fmt.Errorf("registry: inspect image reply target: %w", err)
		}
		if approvalReply {
			return reject("image_input_reply")
		}
	}
	target, err := resolveRoute(ctx, tx, in)
	if err != nil {
		if deterministicTelegramError(err) {
			return reject(telegramErrorCode(err))
		}
		return IncomingUpdate{}, err
	}
	supported, err := workerSupportsImageInput(ctx, tx, target.workerID)
	if err != nil {
		return IncomingUpdate{}, err
	}
	if !supported {
		return reject("image_worker_upgrade")
	}
	in.imageTarget = &target
	return in, nil
}

func resolveImageOrCurrentRoute(ctx context.Context, tx *dbTx, in IncomingUpdate) (routeTarget, error) {
	if in.imageTarget == nil {
		return resolveRoute(ctx, tx, in)
	}
	frozen := *in.imageTarget
	current, err := sessionRoute(ctx, tx, frozen.sessionID)
	if err != nil {
		return routeTarget{}, err
	}
	if current.workerID != frozen.workerID || current.runtimeID != frozen.runtimeID || current.generation != frozen.generation || current.threadID != frozen.threadID {
		return routeTarget{}, ErrTelegramTarget
	}
	if strings.EqualFold(strings.TrimSpace(in.Action), "steer") && current.activeTurnID != frozen.activeTurnID {
		return routeTarget{}, ErrStaleTurn
	}
	return frozen, nil
}

func workerSupportsImageInput(ctx context.Context, tx *dbTx, workerID uuid.UUID) (bool, error) {
	var metadata []byte
	if err := tx.QueryRow(ctx, "SELECT heartbeat_metadata FROM workers WHERE worker_id=$1", workerID).Scan(&metadata); err != nil {
		return false, fmt.Errorf("registry: read image capability: %w", err)
	}
	var capability struct {
		SupportsImageInput bool `json:"supports_image_input"`
	}
	return json.Unmarshal(metadata, &capability) == nil && capability.SupportsImageInput, nil
}

// mediaErrorResult accepts only stable renderer codes. Download errors can
// contain credentials or file URLs and must never be stored as user feedback.
func mediaErrorResult(code string) AcceptResult {
	switch code {
	case "image_too_large", "image_download_failed", "image_unsupported", "image_worker_upgrade", "image_input_reply", "target_unavailable":
	default:
		code = "image_unsupported"
	}
	return AcceptResult{View: "error", ErrorCode: code}
}
