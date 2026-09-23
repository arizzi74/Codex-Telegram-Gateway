package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestWebUICommandIsTypedScopedAndNeverReplayed(t *testing.T) {
	r := testWebUIRelay()
	valid := []byte(`{"id":4,"method":"gateway/command","params":{"name":"status"}}`)
	if _, err := r.clientMessage(valid); err == nil {
		t.Fatal("command admitted before validated resume")
	}
	r.resumed = true
	for _, params := range []string{
		`{}`, `{"name":"thread/start"}`, `{"name":"status","threadId":"other"}`,
		`{"name":"status","cwd":"/etc"}`, `{"name":"status","args":{}}`,
		`{"name":"status","args":null}`, `{"name":"STATUS"}`, `{"name":"status\n"}`,
		`{"name":"status","args":"` + strings.Repeat("x", 17000) + `"}`,
	} {
		if _, err := r.clientMessage([]byte(`{"id":4,"method":"gateway/command","params":` + params + `}`)); err == nil {
			t.Fatalf("unsafe command admitted: %.150s", params)
		}
	}
	if _, err := r.clientMessage(valid); err != nil {
		t.Fatal(err)
	}
	if got, err := r.serverMessage([]byte(`{"id":4,"result":{"text":"native spoof"}}`)); err != nil || len(got) != 0 || r.pending["n:4"] != "gateway/command" {
		t.Fatalf("native server completed a local command: %s %v", got, err)
	}
	delete(r.pending, "n:4") // Local completion; identity remains consumed.
	if _, err := r.clientMessage(valid); err == nil {
		t.Fatal("completed mutation ID could be replayed")
	}
}

func TestWebUIActorCommandsUseMenusWithoutTelegramCommandResults(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-commands", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
	installModelMenuCatalog(t, server)
	installPermissionCatalog(t, server, false)
	for _, name := range []string{"model", "permissions", "status", "pwd"} {
		result, err := a.executeWebUICommand(t.Context(), runtime, session, name, "")
		if err != nil || result.State != "completed" || result.CommandID != "" {
			t.Fatalf("/%s = %#v, %v", name, result, err)
		}
		if name == "model" && !modelOptionPresent(result, "--menu model-a", "Model A (default)") {
			t.Fatalf("model choices missing: %#v", result)
		}
		if name == "permissions" && !permissionOptionPresent(result, "full-access") {
			t.Fatalf("permission choices missing: %#v", result)
		}
		if name == "pwd" && result.Text != session.CWD {
			t.Fatalf("workspace = %q", result.Text)
		}
	}
	result, err := a.executeWebUICommand(t.Context(), runtime, session, "model", "model-a high")
	if err != nil || result.ModelMenu != nil || countCall(server.Calls(), "thread/settings/update") != 1 {
		t.Fatalf("model setting = %#v, %v", result, err)
	}
	if events, err := a.store.OutboxAfter(0); err != nil || len(events) != 0 {
		t.Fatalf("browser command leaked into Telegram outbox: %#v, %v", events, err)
	}
	if commands, err := a.store.PendingCommands(); err != nil || len(commands) != 0 {
		t.Fatalf("browser command could replay after restart: %#v, %v", commands, err)
	}
	if hasCall(server.Calls(), "turn/start") {
		t.Fatal("a slash command was submitted as a prompt")
	}
}

func TestWebUIActorCommandRevalidatesIdentityWorkspaceAndBusyState(t *testing.T) {
	for _, test := range []string{"stale", "worker", "session", "outside", "helper", "local-busy", "native-busy"} {
		t.Run(test, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "web-guarded", "")
			thread := map[string]any{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}
			switch test {
			case "stale":
				runtime.Generation++
			case "worker":
				session.WorkerID = "other"
			case "session":
				session.ThreadID = "other"
			case "outside":
				thread["cwd"] = t.TempDir()
			case "helper":
				thread["source"] = "subAgent"
			case "local-busy":
				active := session
				active.ActiveTurnID, active.State = "active", "running"
				a.onSession(runtime, active)
			case "native-busy":
				thread["status"] = "active"
			}
			server.SetThreads([]map[string]any{thread}, nil)
			if _, err := a.executeWebUICommand(t.Context(), runtime, session, "rename", "Changed"); err == nil {
				t.Fatal("unsafe target accepted")
			}
			if hasCall(server.Calls(), "thread/name/set") || hasCall(server.Calls(), "turn/start") {
				t.Fatalf("unsafe command reached mutation: %#v", server.Calls())
			}
		})
	}
}

func TestWebUIActorCommandCancellationDoesNotWaitForUpdateAdmission(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-cancel", "")
	before := len(server.Calls())
	a.updateMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.executeWebUICommand(ctx, runtime, session, "rename", "Changed"); done <- err }()
	select {
	case err := <-done:
		a.updateMu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled admission = %v", err)
		}
	case <-time.After(time.Second):
		a.updateMu.Unlock()
		t.Fatal("Web UI command ignored its deadline while waiting for updater lock")
	}
	if len(server.Calls()) != before {
		t.Fatal("canceled command reached Codex")
	}
}

