package protocol

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestWebUIEnvelopeAndImmutableTarget(t *testing.T) {
	frame := WebUIFrame{ID: uuid.NewString(), Action: "open", RuntimeID: uuid.NewString(), RuntimeGeneration: 2, SessionID: uuid.NewString()}
	if err := frame.Validate(); err != nil {
		t.Fatal(err)
	}
	envelope, err := NewEnvelope("webui", frame)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(envelope)
	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Payload[WebUIFrame](decoded)
	if err != nil || got.ID != frame.ID || got.SessionID != frame.SessionID || got.RuntimeGeneration != 2 {
		t.Fatalf("frame roundtrip = %#v,%v", got, err)
	}
	frame.RuntimeGeneration = 0
	if err := frame.Validate(); err == nil {
		t.Fatal("unfenced runtime allowed")
	}
	frame.Action = "input"
	frame.Data = json.RawMessage(`{"id":1,"method":"thread/read"}`)
	if err := frame.Validate(); err != nil {
		t.Fatal(err)
	}
	frame.Data = json.RawMessage(`not json`)
	if err := frame.Validate(); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	frame.Action = "unknown"
	if err := frame.Validate(); err == nil {
		t.Fatal("unknown frame accepted")
	}
}
