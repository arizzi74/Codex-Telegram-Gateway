package protocol

import (
	"encoding/json"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeRejectsInvalidFrames(t *testing.T) {
	e, err := NewEnvelope("hello", Hello{WorkerID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(e)
	if _, err = Decode(b); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, strings.Replace(string(b), `"version":1`, `"version":2`, 1), strings.Replace(string(b), `"type":"hello"`, `"type":"shell"`, 1), strings.Repeat(" ", MaxFrameBytes+1)} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatal("accepted invalid frame")
		}
	}
}

func TestCommandRequiresExactTarget(t *testing.T) {
	c := Command{ID: uuid.NewString(), WorkerID: uuid.NewString(), RuntimeID: uuid.NewString(), RuntimeGeneration: 1, SessionID: uuid.NewString(), ThreadID: "thread", Operation: Steer, ExpectedTurnID: "turn", Arguments: Arguments{Text: "hello"}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.ExpectedTurnID = ""
	if c.Validate() == nil {
		t.Fatal("accepted untargeted steer")
	}
	c.Operation = Interrupt
	if c.Validate() == nil {
		t.Fatal("accepted untargeted interrupt")
	}
	c.Operation = StartTurn
	c.RuntimeGeneration = 0
	if c.Validate() == nil {
		t.Fatal("accepted absent generation")
	}
}

func TestUnknownEventsAreDurable(t *testing.T) {
	if !(Event{Kind: "future_critical_event"}).Durable() {
		t.Fatal("unknown event dropped")
	}
	if (Event{Kind: "agent_message_delta"}).Durable() {
		t.Fatal("stream delta must be transient")
	}
}