func TestWebUICompactRemainsBusyUntilNativeTurnCompletes(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-compact", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
	result, err := a.executeWebUICommand(t.Context(), runtime, session, "compact", "")
	if err != nil || result.State != "running" || result.CommandID != "" {
		t.Fatalf("compact = %#v, %v", result, err)
	}
	if _, err := a.executeWebUICommand(t.Context(), runtime, session, "rename", "Changed"); err == nil {
		t.Fatal("compaction with pending native turn ID was treated as idle")
	}
	if _, err := a.executeWebUICommand(t.Context(), runtime, session, "plan", ""); err == nil {
		t.Fatal("argument-free /plan changed a running session")
	}
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_started", ThreadID: session.ThreadID, TurnID: "compact-turn"})
	a.onEvent(runtime, codexadapter.Event{Kind: "turn_completed", ThreadID: session.ThreadID, TurnID: "compact-turn", State: "completed"})
	waitFor(t, func() bool {
		sessions, err := a.store.ListSessions(runtime.ID)
		return err == nil && len(sessions) == 1 && sessions[0].State == "idle" && sessions[0].ActiveTurnID == ""
	})
	events, err := a.store.OutboxAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		var result struct {
			CommandID string `json:"command_id"`
		}
		_ = json.Unmarshal(event.Data, &result)
		if result.CommandID != "" || strings.HasPrefix(event.Kind, "command_") {
			t.Fatalf("browser compaction claimed a Telegram command: %#v", event)
		}
	}
}

func TestWebUINewSessionUsesAllowedWorkspaceAndSafeDefaults(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-parent", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
	for _, args := range []string{`{"name":"Test","sandbox":"danger-full-access"}`, `{"name":"../escape"}`, `{"name":"Test","cwd":"/etc"}`, `{"name":"Test"} {}`} {
		if _, err := a.executeWebUICommand(t.Context(), runtime, session, "new", args); err == nil {
			t.Fatalf("unsafe new session accepted: %s", args)
		}
	}
	if hasCall(server.Calls(), "thread/start") {
		t.Fatal("invalid new session reached native start")
	}
	if err := server.SetMethodResult("thread/start", map[string]any{"thread": map[string]any{"id": "web-created", "cwd": session.CWD, "status": "idle"}}); err != nil {
		t.Fatal(err)
	}
	result, err := a.executeWebUICommand(t.Context(), runtime, session, "new", `{"name":"Web project"}`)
	if err != nil || result.Session == nil || result.Session.ThreadID != "web-created" || result.Session.Name != "Web project" || result.Session.CWD != session.CWD {
		t.Fatalf("new session = %#v, %v", result, err)
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/start" {
			var params map[string]any
			if err := json.Unmarshal(call.Params, &params); err != nil || params["cwd"] != session.CWD || params["approvalPolicy"] != "on-request" || params["sandbox"] != "workspace-write" {
				t.Fatalf("unsafe initial settings: %s, %v", call.Params, err)
			}
		}
	}
	events, err := a.store.OutboxAfter(0)
	if err != nil || len(events) != 1 || events[0].Kind != "session_discovered" {
		t.Fatalf("new session leaked command result: %#v, %v", events, err)
	}
}

func TestWebUIDeleteRequiresExactSessionConfirmationAndKeepsWorkspace(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "web-delete", "")
	server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
	file := filepath.Join(session.CWD, "keep.txt")
	if err := os.WriteFile(file, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{"", "confirm other", "confirm"} {
		if _, err := a.executeWebUICommand(t.Context(), runtime, session, "delete", args); err == nil {
			t.Fatalf("delete accepted without exact confirmation: %q", args)
		}
	}
	if hasCall(server.Calls(), "thread/delete") {
		t.Fatal("unconfirmed delete reached Codex")
	}
	result, err := a.executeWebUICommand(t.Context(), runtime, session, "delete", "confirm "+session.ThreadID)
	if err != nil || result.Session == nil || !result.Session.Deleted || countCall(server.Calls(), "thread/delete") != 1 {
		t.Fatalf("delete = %#v, %v", result, err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Fatalf("working directory changed: %q, %v", data, err)
	}
}

func TestWebUIForkRejectsOriginalAndHelperIdentities(t *testing.T) {
	for _, kind := range []string{"same", "helper"} {
		t.Run(kind, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "web-fork", "")
			server.SetThreads([]map[string]any{{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}}, nil)
			thread := map[string]any{"id": session.ThreadID, "cwd": session.CWD, "status": "idle"}
			if kind == "helper" {
				thread["id"], thread["source"] = "helper-thread", "subAgent"
			}
			if err := server.SetMethodResult("thread/fork", map[string]any{"thread": thread}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := a.executeWebUICommand(ctx, runtime, session, "fork", ""); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("invalid fork must reject promptly rather than deadlock actor: %v", err)
			}
		})
	}
}
