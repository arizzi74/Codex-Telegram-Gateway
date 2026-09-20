package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Reorder only the delivery copy: the durable worker event must retain its
// original payload so a retry of that event passes replay verification.
func historyDeliveryEvent(ctx context.Context, tx *dbTx, event protocol.Event, commandID *uuid.UUID) (protocol.Event, error) {
	if event.Kind != "command_completed" || commandID == nil {
		return event, nil
	}
	var result protocol.Result
	if json.Unmarshal(event.Data, &result) != nil || result.History == nil || !result.History.Conversation || result.History.NewestFirst {
		return event, nil
	}
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM commands WHERE command_id=$1 AND operation='read_history'`, *commandID).Scan(&payload); err != nil {
		return event, fmt.Errorf("registry: read history delivery request: %w", err)
	}
	var command protocol.Command
	if json.Unmarshal(payload, &command) != nil || command.Arguments.History.Validate() != nil {
		return event, ErrEventTarget
	}
	if !command.Arguments.History.NewestFirst {
		return event, nil
	}
	slices.Reverse(result.History.Messages)
	result.History.NewestFirst = true
	data, err := json.Marshal(result)
	if err != nil {
		return event, fmt.Errorf("registry: encode history delivery: %w", err)
	}
	event.Data = data
	return event, nil
}
