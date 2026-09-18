package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func imageTestCommand() Command {
	return Command{ID: uuid.NewString(), WorkerID: uuid.NewString(), RuntimeID: uuid.NewString(), RuntimeGeneration: 1,
		SessionID: uuid.NewString(), ThreadID: "thread", Operation: StartTurn,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
}

func TestImageInputValidation(t *testing.T) {
	png := Image{MIMEType: "image/png", Data: []byte("\x89PNG\r\n\x1a\n")}
	for _, tc := range []struct {
		name   string
		images []Image
		valid  bool
	}{
		{"none", nil, true},
		{"png", []Image{png}, true},
		{"jpeg", []Image{{MIMEType: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff, 0xe0}}}, true},
		{"gif", []Image{{MIMEType: "image/gif", Data: []byte("GIF89a")}}, true},
		{"webp", []Image{{MIMEType: "image/webp", Data: []byte("RIFF1234WEBPVP8 ")}}, true},
		{"empty", []Image{{MIMEType: "image/png"}}, false},
		{"svg", []Image{{MIMEType: "image/svg+xml", Data: []byte("<svg/>")}}, false},
		{"mime parameters", []Image{{MIMEType: "image/png;foo=bar", Data: png.Data}}, false},
		{"mislabeled", []Image{{MIMEType: "image/png", Data: []byte("not an image")}}, false},
		{"too large", []Image{{MIMEType: "image/png", Data: make([]byte, MaxImageBytes+1)}}, false},
		{"too many", []Image{png, png}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateImages(tc.images); (err == nil) != tc.valid {
				t.Fatalf("image validation = %v, valid=%v", err, tc.valid)
			}
		})
	}
	c := imageTestCommand()
	c.Arguments.Images = []Image{png}
	if err := c.Validate(); err != nil {
		t.Fatalf("image-only turn: %v", err)
	}
	c.Operation, c.ExpectedTurnID = Steer, "turn"
	if err := c.Validate(); err != nil {
		t.Fatalf("image-only steer: %v", err)
	}
	c.Operation = Interrupt
	if err := c.Validate(); err == nil {
		t.Fatal("image silently accepted on an operation that cannot consume it")
	}
}

func TestMaximumImageFitsCompleteFrameAndRoundTrips(t *testing.T) {
	c := imageTestCommand()
	data := make([]byte, MaxImageBytes)
	copy(data, "\x89PNG\r\n\x1a\n")
	c.Arguments = Arguments{Text: strings.Repeat("caption ", 1024), Images: []Image{{MIMEType: "image/png", Data: data}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	envelope, err := NewEnvelope("command", c)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(envelope)
	if err != nil || len(frame) > MaxFrameBytes {
		t.Fatalf("maximum image frame size=%d: %v", len(frame), err)
	}
	decoded, err := Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	command, err := Payload[Command](decoded)
	if err != nil || len(command.Arguments.Images) != 1 || !bytes.Equal(command.Arguments.Images[0].Data, data) || command.Arguments.Text != c.Arguments.Text {
		t.Fatalf("image command did not round trip: %v", err)
	}
}

func TestEnvelopeSizeIncludesHeader(t *testing.T) {
	// The JSON object fits by itself, but the complete transport frame does not.
	payload := json.RawMessage(`{"value":"` + strings.Repeat("a", MaxFrameBytes-40) + `"}`)
	if len(payload) >= MaxFrameBytes {
		t.Fatal("test payload itself is too large")
	}
	if _, err := NewEnvelope("command", payload); err == nil {
		t.Fatal("accepted a payload whose envelope exceeds the frame limit")
	}
}
