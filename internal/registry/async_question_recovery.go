package registry

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// recoverAsyncQuestionResponse makes a definitely unsuccessful response
// answerable again. The submitted answers remain in the immutable command;
// resetting the draft lets the pending menu ask for a fresh response.
// An unknown outcome or an expired command that was dispatched may already
// have reached Codex, so those claims must remain fenced against duplicate input.
func recoverAsyncQuestionResponse(ctx context.Context, tx *dbTx, commandID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state='cleared',resolved_at=`+sqliteNow+`,
            resolution=json_object('command_id',$1,'state','cleared')
        WHERE response_command_id=$1 AND state='pending' AND json_extract(request_payload,'$.async')=1
          AND EXISTS(SELECT 1 FROM commands command WHERE command.command_id=$1
            AND command.operation='input_response' AND command.status='failed' AND command.error_code=$2
            AND command.worker_id=approvals.worker_id AND command.runtime_id=approvals.runtime_id
            AND command.runtime_generation=approvals.runtime_generation AND command.session_id=approvals.session_id)`,
		commandID, protocol.ApprovalNotPending); err != nil {
		return fmt.Errorf("registry: clear unavailable async question: %w", err)
	}
	_, err := tx.Exec(ctx, `UPDATE approvals SET response_command_id=NULL,input_answers='{}'
        WHERE response_command_id=$1 AND state='pending' AND json_extract(request_payload,'$.async')=1
          AND EXISTS(SELECT 1 FROM commands command WHERE command.command_id=$1
            AND command.operation='input_response'
            AND command.worker_id=approvals.worker_id AND command.runtime_id=approvals.runtime_id
            AND command.runtime_generation=approvals.runtime_generation AND command.session_id=approvals.session_id
            AND ((command.status='failed' AND COALESCE(command.error_code,'')<>$2)
              OR (command.status='expired' AND command.dispatched_at IS NULL AND command.acknowledged_at IS NULL)))`,
		commandID, protocol.OutcomeUnknown)
	if err != nil {
		return fmt.Errorf("registry: recover async question response: %w", err)
	}
	return nil
}
