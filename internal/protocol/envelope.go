// Package protocol defines the versioned worker/gateway protocol. It contains no
// Telegram types or Codex JSON-RPC wire types.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const Version = 1
const MaxFrameBytes = 16 << 20

type Envelope struct {
	Version   int             `json:"version"`
	Type      string          `json:"type"`
	MessageID string          `json:"message_id"`
	SentAt    time.Time       `json:"sent_at"`
	Payload   json.RawMessage `json:"payload"`
}

func NewEnvelope(kind string, payload any) (Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	e := Envelope{Version: Version, Type: kind, MessageID: uuid.NewString(), SentAt: time.Now().UTC(), Payload: body}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	frame, err := json.Marshal(e)
	if err != nil {
		return Envelope{}, err
	}
	if len(frame) > MaxFrameBytes {
		return Envelope{}, errors.New("frame too large")
	}
	return e, nil
}

func (e Envelope) Validate() error {
	if e.Version != Version {
		return &Error{Code: UnsupportedProtocol, Message: "Unsupported worker protocol version."}
	}
	switch e.Type {
	case "hello", "hello_ack", "heartbeat", "command", "command_ack", "worker_update_request", "worker_event", "event_ack", "error", "webui":
	default:
		return fmt.Errorf("unknown envelope type %q", e.Type)
	}
	if _, err := uuid.Parse(e.MessageID); err != nil {
		return errors.New("invalid message_id")
	}
	if e.SentAt.IsZero() {
		return errors.New("missing sent_at")
	}
	if len(e.Payload) == 0 || len(e.Payload) > MaxFrameBytes || !json.Valid(e.Payload) || e.Payload[0] != '{' {
		return errors.New("payload must be a bounded JSON object")
	}
	return nil
}

func Decode(frame []byte) (Envelope, error) {
	var e Envelope
	if len(frame) > MaxFrameBytes {
		return e, errors.New("frame too large")
	}
	if err := json.Unmarshal(frame, &e); err != nil {
		return e, err
	}
	return e, e.Validate()
}

func Payload[T any](e Envelope) (T, error) {
	var p T
	err := json.Unmarshal(e.Payload, &p)
	return p, err
}
