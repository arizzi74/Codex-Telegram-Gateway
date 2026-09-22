package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// PickerCleanup deletes one message from an accepted session selection step.
// It uses the ordinary durable delivery queue, including retries and leases.
type PickerCleanup struct {
	MessageID        int64  `json:"message_id"`
	OriginDeliveryID string `json:"origin_delivery_id,omitempty"`
}

var ErrTelegramPickerCleanupPending = errors.New("registry: previous Telegram selection cleanup is pending")

// New selection messages wait for both recorded deletions and a source send
// already underway. Its bounded lease allows recovery after a sender crash;
// any late checkpoint independently queues another deletion.
const pendingPickerCleanupSQL = `delivery.kind='ui_response'
    AND COALESCE(json_extract(delivery.payload,'$.previous_picker_id'),'')<>''
    AND (EXISTS (
        SELECT 1 FROM telegram_deliveries cleanup
        WHERE cleanup.kind='picker_cleanup' AND cleanup.status IN ('pending','failed','sending')
          AND cleanup.bot_id=delivery.bot_id AND cleanup.chat_id=delivery.chat_id
          AND cleanup.message_thread_id=delivery.message_thread_id
          AND (cleanup.delivery_id=json_extract(delivery.payload,'$.previous_picker_id')
            OR json_extract(cleanup.payload,'$.origin_delivery_id')=json_extract(delivery.payload,'$.previous_picker_id'))
    ) OR EXISTS (
        SELECT 1 FROM telegram_deliveries source
        WHERE source.delivery_id=json_extract(delivery.payload,'$.previous_picker_id')
          AND source.bot_id=delivery.bot_id AND source.chat_id=delivery.chat_id
          AND source.message_thread_id=delivery.message_thread_id
          AND source.visibility_revoked=1 AND source.status='sending' AND source.next_attempt_at>` + sqliteNow + `
    ))`

func pickerViewAllowed(view, sourceAction, action string) bool {
	switch view {
	case "sessions", "instances":
		return action == "sessions" || action == "select" || action == "connect" || action == "new"
	case "runtime_picker":
		return action == "sessions" && sourceAction == "sessions"
	}
	return false
}

// Sibling buttons are retired together. Check before any binding or wizard
// mutation so a second queued tap cannot change selection from an old menu.
func validatePickerCallback(ctx context.Context, tx *dbTx, in IncomingUpdate, callback callbackPayload, action string) error {
	if callback.OriginDeliveryID == "" || (action != "sessions" && action != "select" && action != "connect" && action != "new") {
		return nil
	}
	var view, sourceAction string
	var revoked bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(json_extract(payload,'$.view'),''),COALESCE(json_extract(payload,'$.action'),''),visibility_revoked
        FROM telegram_deliveries WHERE delivery_id=$1 AND bot_id=$2 AND chat_id=$3 AND message_thread_id=$4 AND kind='ui_response'`, callback.OriginDeliveryID, in.BotID, in.ChatID, in.TopicID).Scan(&view, &sourceAction, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if revoked && pickerViewAllowed(view, sourceAction, action) {
		return ErrCallbackInvalid
	}
	return nil
}

func retirePickerForCallback(ctx context.Context, tx *dbTx, in IncomingUpdate, callback callbackPayload, action string, result AcceptResult) (AcceptResult, error) {
	if result.ErrorCode != "" || (result.View != "sessions" && result.View != "selected" && result.View != "new_session_name") {
		return result, nil
	}
	if callback.OriginDeliveryID == "" {
		// Legacy callback tokens predate source delivery tracking. Telegram has
		// authenticated the exact clicked message; never infer a broader scope.
		if in.CallbackMessageID > 0 {
			id, err := enqueuePickerCleanup(ctx, tx, in.BotID, in.ChatID, in.TopicID, "", in.CallbackMessageID)
			result.PreviousPickerID = id
			return result, err
		}
		return result, nil
	}
	var view, sourceAction string
	var messageID int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(json_extract(payload,'$.view'),''),COALESCE(json_extract(payload,'$.action'),''),COALESCE(telegram_message_id,0)
        FROM telegram_deliveries WHERE delivery_id=$1 AND bot_id=$2 AND chat_id=$3 AND message_thread_id=$4 AND kind='ui_response'`, callback.OriginDeliveryID, in.BotID, in.ChatID, in.TopicID).Scan(&view, &sourceAction, &messageID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !pickerViewAllowed(view, sourceAction, action)) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries SET visibility_revoked=1 WHERE delivery_id=$1`, callback.OriginDeliveryID); err != nil {
		return result, err
	}
	messageIDs := map[int64]bool{}
	if messageID > 0 {
		messageIDs[messageID] = true
	}
	// The button can be clicked before the API send has been checkpointed.
	if in.CallbackMessageID > 0 {
		messageIDs[in.CallbackMessageID] = true
	}
	rows, err := tx.Query(ctx, `SELECT telegram_message_id FROM telegram_delivery_chunks WHERE delivery_id=$1 AND status='sent' AND telegram_message_id>0`, callback.OriginDeliveryID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		messageIDs[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for id := range messageIDs {
		if _, err := enqueuePickerCleanup(ctx, tx, in.BotID, in.ChatID, in.TopicID, callback.OriginDeliveryID, id); err != nil {
			return result, err
		}
	}
	result.PreviousPickerID = callback.OriginDeliveryID
	return result, nil
}

func enqueuePickerCleanup(ctx context.Context, tx *dbTx, bot string, chat, topic int64, origin string, message int64) (string, error) {
	key, err := json.Marshal([]any{bot, chat, topic, origin, message})
	if err != nil {
		return "", err
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, append([]byte("telegram-picker-cleanup:"), key...)).String()
	payload, err := json.Marshal(PickerCleanup{MessageID: message, OriginDeliveryID: origin})
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,message_thread_id,kind,payload)
        VALUES($1,$2,$3,$4,'picker_cleanup',$5) ON CONFLICT(delivery_id) DO NOTHING`, id, bot, chat, topic, string(payload))
	return id, err
}

func checkpointPickerCleanup(ctx context.Context, tx *dbTx, deliveryID uuid.UUID, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	var bot string
	var chat, topic int64
	err := tx.QueryRow(ctx, `SELECT bot_id,chat_id,message_thread_id FROM telegram_deliveries
        WHERE delivery_id=$1 AND visibility_revoked=1 AND kind='ui_response'
          AND (json_extract(payload,'$.view') IN ('sessions','instances') OR (json_extract(payload,'$.view')='runtime_picker' AND json_extract(payload,'$.action')='sessions'))`, deliveryID).Scan(&bot, &chat, &topic)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registry: read retired picker checkpoint: %w", err)
	}
	_, err = enqueuePickerCleanup(ctx, tx, bot, chat, topic, deliveryID.String(), messageID)
	return err
}
