package protocol

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// WebUIFrame is an ephemeral, session-bound app-server relay. Frames are never
// persisted in the command outbox or replayed following a reconnect.
type WebUIFrame struct {
	ID                string          `json:"id"`
	Action            string          `json:"action"`
	RuntimeID         string          `json:"runtime_id,omitempty"`
	RuntimeGeneration uint64          `json:"runtime_generation,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	Data              json.RawMessage `json:"data,omitempty"`
	Error             string          `json:"error,omitempty"`
}

// One maximum image expands to ~13.4 MiB of base64. Reserve space for
// text and both relay/envelope wrappers beneath the 16 MiB transport cap.
const MaxWebUIInputBytes = 15 << 20

func (f WebUIFrame) Validate() error {
	if _, err := uuid.Parse(f.ID); err != nil {
		return errors.New("invalid web UI connection identity")
	}
	switch f.Action {
	case "open":
		if _, err := uuid.Parse(f.RuntimeID); err != nil {
			return errors.New("invalid web UI runtime identity")
		}
		if _, err := uuid.Parse(f.SessionID); err != nil || f.RuntimeGeneration == 0 {
			return errors.New("invalid web UI session target")
		}
	case "input":
		if len(f.Data) == 0 || len(f.Data) > MaxWebUIInputBytes || !json.Valid(f.Data) {
			return errors.New("invalid or oversized web UI input")
		}
	case "close", "ready", "output", "error":
	default:
		return errors.New("unknown web UI action")
	}
	return nil
}
