package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestCodexStatusReadsSnapshotAndNeverStartsPrompt(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	server.SetThreads([]map[string]any{{
		"id": "thread-status", "sessionId": "thread-status", "cwd": runtime.DefaultCWD,
		"model": "gpt-5.4", "reasoningEffort": "high", "status": "idle",
	}}, nil)
	session := installSession(a, runtime, "thread-status", "")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	if actor == nil {
		t.Fatal("session actor was not installed")
	}
	client, _, ok := a.manager.Client(runtime.ID)
	if !ok {
		t.Fatal("runtime client missing")
	}
	result, err := actor.executeCodexCommand(context.Background(), client, "status", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "Model: gpt-5.4") || !strings.Contains(result.Text, "Reasoning: high") || !strings.Contains(result.Text, "Token usage:") {
		t.Fatalf("status output:\n%s", result.Text)
	}
	if strings.Contains(result.Text, "read-only thread") || !strings.Contains(result.Text, "default not reported by Codex") {
		t.Fatalf("status confused metadata inspection with session permissions:\n%s", result.Text)
	}
	if hasCall(server.Calls(), "turn/start") {
		t.Fatal("/status was forwarded as a model prompt")
	}
	for _, method := range []string{"thread/read", "config/read", "account/usage/read", "account/rateLimits/read"} {
		if !hasCall(server.Calls(), method) {
			t.Errorf("status omitted %s RPC", method)
		}
	}
}

func TestCodexCommandValidationStopsBeforeRPC(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-validation", "")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	before := len(server.Calls())
	_, err := actor.executeCodexCommand(context.Background(), client, "rename", "")
	var validation *CodexCommandValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %T %v, want CodexCommandValidationError", err, err)
	}
	if after := len(server.Calls()); after != before {
		t.Fatalf("invalid command sent RPC: before=%d after=%d", before, after)
	}
}

func TestThreadReasoningEffortUsesReadSnapshot(t *testing.T) {
	thread := codexadapter.Thread{Raw: []byte(`{"reasoningEffort":"xhigh"}`)}
	if got := threadReasoningEffort(thread); got != "xhigh" {
		t.Fatalf("reasoning effort = %q", got)
	}
}

func TestColdPSDoesNotResumeThread(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installColdSession(a, runtime, "thread-cold-ps")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(context.Background(), client, "ps", "")
	if err != nil || !strings.Contains(result.Text, "not loaded") {
		t.Fatalf("cold ps = %#v, %v", result, err)
	}
	if hasCall(server.Calls(), "thread/resume") || hasCall(server.Calls(), "thread/backgroundTerminals/list") || hasCall(server.Calls(), "turn/start") {
		t.Fatalf("cold ps changed thread state; calls=%#v", server.Calls())
	}
}
