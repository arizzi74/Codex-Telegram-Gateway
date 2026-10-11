package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWebUIFileTargetAndRequests(t *testing.T) {
	frame := WebUIFrame{ID: uuid.NewString(), Action: "file_open", RuntimeID: uuid.NewString(), RuntimeGeneration: 4, SessionID: uuid.NewString(), Data: json.RawMessage(`{"path":"/workspace/report.pdf"}`)}
	if err := frame.Validate(); err != nil {
		t.Fatal(err)
	}
	frame.RuntimeGeneration = 0
	if err := frame.Validate(); err == nil {
		t.Fatal("unfenced file target accepted")
	}
	frame.RuntimeGeneration = 4
	for _, raw := range []string{`{}`, `null`, `{"path":null}`, `{"path":""}`, `{"path":"ok","extra":true}`, `{"path":"ok"} {}`, `{"path":"bad\nname"}`} {
		frame.Data = json.RawMessage(raw)
		if err := frame.Validate(); err == nil {
			t.Errorf("invalid file request accepted: %s", raw)
		}
	}
}

func TestNormalizeWebUIFilePath(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"/workspace/report.pdf", "/workspace/report.pdf"},
		{"./report.pdf", "./report.pdf"},
		{"~/report.pdf", "~/report.pdf"},
		{"/workspace/code.go:12:3", "/workspace/code.go"},
		{"/workspace/code.go#L12-L14", "/workspace/code.go"},
		{"file:///workspace/my%20report.pdf#L10", "/workspace/my report.pdf"},
		{"file://localhost/workspace/report.pdf", "/workspace/report.pdf"},
		{"file:///workspace/report%2520name.pdf", "/workspace/report%20name.pdf"},
		{"/workspace/report%20name.pdf", "/workspace/report%20name.pdf"},
	} {
		got, err := NormalizeWebUIFilePath(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("normalize %q = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, path := range []string{"", " ", "bad\x00name", "bad\nname", "bad\x7fname", "bad\\name", string([]byte{0xff}), strings.Repeat("a", MaxWebUIFilePath+1), "file://remote/workspace/report.pdf", "file:///workspace/report.pdf?token=secret", "file:///workspace/report.pdf#secret", "file:///workspace/%00file", "file:relative", "https://example.com/file"} {
		if _, err := NormalizeWebUIFilePath(path); err == nil {
			t.Errorf("unsafe path accepted: %q", path)
		}
	}
}

func TestWebUIFileReadBounds(t *testing.T) {
	for _, raw := range []string{`{"offset":0}`, `{"offset":134217728}`} {
		if _, err := DecodeWebUIFileRead([]byte(raw)); err != nil {
			t.Fatalf("valid read rejected: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"offset":null}`, `{"offset":-1}`, `{"offset":134217729}`, `{"offset":1.5}`, `{"offset":"0"}`, `{"offset":0,"length":99999}`, `{"offset":0} {}`, strings.Repeat(" ", 129) + `{"offset":0}`} {
		if _, err := DecodeWebUIFileRead([]byte(raw)); err == nil {
			t.Errorf("invalid file read accepted: %s", raw)
		}
	}
}

func TestWebUIFilesCapabilityRoundTrip(t *testing.T) {
	for _, payload := range []any{Hello{SupportsWebUIFiles: true}, Heartbeat{SupportsWebUIFiles: true}} {
		encoded, err := json.Marshal(payload)
		if err != nil || !strings.Contains(string(encoded), `"supports_webui_files":true`) {
			t.Fatalf("file capability missing: %s, %v", encoded, err)
		}
	}
}
