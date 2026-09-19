package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWorkspaceBrowserHomePreferenceNavigationAndPaging(t *testing.T) {
	home := t.TempDir()
	roots := []string{home}
	page, err := browseWorkspace(home, roots, &protocol.WorkspaceRequest{})
	if err != nil || page.Path != home || page.Parent != "" {
		t.Fatalf("home page = %#v, %v", page, err)
	}
	codex := filepath.Join(home, "CODEX")
	if err := os.Mkdir(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range protocol.WorkspacePageSize + 2 {
		if err := os.Mkdir(filepath.Join(codex, fmt.Sprintf("folder-%02d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	page, err = browseWorkspace(home, roots, &protocol.WorkspaceRequest{})
	if err != nil || page.Path != codex || page.Parent != home || len(page.Directories) != protocol.WorkspacePageSize || !page.HasMore {
		t.Fatalf("CODEX page = %#v, %v", page, err)
	}
	if page.Directories[0].Name != "folder-00" || page.Directories[0].Path != filepath.Join(codex, "folder-00") {
		t.Fatalf("unexpected first entry %#v", page.Directories[0])
	}
	page, err = browseWorkspace(home, roots, &protocol.WorkspaceRequest{Path: codex, Offset: protocol.WorkspacePageSize})
	if err != nil || len(page.Directories) != 2 || page.HasMore || page.Offset != protocol.WorkspacePageSize {
		t.Fatalf("second page = %#v, %v", page, err)
	}
	page, err = browseWorkspace(home, roots, &protocol.WorkspaceRequest{Path: page.Directories[0].Path})
	if err != nil || page.Parent != codex || len(page.Directories) != 0 {
		t.Fatalf("child page = %#v, %v", page, err)
	}
}

func TestWorkspaceBrowserRejectsEscapesAndUsesRestrictedRoots(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(home, "safe")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(home, "allowed-link")); err != nil {
		t.Fatal(err)
	}
	page, err := browseWorkspace(home, []string{home}, &protocol.WorkspaceRequest{})
	if err != nil || len(page.Directories) != 2 || page.Directories[0].Path != inside {
		t.Fatalf("symlink filtered page = %#v, %v", page, err)
	}
	for _, path := range []string{outside, filepath.Dir(home), filepath.Join(home, "escape"), "../outside"} {
		if _, err := browseWorkspace(home, []string{home}, &protocol.WorkspaceRequest{Path: path}); err == nil {
			t.Errorf("accepted outside path %q", path)
		}
	}
	page, err = browseWorkspace(home, []string{inside}, &protocol.WorkspaceRequest{})
	if err != nil || page.Path != inside || page.Parent != "" {
		t.Fatalf("restricted default = %#v, %v", page, err)
	}
}

func TestCreateSessionWorkspacePreservesExistingPathsAndRejectsEscapes(t *testing.T) {
	parent, outside := t.TempDir(), t.TempDir()
	roots := []string{parent}
	child, err := createSessionWorkspace(parent, "Session name", roots)
	if err != nil || child != filepath.Join(parent, "Session_name") {
		t.Fatalf("create = %q, %v", child, err)
	}
	marker := filepath.Join(child, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := createSessionWorkspace(parent, "Session name", roots); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error = %v", err)
	}
	if content, err := os.ReadFile(marker); err != nil || string(content) != "keep me" {
		t.Fatalf("existing content = %q, %v", content, err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{outside, filepath.Join(parent, "escape")} {
		if _, err := createSessionWorkspace(candidate, "Must not exist", roots); err == nil {
			t.Errorf("accepted escaping parent %q", candidate)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "Must_not_exist")); !os.IsNotExist(err) {
		t.Fatalf("created outside root: %v", err)
	}
}

func TestAgentGuidedSessionCreatesNamedDirectoryAndThreadOnce(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	c := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	c.Arguments = protocol.Arguments{CWD: runtime.DefaultCWD, SessionName: "My guided session", CreateDirectory: true}
	if ack, err := a.HandleCommand(context.Background(), c); err != nil || ack.Status != "accepted" {
		t.Fatalf("ack = %#v, %v", ack, err)
	}
	var record CommandRecord
	waitFor(t, func() bool {
		var found bool
		var err error
		record, found, err = a.store.LoadCommand(c.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	child := filepath.Join(runtime.DefaultCWD, "My_guided_session")
	if record.Result == nil || record.Result.Session == nil || record.Result.Session.Name != "My guided session" || record.Result.Session.CWD != child {
		t.Fatalf("created session = %#v", record.Result)
	}
	if info, err := os.Stat(child); err != nil || !info.IsDir() {
		t.Fatalf("session directory = %v, %v", info, err)
	}
	if ack, err := a.HandleCommand(context.Background(), c); err != nil || ack.Status != "duplicate" {
		t.Fatalf("duplicate = %#v, %v", ack, err)
	}
	for _, call := range server.Calls() {
		if call.Method != "thread/start" && call.Method != "thread/name/set" {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal(call.Params, &args); err != nil {
			t.Fatal(err)
		}
		if call.Method == "thread/start" && args["cwd"] != child {
			t.Fatalf("thread cwd = %#v", args)
		}
		if call.Method == "thread/name/set" && args["name"] != "My guided session" {
			t.Fatalf("thread name = %#v", args)
		}
	}
	if countCall(server.Calls(), "thread/start") != 1 || countCall(server.Calls(), "thread/name/set") != 1 {
		t.Fatalf("unexpected calls = %#v", server.Calls())
	}
}

func TestAgentGuidedSessionCollisionDoesNotCallCodex(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	if err := os.Mkdir(filepath.Join(runtime.DefaultCWD, "Existing"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	c.Arguments = protocol.Arguments{CWD: runtime.DefaultCWD, SessionName: "Existing", CreateDirectory: true}
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(c.ID)
		return err == nil && found && record.State == CommandFailed
	})
	if hasCall(server.Calls(), "thread/start") {
		t.Fatal("directory collision reached Codex")
	}
}

func TestAgentWorkspaceBrowseTargetsRuntimeWithoutCodexRPC(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	c := agentCommand(runtime, protocol.Session{}, protocol.BrowseWorkspace)
	c.Arguments = protocol.Arguments{Workspace: &protocol.WorkspaceRequest{Path: runtime.DefaultCWD}}
	if ack, err := a.HandleCommand(context.Background(), c); err != nil || ack.Status != "accepted" {
		t.Fatalf("browse ack = %#v, %v", ack, err)
	}
	record, found, err := a.store.LoadCommand(c.ID)
	if err != nil || !found || record.Result == nil || record.Result.Workspace == nil || record.Result.Workspace.Path != runtime.DefaultCWD {
		t.Fatalf("browse result = %#v, %v", record, err)
	}
	if countCall(server.Calls(), "thread/start") != 0 || countCall(server.Calls(), "thread/list") != 0 {
		t.Fatalf("browse called Codex: %#v", server.Calls())
	}
}

func TestGuidedCreationCrashDoesNotReplayFilesystemMutation(t *testing.T) {
	parent := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state.db")
	workerID := uuid.NewString()
	store, err := OpenStore(statePath, workerID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := store.BeginRuntime("profile", "Profile", parent)
	if err != nil {
		t.Fatal(err)
	}
	c := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	c.Arguments = protocol.Arguments{CWD: parent, SessionName: "Interrupted creation", CreateDirectory: true}
	if _, err := store.Receive(c); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCommandState(c.ID, CommandExecuting, nil); err != nil {
		t.Fatal(err)
	}
	child, err := createSessionWorkspace(parent, c.Arguments.SessionName, []string{parent})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(statePath, workerID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pending, err := store.PendingCommands()
	if err != nil || len(pending) != 0 {
		t.Fatalf("replayed creation after crash: %#v, %v", pending, err)
	}
	received, err := store.Receive(c)
	if err != nil || received.Accepted || received.Record.State != CommandOutcomeUnknown {
		t.Fatalf("redelivery after crash = %#v, %v", received, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(child) {
		t.Fatalf("created directories changed after restart: %#v, %v", entries, err)
	}
}

func TestAgentLegacyNewSessionUsesExistingWorkingDirectory(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	c := agentCommand(runtime, protocol.Session{}, protocol.NewSession)
	if _, err := a.HandleCommand(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		record, found, err := a.store.LoadCommand(c.ID)
		return err == nil && found && record.State == CommandCompleted && record.Result.Session.CWD == runtime.DefaultCWD
	})
	entries, err := os.ReadDir(runtime.DefaultCWD)
	if err != nil || len(entries) != 0 || hasCall(server.Calls(), "thread/name/set") {
		t.Fatalf("legacy creation changed workspace or name: %#v, %v", entries, err)
	}
}
