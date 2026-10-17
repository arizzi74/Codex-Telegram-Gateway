package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFileInputValidationAndOperations(t *testing.T) {
	for _, test := range []struct {
		name string
		file File
		bad  bool
	}{
		{"document", File{Name: "report.pdf", Data: []byte("pdf")}, false},
		{"empty document", File{Name: "empty.txt"}, false},
		{"unicode", File{Name: "résumé 例.txt", Data: []byte("text")}, false},
		{"missing name", File{Data: []byte("text")}, true},
		{"parent path", File{Name: "../report.pdf"}, true},
		{"windows path", File{Name: `a\report.pdf`}, true},
		{"absolute path", File{Name: "/report.pdf"}, true},
		{"control", File{Name: "report\n.pdf"}, true},
		{"nul", File{Name: "report\x00.pdf"}, true},
		{"too long", File{Name: strings.Repeat("a", 1025)}, true},
		{"too large", File{Name: "report.pdf", Data: make([]byte, MaxFileBytes+1)}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateFiles([]File{test.file}); (err != nil) != test.bad {
				t.Fatalf("validation=%v bad=%v", err, test.bad)
			}
		})
	}
	if ValidateFiles([]File{{Name: "a"}, {Name: "b"}}) == nil {
		t.Fatal("multiple files accepted")
	}
	for _, op := range []Operation{StartTurn, Steer, StoreFiles, Interrupt, NewSession, CodexCommand} {
		c := imageTestCommand()
		c.Operation, c.ExpectedTurnID = op, "turn"
		c.Arguments = Arguments{CWD: "/workspace", Files: []File{{Name: "report.pdf", Data: []byte("pdf")}}}
		valid := op == StartTurn || op == Steer || op == StoreFiles
		if err := c.Validate(); (err == nil) != valid {
			t.Fatalf("operation=%s validation=%v valid=%v", op, err, valid)
		}
	}
	c := imageTestCommand()
	c.Operation = StoreFiles
	c.Arguments = Arguments{CWD: "/workspace", Text: "caption", Files: []File{{Name: "report.pdf"}}}
	if c.Validate() == nil {
		t.Fatal("file saving accepted a caption without forwarding")
	}
	c.Arguments.Text = ""
	c.Arguments.CWD = ""
	if c.Validate() == nil {
		t.Fatal("file input accepted a mutable workspace")
	}
}

func TestMaximumFileFitsCompleteFrameAndRoundTrips(t *testing.T) {
	c := imageTestCommand()
	c.Arguments = Arguments{CWD: "/workspace", Text: strings.Repeat("caption ", 1024), Files: []File{{Name: "archive.zip", Data: bytes.Repeat([]byte{0xff}, MaxFileBytes)}}}
	envelope, err := NewEnvelope("command", c)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(envelope)
	if err != nil || len(frame) > MaxFrameBytes {
		t.Fatalf("frame=%d error=%v", len(frame), err)
	}
	decoded, err := Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	command, err := Payload[Command](decoded)
	if err != nil || len(command.Arguments.Files) != 1 || !bytes.Equal(command.Arguments.Files[0].Data, c.Arguments.Files[0].Data) {
		t.Fatalf("file did not round trip: %v", err)
	}
}
