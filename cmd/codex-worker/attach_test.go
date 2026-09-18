package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/worker"
)

func TestAttachmentLatestDoesNotDependOnDiscoveredSessions(t *testing.T) {
	status := worker.Status{Runtimes: []protocol.Runtime{{ID: "runtime", LocalSocket: "/private/worker.sock"}}}
	got, err := attachmentArgs(status, []string{"--latest"})
	want := []string{"attach", "--socket", "/private/worker.sock", "--latest"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("latest attachment = %v, %v", got, err)
	}
	// A cold session may not be discovered yet. Let the connected Codex server
	// resolve history rather than using the status file as the conversation list.
	got, err = attachmentArgs(status, nil)
	if err != nil || !reflect.DeepEqual(got, want[:3]) {
		t.Fatalf("interactive attachment = %v, %v", got, err)
	}
}

func TestAttachmentExplicitSessionSelectsItsRuntime(t *testing.T) {
	status := worker.Status{
		Runtimes: []protocol.Runtime{{ID: "first", LocalSocket: "/private/first.sock"}, {ID: "second", LocalSocket: "/private/second.sock"}},
		Sessions: []protocol.Session{{ID: "session", ThreadID: "thread", RuntimeID: "second", Name: "my session"}},
	}
	for _, selector := range []string{"session", "thread", "my session"} {
		got, err := attachmentArgs(status, []string{selector})
		if err != nil || !reflect.DeepEqual(got, []string{"attach", "--socket", "/private/second.sock", "thread"}) {
			t.Fatalf("attachment for %q = %v, %v", selector, got, err)
		}
	}
	for _, args := range [][]string{nil, {"--latest"}} {
		if _, err := attachmentArgs(status, args); err == nil || !strings.Contains(err.Error(), "multiple runtimes") {
			t.Fatalf("ambiguous endpoint for %v was accepted: %v", args, err)
		}
	}
}

func TestAttachmentRejectsInvalidSelections(t *testing.T) {
	status := worker.Status{
		Runtimes: []protocol.Runtime{{ID: "runtime", LocalSocket: "/private/worker.sock"}},
		Sessions: []protocol.Session{{Name: "duplicate", ThreadID: "one", RuntimeID: "runtime"}, {Name: "duplicate", ThreadID: "two", RuntimeID: "runtime"}},
	}
	for _, args := range [][]string{{"--latest", "one"}, {"one", "--latest"}, {"--latest", "--latest"}, {"--unknown"}, {"missing"}, {"duplicate"}} {
		if _, err := attachmentArgs(status, args); err == nil {
			t.Fatalf("invalid selection %v accepted", args)
		}
	}
	if _, err := attachmentArgs(worker.Status{}, []string{"--latest"}); err == nil {
		t.Fatal("latest attachment without a managed endpoint accepted")
	}
}
