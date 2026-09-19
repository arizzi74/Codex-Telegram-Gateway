package protocol

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionDirectoryName(t *testing.T) {
	for _, test := range []struct{ input, display, directory string }{
		{"  My session  ", "My session", "My_session"},
		{"Two  spaces", "Two  spaces", "Two__spaces"},
		{"日本語 session", "日本語 session", "日本語_session"},
		{"a\u00a0b", "a\u00a0b", "a_b"},
	} {
		display, err := NormalizeSessionName(test.input)
		if err != nil || display != test.display {
			t.Fatalf("normalize %q = %q, %v", test.input, display, err)
		}
		directory, err := SessionDirectoryName(test.input)
		if err != nil || directory != test.directory {
			t.Fatalf("directory %q = %q, %v", test.input, directory, err)
		}
	}
	for _, bad := range []string{"", "   ", ".", "..", "../outside", "a/b", `a\b`, "a\x00b", "a\nb", "a\tb", "\xff", strings.Repeat("x", MaxSessionNameBytes+1)} {
		if _, err := SessionDirectoryName(bad); err == nil {
			t.Errorf("accepted invalid name %q", bad)
		}
	}
}

func TestWorkspaceOperationValidation(t *testing.T) {
	now := time.Now()
	base := Command{ID: uuid.NewString(), WorkerID: uuid.NewString(), RuntimeID: uuid.NewString(), RuntimeGeneration: 1, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	for _, op := range []Operation{BrowseWorkspace, NewSession, DeleteSession} {
		command := base
		command.Operation = op
		switch op {
		case BrowseWorkspace:
			command.Arguments.Workspace = &WorkspaceRequest{}
		case NewSession:
			command.Arguments = Arguments{SessionName: "A session", CreateDirectory: true, CWD: "/workspace"}
		case DeleteSession:
			command.SessionID, command.ThreadID = uuid.NewString(), "thread"
		}
		if err := command.Validate(); err != nil {
			t.Errorf("%s: %v", op, err)
		}
		command.Arguments.SessionName = "../unsafe"
		if err := command.Validate(); err == nil {
			t.Errorf("%s accepted inappropriate session name", op)
		}
	}
	base.Operation = BrowseWorkspace
	for _, request := range []*WorkspaceRequest{nil, {Offset: -1}, {Offset: 1_000_001}, {Path: "bad\x00path"}, {Path: strings.Repeat("a", 4097)}} {
		base.Arguments.Workspace = request
		if err := base.Validate(); err == nil {
			t.Errorf("accepted invalid workspace request %#v", request)
		}
	}
	base.Operation, base.Arguments = NewSession, Arguments{CreateDirectory: true}
	if err := base.Validate(); err == nil {
		t.Error("directory creation accepted without name")
	}
}
