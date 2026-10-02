package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestEnsureSessionWorkspaceUsesExactHomePathAndPreservesExistingFiles(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	path, err := ensureSessionWorkspace(home, "~/CODEX/custom/project", []string{home})
	want := filepath.Join(home, "CODEX", "custom", "project")
	if err != nil || path != want {
		t.Fatalf("new exact path: %q %v", path, err)
	}
	marker := filepath.Join(want, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := ensureSessionWorkspace(home, want, []string{home}); err != nil || again != want {
		t.Fatalf("existing exact path: %q %v", again, err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "keep" {
		t.Fatalf("existing content changed: %q %v", content, err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "file"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, "~/../outside", "~someone/project", "relative/project", filepath.Join(home, "escape", "no-project"), filepath.Join(home, "file", "no-project")} {
		if _, err := ensureSessionWorkspace(home, path, []string{home}); err == nil {
			t.Errorf("accepted invalid destination %q", path)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "no-project")); !os.IsNotExist(err) {
		t.Fatalf("created outside allowed root: %v", err)
	}
}

func TestWorkspaceBrowserExpandsHomeOnWorker(t *testing.T) {
	home := t.TempDir()
	page, err := browseWorkspace(home, []string{home}, &protocol.WorkspaceRequest{Path: "~"})
	if err != nil || page.Path != home {
		t.Fatalf("home browse: %+v %v", page, err)
	}
	if _, err := browseWorkspace(home, []string{home}, &protocol.WorkspaceRequest{Path: "~other"}); err == nil {
		t.Fatal("another user's home accepted")
	}
}

func TestAgentWebUICreatesFirstSessionAtExactPathWithLegacyHistory(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	t.Setenv("HOME", runtime.DefaultCWD)
	if len(a.sessions) != 0 {
		t.Fatal("test worker was not fresh")
	}
	destination := "~/CODEX/custom_project"
	want := filepath.Join(runtime.DefaultCWD, "CODEX", "custom_project")
	if err := server.SetMethodResult("thread/start", map[string]any{"thread": map[string]any{"id": "fresh-created", "cwd": want, "source": "cli"}}); err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	command.Arguments = protocol.Arguments{SessionName: "Different display name", CWD: destination, EnsureWorkspace: true, HistoryMode: "legacy"}
	if ack, err := a.HandleCommand(t.Context(), command); err != nil || ack.Status != "accepted" {
		t.Fatalf("fresh worker admission: %+v %v", ack, err)
	}
	var record CommandRecord
	waitFor(t, func() bool {
		var found bool
		var err error
		record, found, err = a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	if record.Result == nil || record.Result.Session == nil || record.Result.Session.CWD != want || record.Result.Session.Name != "Different display name" {
		t.Fatalf("first created session: %+v", record.Result)
	}
	for _, call := range server.Calls() {
		if call.Method != "thread/start" {
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params["cwd"] != want || params["historyMode"] != "legacy" || params["approvalPolicy"] != "on-request" || params["sandbox"] != "workspace-write" {
			t.Fatalf("unsafe or nonattachable thread start: %+v", params)
		}
	}
	if ack, err := a.HandleCommand(t.Context(), command); err != nil || ack.Status != "duplicate" {
		t.Fatalf("duplicate admission: %+v %v", ack, err)
	}
	if countCall(server.Calls(), "thread/start") != 1 {
		t.Fatal("first-session recovery started another thread")
	}
	a.mu.RLock()
	actor := a.sessions[record.Result.Session.ID]
	a.mu.RUnlock()
	if actor == nil {
		t.Fatal("first session could not be attached without pre-existing actor")
	}
}

func TestAgentWebUICreatesSessionInExistingDirectoryWithoutAddingName(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	marker := filepath.Join(runtime.DefaultCWD, "keep")
	if err := server.SetMethodResult("thread/start", map[string]any{"thread": map[string]any{"id": "existing-created", "cwd": runtime.DefaultCWD, "source": "cli"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	command.Arguments = protocol.Arguments{SessionName: "Display name", CWD: runtime.DefaultCWD, EnsureWorkspace: true, HistoryMode: "legacy"}
	if _, err := a.HandleCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserved" {
		t.Fatalf("existing files: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(runtime.DefaultCWD, "Display_name")); !os.IsNotExist(err) {
		t.Fatalf("appended display name to exact path: %v", err)
	}
	if countCall(server.Calls(), "thread/start") != 1 {
		t.Fatal("existing path did not start one thread")
	}
}

func TestAgentWebUIPostStartFailureKeepsCreationOutcomeUnknown(t *testing.T) {
	for _, mode := range []string{"rename rejected", "wrong cwd", "subagent"} {
		t.Run(mode, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			thread := map[string]any{"id": "already-started", "cwd": runtime.DefaultCWD, "source": "cli"}
			if mode == "wrong cwd" {
				thread["cwd"] = t.TempDir()
			}
			if mode == "subagent" {
				thread["parentThreadId"] = "parent"
			}
			if err := server.SetMethodResult("thread/start", map[string]any{"thread": thread}); err != nil {
				t.Fatal(err)
			}
			if mode == "rename rejected" {
				server.SetRPCError("thread/name/set", -32000, "private rename failure")
			}
			command := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
			command.Arguments = protocol.Arguments{SessionName: "Project", CWD: runtime.DefaultCWD, EnsureWorkspace: true, HistoryMode: "legacy"}
			if _, err := a.HandleCommand(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			var record CommandRecord
			waitFor(t, func() bool {
				var found bool
				var err error
				record, found, err = a.store.LoadCommand(command.ID)
				return err == nil && found && record.State == CommandOutcomeUnknown
			})
			if record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != protocol.OutcomeUnknown || record.Result.Session != nil {
				t.Fatalf("post-start failure incorrectly claimed safe retry: %+v", record)
			}
			if ack, err := a.HandleCommand(t.Context(), command); err != nil || ack.Status != "duplicate" {
				t.Fatalf("unknown recovery: %+v %v", ack, err)
			}
			if countCall(server.Calls(), "thread/start") != 1 {
				t.Fatal("unknown creation replayed")
			}
			a.mu.RLock()
			actors := len(a.sessions)
			a.mu.RUnlock()
			if actors != 0 {
				t.Fatal("unexpected or incompletely named thread became a visible session")
			}
		})
	}
}

func TestAgentWebUIRejectsUnsafeDestinationBeforeCallingCodex(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	outside := t.TempDir()
	command := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	command.Arguments = protocol.Arguments{SessionName: "Project", CWD: filepath.Join(outside, "no-project"), EnsureWorkspace: true, HistoryMode: "legacy"}
	if _, err := a.HandleCommand(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandFailed
	})
	if countCall(server.Calls(), "thread/start") != 0 {
		t.Fatal("outside-root path reached Codex")
	}
	if _, err := os.Stat(filepath.Join(outside, "no-project")); !os.IsNotExist(err) {
		t.Fatalf("outside-root path created: %v", err)
	}
}
