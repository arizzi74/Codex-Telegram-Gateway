package registry

import "errors"

// ErrTelegramProgressOrderPending defers a fresh tool message until the
// commentary restored beside it has been delivered. It is a retry, not a
// cancellation: the commentary can be temporarily rate limited by Telegram.
var ErrTelegramProgressOrderPending = errors.New("registry: Telegram commentary delivery is pending")

// Claim order and INSERT order cannot order separate senders, or a retry after
// a crash. Establish the commentary slot durably before the first tool slot.
// Once either slot is visible, edits remain independent. In particular, a
// tool-only turn need not wait for commentary that Codex has never produced.
// The queue index bounds the pending lookup; the checkpoint lookup uses the
// existing runtime/generation/session/turn scope index, not completed history.
const pendingProgressOrderSQL = `(delivery.kind='tool_progress_message' AND EXISTS (
    SELECT 1 FROM events tool_event
    WHERE tool_event.event_id=delivery.event_id
      AND NOT EXISTS (
        SELECT 1 FROM telegram_progress_messages shown
        JOIN telegram_deliveries previous ON previous.delivery_id=shown.delivery_id
        WHERE shown.runtime_id=tool_event.runtime_id
          AND shown.runtime_generation=tool_event.runtime_generation
          AND shown.session_id=tool_event.session_id
          AND shown.turn_id=json_extract(tool_event.payload,'$.turn_id')
          AND shown.bot_id=delivery.bot_id AND shown.chat_id=delivery.chat_id
          AND shown.message_thread_id=delivery.message_thread_id
          AND shown.status='pending' AND shown.retire_requested=0
          AND previous.visibility_revoked=0 AND previous.status='sent'
          AND previous.kind IN ('agent_progress_message','tool_progress_message')
      )
      AND EXISTS (
        SELECT 1 FROM telegram_deliveries commentary
        CROSS JOIN events commentary_event ON commentary_event.event_id=commentary.event_id
        WHERE commentary.status IN ('pending','failed','sending')
          AND commentary.kind='agent_progress_message' AND commentary.visibility_revoked=0
          AND commentary.bot_id=delivery.bot_id AND commentary.chat_id=delivery.chat_id
          AND commentary.message_thread_id=delivery.message_thread_id
          AND commentary_event.runtime_id=tool_event.runtime_id
          AND commentary_event.runtime_generation=tool_event.runtime_generation
          AND commentary_event.session_id=tool_event.session_id
          AND json_extract(commentary_event.payload,'$.turn_id')=json_extract(tool_event.payload,'$.turn_id')
          AND NOT EXISTS (
            SELECT 1 FROM events newer
            JOIN telegram_deliveries replacement ON replacement.event_id=newer.event_id
            WHERE newer.runtime_id=commentary_event.runtime_id
              AND newer.runtime_generation=commentary_event.runtime_generation
              AND newer.session_id=commentary_event.session_id
              AND newer.event_seq>commentary_event.event_seq
              AND json_extract(newer.payload,'$.turn_id')=json_extract(commentary_event.payload,'$.turn_id')
              AND replacement.kind='agent_progress_message'
              AND replacement.bot_id=commentary.bot_id AND replacement.chat_id=commentary.chat_id
              AND replacement.message_thread_id=commentary.message_thread_id
          )
      )
))`
